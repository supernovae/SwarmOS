package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/harness"
)

func newOnboardCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "onboard",
		Short: "Print a guide your coding agent can follow to onboard or upgrade this repository",
		Long: `Print the SwarmOS onboarding guide as a prompt for the coding agent of your
choice. The agent previews repo init, proposes validation checks that match your
CI, runs init, and previews role, model, and effort edits for your review. This
command only prints the guide and can run before the repository has SwarmOS files:

  codex "$(swarm repo onboard)"
  claude "$(swarm repo onboard)"

Once a repository is initialized, Claude Code and OpenCode also have the same
guide as the /swarm-onboard slash command.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			guide, err := harness.OnboardingGuide()
			if err != nil {
				reportJSONFailure(cmd, asJSON, "onboard", err)
				return err
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{"kind": "onboard", "guide": guide})
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), guide)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the onboarding guide as JSON")
	return cmd
}
