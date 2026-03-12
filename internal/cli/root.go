package cli

import (
	"github.com/spf13/cobra"
)

// New creates the root command for the rick CLI.
func New() *cobra.Command {
	root := &cobra.Command{
		Use:   "rick",
		Short: "Rick v2 — event-driven AI orchestrator",
		Long: `Rick v2 is an event-driven AI workflow system that executes structured
development workflows using AI backends (Claude, Gemini) with full
event sourcing, pure event choreography, and feedback loops.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newRunCmd(),
		newPlanCmd(),
		newEventsCmd(),
		newStatusCmd(),
		newFindCmd(),
		newMCPCmd(),
		newServeCmd(),
		newCancelCmd(),
		newPauseCmd(),
		newResumeCmd(),
		newGuideCmd(),
	)

	return root
}
