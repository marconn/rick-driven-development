package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

func newStatusCmd() *cobra.Command {
	var dbPath string

	cmd := &cobra.Command{
		Use:   "status <workflow-id>",
		Short: "Show workflow status",
		Long:  `Replay events for a workflow and display the current aggregate state.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return showStatus(cmd.Context(), dbPath, args[0])
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "rick.db", "SQLite database path")
	return cmd
}

func showStatus(ctx context.Context, dbPath, aggregateID string) error {
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()

	events, err := store.Load(ctx, aggregateID)
	if err != nil {
		return fmt.Errorf("load events: %w", err)
	}

	if len(events) == 0 {
		_, _ = fmt.Fprintf(os.Stderr, "no events found for aggregate %s\n", aggregateID)
		return nil
	}

	agg := engine.NewWorkflowAggregate(aggregateID)
	for _, env := range events {
		agg.Apply(env)
	}

	// Activity diagnostics replay the full correlation stream (handler-scoped
	// aggregates like AIRequestStarted live outside the workflow aggregate, so
	// store.Load(aggregateID) can't see them). The workflow aggregate itself
	// uses the workflow_id as both aggregate_id and correlation_id, so this
	// load reaches every event the persona runner persisted for this run.
	corrEvents, err := store.LoadByCorrelation(ctx, aggregateID)
	if err != nil {
		return fmt.Errorf("load correlation events: %w", err)
	}
	inFlight := computeInFlightPersonas(corrEvents)
	lastEvt, lastAge := lastActivity(corrEvents, time.Now())

	_, _ = fmt.Fprintf(os.Stdout, "Workflow:    %s\n", aggregateID)
	_, _ = fmt.Fprintf(os.Stdout, "Status:      %s\n", agg.Status)
	_, _ = fmt.Fprintf(os.Stdout, "Version:     %d\n", agg.Version)
	if lastEvt != "" {
		_, _ = fmt.Fprintf(os.Stdout, "Last event:  %s (%s ago)\n", lastEvt, formatAge(lastAge))
	}

	if agg.Prompt != "" {
		_, _ = fmt.Fprintf(os.Stdout, "Prompt:      %s\n", truncate(agg.Prompt, 80))
	}

	if agg.Status == engine.StatusPaused || agg.Status == engine.StatusCancelled {
		_, _ = fmt.Fprintln(os.Stdout)
		if agg.Status == engine.StatusPaused {
			_, _ = fmt.Fprintln(os.Stdout, "Use 'rick resume' or 'rick guide' to continue this workflow.")
		}
	}

	// Suppress the in-flight section for terminal-state workflows. A cancelled
	// workflow that killed a persona mid-call still has a started-without-
	// response pair on the stream, and printing "IN FLIGHT" against a status
	// of "cancelled" is more misleading than informative. The same persona
	// will appear in the COMPLETED=false column below.
	if len(inFlight) > 0 && !isTerminalStatus(agg.Status) {
		_, _ = fmt.Fprintln(os.Stdout)
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "IN FLIGHT\tBACKEND\tSTARTED\tELAPSED")
		_, _ = fmt.Fprintln(w, "---------\t-------\t-------\t-------")
		now := time.Now()
		for _, ifp := range inFlight {
			elapsed := now.Sub(ifp.StartedAt).Truncate(time.Second)
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				ifp.Persona,
				ifp.Backend,
				ifp.StartedAt.Local().Format("15:04:05"),
				elapsed)
		}
		_ = w.Flush()
	}

	if len(agg.CompletedPersonas) > 0 || len(agg.FeedbackCount) > 0 {
		_, _ = fmt.Fprintln(os.Stdout)
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "PERSONA\tCOMPLETED\tFEEDBACK ITERATIONS")
		_, _ = fmt.Fprintln(w, "-------\t---------\t-------------------")

		// Collect all persona names from both maps
		seen := make(map[string]bool)
		for name := range agg.CompletedPersonas {
			seen[name] = true
		}
		for name := range agg.FeedbackCount {
			seen[name] = true
		}
		names := make([]string, 0, len(seen))
		for name := range seen {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			completed := agg.CompletedPersonas[name]
			feedback := agg.FeedbackCount[name]
			_, _ = fmt.Fprintf(w, "%s\t%v\t%d\n", name, completed, feedback)
		}
		_ = w.Flush()
	}

	return nil
}

// inFlightPersona holds the diagnostic fields shown in the IN FLIGHT section.
type inFlightPersona struct {
	Persona   string
	Backend   string
	StartedAt time.Time
}

// computeInFlightPersonas walks the correlation stream and returns one entry
// per persona whose most recent AIRequestStarted has no matching
// AIResponseReceived / PersonaFailed terminator. Order matters: events come
// version-/rowid-ordered from store.LoadByCorrelation, so a later iteration's
// "started" correctly supersedes an earlier "received" for the same persona.
func computeInFlightPersonas(events []event.Envelope) []inFlightPersona {
	type state struct {
		inFlight  bool
		backend   string
		startedAt time.Time
	}
	byPersona := map[string]state{}

	personaFromAI := func(env event.Envelope) (string, event.AIRequestStartedPayload) {
		var p event.AIRequestStartedPayload
		_ = json.Unmarshal(env.Payload, &p)
		return p.Persona, p
	}

	for _, env := range events {
		switch env.Type {
		case event.AIRequestStarted:
			name, p := personaFromAI(env)
			if name == "" {
				continue
			}
			started := time.Unix(0, p.SpawnUnixNano)
			if p.SpawnUnixNano == 0 {
				started = env.Timestamp
			}
			byPersona[name] = state{inFlight: true, backend: p.Backend, startedAt: started}
		case event.AIResponseReceived:
			var p event.AIResponsePayload
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.Persona == "" {
				continue
			}
			if s, ok := byPersona[p.Persona]; ok {
				s.inFlight = false
				byPersona[p.Persona] = s
			}
		case event.PersonaFailed, event.PersonaFailedTracked:
			var p event.PersonaFailedPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil || p.Persona == "" {
				continue
			}
			if s, ok := byPersona[p.Persona]; ok {
				s.inFlight = false
				byPersona[p.Persona] = s
			}
		}
	}

	out := make([]inFlightPersona, 0, len(byPersona))
	for name, s := range byPersona {
		if !s.inFlight {
			continue
		}
		out = append(out, inFlightPersona{Persona: name, Backend: s.backend, StartedAt: s.startedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// lastActivity returns the type and elapsed time of the most recent event in
// the correlation stream. The age is measured from `now` to the event's
// timestamp; callers pass time.Now() and the result is a positive duration
// for normal operation.
func lastActivity(events []event.Envelope, now time.Time) (string, time.Duration) {
	if len(events) == 0 {
		return "", 0
	}
	var newest event.Envelope
	for _, env := range events {
		if env.Timestamp.After(newest.Timestamp) {
			newest = env
		}
	}
	if newest.Timestamp.IsZero() {
		return "", 0
	}
	return string(newest.Type), now.Sub(newest.Timestamp)
}

// isTerminalStatus reports whether the aggregate is past the point of further
// dispatch. Used to suppress the in-flight section, which only makes sense for
// workflows that could still emit events.
func isTerminalStatus(s engine.WorkflowStatus) bool {
	return s == engine.StatusCompleted || s == engine.StatusFailed || s == engine.StatusCancelled
}

// formatAge renders a duration as "1h12m", "8m37s", or "12s" — short enough
// for the status line, long enough to distinguish active from stale.
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

