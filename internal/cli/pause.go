package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

func newPauseCmd() *cobra.Command {
	var dbPath, reason, serverURL string

	cmd := &cobra.Command{
		Use:   "pause <workflow-id>",
		Short: "Pause a running workflow",
		Long: `Pause a running workflow. In-flight personas complete but new dispatches
are blocked until resumed. Use 'rick resume' or 'rick guide' to continue.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPause(cmd.Context(), serverURL, dbPath, args[0], reason)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "rick.db", "SQLite database path (offline fallback)")
	cmd.Flags().StringVar(&reason, "reason", "operator requested", "Pause reason")
	cmd.Flags().StringVar(&serverURL, "server", defaultMCPURL, "Rick MCP server URL")
	return cmd
}

func newResumeCmd() *cobra.Command {
	var dbPath, reason, serverURL string

	cmd := &cobra.Command{
		Use:   "resume <workflow-id>",
		Short: "Resume a paused workflow",
		Long:  `Resume a paused workflow. Blocked persona dispatches are replayed.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResume(cmd.Context(), serverURL, dbPath, args[0], reason)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "rick.db", "SQLite database path (offline fallback)")
	cmd.Flags().StringVar(&reason, "reason", "", "Resume reason")
	cmd.Flags().StringVar(&serverURL, "server", defaultMCPURL, "Rick MCP server URL")
	return cmd
}

func newGuideCmd() *cobra.Command {
	var dbPath, target, serverURL string
	var noResume bool

	cmd := &cobra.Command{
		Use:   "guide <workflow-id> <guidance-text>",
		Short: "Inject operator guidance into a workflow",
		Long: `Inject operator guidance into a paused or running workflow.
The guidance becomes part of the context for the next persona invocation.
By default, auto-resumes a paused workflow.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGuide(cmd.Context(), serverURL, dbPath, args[0], args[1], target, !noResume)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "rick.db", "SQLite database path (offline fallback)")
	cmd.Flags().StringVar(&target, "target", "", "Target persona (optional)")
	cmd.Flags().BoolVar(&noResume, "no-resume", false, "Do not auto-resume after guidance")
	cmd.Flags().StringVar(&serverURL, "server", defaultMCPURL, "Rick MCP server URL")
	return cmd
}

// --- Pause ---

func runPause(ctx context.Context, serverURL, dbPath, aggregateID, reason string) error {
	_, err := mcpCall(ctx, serverURL, "rick_pause_workflow", map[string]any{
		"workflow_id": aggregateID,
		"reason":      reason,
	})
	if err == nil {
		_, _ = fmt.Fprintf(os.Stdout, "Workflow %s paused: %s\n", aggregateID, reason)
		return nil
	}

	_, _ = fmt.Fprintf(os.Stderr, "server unreachable, writing directly to database\n")
	return runPauseDirect(ctx, dbPath, aggregateID, reason)
}

func runPauseDirect(ctx context.Context, dbPath, aggregateID, reason string) error {
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()

	agg, err := replayAggregate(ctx, store, aggregateID)
	if err != nil {
		return err
	}
	if agg.Status != engine.StatusRunning {
		return fmt.Errorf("cannot pause workflow in %s state", agg.Status)
	}

	pauseEvt := event.New(event.WorkflowPaused, 1, event.MustMarshal(event.WorkflowPausedPayload{
		Reason: reason,
		Source: "operator",
	})).
		WithAggregate(aggregateID, agg.Version+1).
		WithCorrelation(aggregateID).
		WithSource("cli:pause")

	if err := store.Append(ctx, aggregateID, agg.Version, []event.Envelope{pauseEvt}); err != nil {
		return fmt.Errorf("store pause event: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "Workflow %s paused: %s\n", aggregateID, reason)
	return nil
}

// --- Resume ---

func runResume(ctx context.Context, serverURL, dbPath, aggregateID, reason string) error {
	_, err := mcpCall(ctx, serverURL, "rick_resume_workflow", map[string]any{
		"workflow_id": aggregateID,
		"reason":      reason,
	})
	if err == nil {
		_, _ = fmt.Fprintf(os.Stdout, "Workflow %s resumed\n", aggregateID)
		return nil
	}

	_, _ = fmt.Fprintf(os.Stderr, "server unreachable, writing directly to database\n")
	return runResumeDirect(ctx, dbPath, aggregateID, reason)
}

func runResumeDirect(ctx context.Context, dbPath, aggregateID, reason string) error {
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()

	agg, err := replayAggregate(ctx, store, aggregateID)
	if err != nil {
		return err
	}
	if agg.Status != engine.StatusPaused {
		return fmt.Errorf("cannot resume workflow in %s state", agg.Status)
	}

	resumeEvt := event.New(event.WorkflowResumed, 1, event.MustMarshal(event.WorkflowResumedPayload{
		Reason: reason,
	})).
		WithAggregate(aggregateID, agg.Version+1).
		WithCorrelation(aggregateID).
		WithSource("cli:resume")

	if err := store.Append(ctx, aggregateID, agg.Version, []event.Envelope{resumeEvt}); err != nil {
		return fmt.Errorf("store resume event: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "Workflow %s resumed\n", aggregateID)
	return nil
}

// --- Guide ---

func runGuide(ctx context.Context, serverURL, dbPath, aggregateID, content, target string, autoResume bool) error {
	args := map[string]any{
		"workflow_id": aggregateID,
		"content":     content,
		"auto_resume": autoResume,
	}
	if target != "" {
		args["target"] = target
	}

	_, err := mcpCall(ctx, serverURL, "rick_inject_guidance", args)
	if err == nil {
		_, _ = fmt.Fprintf(os.Stdout, "Guidance injected into workflow %s\n", aggregateID)
		return nil
	}

	_, _ = fmt.Fprintf(os.Stderr, "server unreachable, writing directly to database\n")
	return runGuideDirect(ctx, dbPath, aggregateID, content, target, autoResume)
}

func runGuideDirect(ctx context.Context, dbPath, aggregateID, content, target string, autoResume bool) error {
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()

	agg, err := replayAggregate(ctx, store, aggregateID)
	if err != nil {
		return err
	}
	if agg.Status != engine.StatusPaused && agg.Status != engine.StatusRunning {
		return fmt.Errorf("cannot inject guidance into workflow in %s state", agg.Status)
	}

	var allEvents []event.Envelope
	nextVersion := agg.Version + 1

	guidanceEvt := event.New(event.OperatorGuidance, 1, event.MustMarshal(event.OperatorGuidancePayload{
		Content:    content,
		Target:     target,
		AutoResume: autoResume,
	})).
		WithAggregate(aggregateID, nextVersion).
		WithCorrelation(aggregateID).
		WithSource("cli:guide")
	allEvents = append(allEvents, guidanceEvt)

	if autoResume && agg.Status == engine.StatusPaused {
		nextVersion++
		resumeEvt := event.New(event.WorkflowResumed, 1, event.MustMarshal(event.WorkflowResumedPayload{
			Reason: "auto-resume after guidance injection",
		})).
			WithAggregate(aggregateID, nextVersion).
			WithCorrelation(aggregateID).
			WithSource("cli:guide")
		allEvents = append(allEvents, resumeEvt)
	}

	if err := store.Append(ctx, aggregateID, agg.Version, allEvents); err != nil {
		return fmt.Errorf("store guidance events: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "Guidance injected into workflow %s\n", aggregateID)
	if autoResume && agg.Status == engine.StatusPaused {
		_, _ = fmt.Fprintf(os.Stdout, "Workflow auto-resumed\n")
	}
	return nil
}
