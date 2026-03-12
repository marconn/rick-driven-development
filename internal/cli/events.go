package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

func newEventsCmd() *cobra.Command {
	var dbPath string
	var correlation bool

	cmd := &cobra.Command{
		Use:   "events <workflow-id>",
		Short: "List events for a workflow",
		Long: `Display events for a workflow.

By default, shows only the workflow aggregate events (lifecycle).
Use --correlation to show ALL events across all personas in the workflow,
including AI requests/responses, verdicts, enrichments, and guidance.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return listEvents(cmd.Context(), dbPath, args[0], correlation)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "rick.db", "SQLite database path")
	cmd.Flags().BoolVarP(&correlation, "correlation", "c", false, "Show all correlated events (across all personas)")
	return cmd
}

func listEvents(ctx context.Context, dbPath, id string, correlation bool) error {
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()

	var events []event.Envelope

	if correlation {
		events, err = store.LoadByCorrelation(ctx, id)
	} else {
		events, err = store.Load(ctx, id)
	}
	if err != nil {
		return fmt.Errorf("load events: %w", err)
	}

	if len(events) == 0 {
		_, _ = fmt.Fprintf(os.Stderr, "no events found for %s\n", id)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if correlation {
		_, _ = fmt.Fprintln(w, "TYPE\tTIMESTAMP\tAGGREGATE\tSOURCE\tSUMMARY")
		_, _ = fmt.Fprintln(w, "----\t---------\t---------\t------\t-------")
		for _, env := range events {
			ts := env.Timestamp.Format(time.TimeOnly)
			agg := env.AggregateID
			if len(agg) > 35 {
				agg = "…" + agg[len(agg)-34:]
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				env.Type, ts, agg, env.Source, eventSummary(env))
		}
	} else {
		_, _ = fmt.Fprintln(w, "VERSION\tTYPE\tTIMESTAMP\tSOURCE\tSUMMARY")
		_, _ = fmt.Fprintln(w, "-------\t----\t---------\t------\t-------")
		for _, env := range events {
			ts := env.Timestamp.Format(time.TimeOnly)
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
				env.Version, env.Type, ts, env.Source, eventSummary(env))
		}
	}

	_, _ = fmt.Fprintf(os.Stderr, "\n%d events\n", len(events))
	return w.Flush()
}

func eventSummary(env event.Envelope) string {
	switch env.Type {
	case event.WorkflowRequested:
		var p event.WorkflowRequestedPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return truncate(p.Prompt, 60)
		}
	case event.WorkflowCompleted:
		var p event.WorkflowCompletedPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return p.Result
		}
	case event.WorkflowFailed:
		var p event.WorkflowFailedPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return fmt.Sprintf("phase=%s: %s", p.Phase, truncate(p.Reason, 50))
		}
	case event.WorkflowPaused:
		var p event.WorkflowPausedPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return truncate(p.Reason, 60)
		}
	case event.WorkflowResumed:
		var p event.WorkflowResumedPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return p.Reason
		}
	case event.PersonaCompleted:
		var p event.PersonaCompletedPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return fmt.Sprintf("%s (chain=%d, %dms)", p.Persona, p.ChainDepth, p.DurationMS)
		}
	case event.PersonaFailed:
		var p event.PersonaFailedPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return fmt.Sprintf("%s: %s (%dms)", p.Persona, truncate(p.Error, 40), p.DurationMS)
		}
	case event.AIRequestSent:
		var p event.AIRequestPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return fmt.Sprintf("%s via %s", p.Phase, p.Backend)
		}
	case event.AIResponseReceived:
		var p event.AIResponsePayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return fmt.Sprintf("%s %dms tokens=%d", p.Phase, p.DurationMS, p.TokensUsed)
		}
	case event.VerdictRendered:
		var p event.VerdictPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return fmt.Sprintf("%s: %s (%d issues)", p.Phase, p.Outcome, len(p.Issues))
		}
	case event.FeedbackGenerated:
		var p event.FeedbackGeneratedPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return fmt.Sprintf("→%s iteration=%d", p.TargetPhase, p.Iteration)
		}
	case event.OperatorGuidance:
		var p event.OperatorGuidancePayload
		if json.Unmarshal(env.Payload, &p) == nil {
			target := p.Target
			if target == "" {
				target = "all"
			}
			return fmt.Sprintf("→%s: %s", target, truncate(p.Content, 50))
		}
	case event.ContextEnrichment:
		var p event.ContextEnrichmentPayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return fmt.Sprintf("%s: %d items (%s)", p.Source, len(p.Items), p.Kind)
		}
	case event.ContextCodebase:
		var p event.ContextCodebasePayload
		if json.Unmarshal(env.Payload, &p) == nil {
			return fmt.Sprintf("%s %s (%d files)", p.Language, p.Framework, len(p.Tree))
		}
	default:
		if event.IsWorkflowStarted(env.Type) {
			var p event.WorkflowStartedPayload
			if json.Unmarshal(env.Payload, &p) == nil {
				return fmt.Sprintf("phases: %v", p.Phases)
			}
		}
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
