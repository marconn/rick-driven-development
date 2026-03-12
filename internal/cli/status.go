package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/marconn/rick-event-driven-development/internal/engine"
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

	_, _ = fmt.Fprintf(os.Stdout, "Workflow:    %s\n", aggregateID)
	_, _ = fmt.Fprintf(os.Stdout, "Status:      %s\n", agg.Status)
	_, _ = fmt.Fprintf(os.Stdout, "Version:     %d\n", agg.Version)
	_, _ = fmt.Fprintf(os.Stdout, "Tokens Used: %d\n", agg.TokensUsed)

	if agg.Prompt != "" {
		_, _ = fmt.Fprintf(os.Stdout, "Prompt:      %s\n", truncate(agg.Prompt, 80))
	}

	if agg.Status == engine.StatusPaused || agg.Status == engine.StatusCancelled {
		_, _ = fmt.Fprintln(os.Stdout)
		if agg.Status == engine.StatusPaused {
			_, _ = fmt.Fprintln(os.Stdout, "Use 'rick resume' or 'rick guide' to continue this workflow.")
		}
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
		for name := range seen {
			completed := agg.CompletedPersonas[name]
			feedback := agg.FeedbackCount[name]
			_, _ = fmt.Fprintf(w, "%s\t%v\t%d\n", name, completed, feedback)
		}
		_ = w.Flush()
	}

	return nil
}

