package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/harness"
)

func newOnboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "onboard",
		Short: "Print a guide your coding agent can follow to onboard or upgrade this repository",
		Long: `Print the SwarmOS onboarding guide as a prompt for the coding agent of your
choice. The agent previews repo init, proposes validation checks that match your
CI, runs init, merges SwarmOS files with what you already have, and hands the
result back for review. Nothing is written by this command, so it works before
the repository has any SwarmOS files:

  codex "$(swarm repo onboard)"
  claude "$(swarm repo onboard)"

Once a repository is initialized, Claude Code and OpenCode also have the same
guide as the /swarm-onboard slash command.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			guide, err := harness.OnboardingGuide()
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), guide)
			return err
		},
	}
}
