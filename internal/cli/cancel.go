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

func newCancelCmd() *cobra.Command {
	var dbPath, reason, serverURL string

	cmd := &cobra.Command{
		Use:   "cancel <workflow-id>",
		Short: "Cancel a running workflow",
		Long: `Cancel a running or paused workflow. Routes through the MCP server when
available so projections and the event bus stay in sync. Falls back to direct
database access when the server is unreachable.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCancel(cmd.Context(), serverURL, dbPath, args[0], reason)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "rick.db", "SQLite database path (offline fallback)")
	cmd.Flags().StringVar(&reason, "reason", "operator requested", "Cancellation reason")
	cmd.Flags().StringVar(&serverURL, "server", defaultMCPURL, "Rick MCP server URL")
	return cmd
}

func runCancel(ctx context.Context, serverURL, dbPath, aggregateID, reason string) error {
	// Try MCP server first — keeps projections and bus in sync.
	_, err := mcpCall(ctx, serverURL, "rick_cancel_workflow", map[string]any{
		"workflow_id": aggregateID,
		"reason":      reason,
	})
	if err == nil {
		_, _ = fmt.Fprintf(os.Stdout, "Workflow %s cancelled: %s\n", aggregateID, reason)
		return nil
	}

	// Fall back to direct DB access.
	_, _ = fmt.Fprintf(os.Stderr, "server unreachable, writing directly to database\n")
	return runCancelDirect(ctx, dbPath, aggregateID, reason)
}

func runCancelDirect(ctx context.Context, dbPath, aggregateID, reason string) error {
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()

	agg, err := replayAggregate(ctx, store, aggregateID)
	if err != nil {
		return err
	}
	if agg.Status != engine.StatusRunning && agg.Status != engine.StatusPaused {
		return fmt.Errorf("cannot cancel workflow in %s state", agg.Status)
	}

	cancelEvt := event.New(event.WorkflowCancelled, 1, event.MustMarshal(event.WorkflowCancelledPayload{
		Reason: reason,
		Source: "cli",
	})).
		WithAggregate(aggregateID, agg.Version+1).
		WithCorrelation(aggregateID).
		WithSource("cli:cancel")

	if err := store.Append(ctx, aggregateID, agg.Version, []event.Envelope{cancelEvt}); err != nil {
		return fmt.Errorf("store cancel event: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "Workflow %s cancelled: %s\n", aggregateID, reason)
	return nil
}
