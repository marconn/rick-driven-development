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
		// Load ~/.config/rick/env before any subcommand runs so RICK_REPOS_PATH
		// and the other operator settings are present even when the process was
		// not launched via the systemd unit that sets EnvironmentFile. Best-effort
		// and additive — already-set vars win — so it never blocks startup.
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			loadConfigEnv()
			return nil
		},
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
