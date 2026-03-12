package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

func newFindCmd() *cobra.Command {
	var dbPath string

	cmd := &cobra.Command{
		Use:   "find <key> <value>",
		Short: "Find workflows by business key",
		Long: `Look up workflows by business identifiers (Jira ticket, repo, source).

Tags are auto-indexed from WorkflowRequested events. Available keys:
  ticket       Jira ticket ID         (e.g., "PROJ-123")
  repo         Repository name        (e.g., "acme/myapp")
  repo_branch  Repo:branch composite  (e.g., "acme/myapp:main")
  source       Source reference        (e.g., "jira:PROJ-123", "gh:owner/repo#1")
  workflow_id  Workflow definition     (e.g., "workspace-dev", "jira-dev")

Examples:
  rick find ticket PROJ-123
  rick find repo acme/myapp
  rick find source "jira:BUG-42"`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFind(cmd.Context(), dbPath, args[0], args[1])
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "rick.db", "SQLite database path")
	return cmd
}

func runFind(ctx context.Context, dbPath, key, value string) error {
	store, err := eventstore.NewSQLiteStore(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()

	ids, err := store.LoadByTag(ctx, key, value)
	if err != nil {
		return fmt.Errorf("load by tag: %w", err)
	}

	if len(ids) == 0 {
		_, _ = fmt.Fprintf(os.Stderr, "no workflows found for %s=%s\n", key, value)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "WORKFLOW ID\tSTATUS\tPROMPT")
	_, _ = fmt.Fprintln(w, "-----------\t------\t------")

	for _, id := range ids {
		status, prompt := "unknown", ""
		agg, err := replayAggregate(ctx, store, id)
		if err == nil {
			status = string(agg.Status)
			prompt = truncate(agg.Prompt, 60)
		}
		// Show short ID if it's a UUID.
		displayID := id
		if len(id) > 12 {
			displayID = id[:12] + "…"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", displayID, status, prompt)
	}

	_ = w.Flush()
	_, _ = fmt.Fprintf(os.Stderr, "\n%d workflow(s) for %s=%s\n", len(ids), key, value)

	// If exactly one result, hint at next steps.
	if len(ids) == 1 {
		_, _ = fmt.Fprintf(os.Stderr, "\nFull ID: %s\n", ids[0])
		_, _ = fmt.Fprintf(os.Stderr, "  rick status %s\n", ids[0])
		_, _ = fmt.Fprintf(os.Stderr, "  rick events -c %s\n", ids[0])
	}

	return nil
}

// replayAggregate is defined in cancel.go — shared across CLI commands.
