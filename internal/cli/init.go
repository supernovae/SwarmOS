package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
)

func newInitCmd(rootOpts *RootOptions) *cobra.Command {
	var force bool
	var syncLabels bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize .swarm/ harness, GitHub templates, and multi-agent slash commands",
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot := rootOpts.resolveRepoRoot()
			res, err := harness.InitRepository(harness.InitOptions{
				RepoRoot: repoRoot,
				Force:    force,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Initialized SwarmOS harness (detected stack: %s)\n", res.Stack.Language)
			for _, f := range res.CreatedFiles {
				fmt.Fprintf(out, "  + %s\n", f)
			}
			for _, f := range res.SkippedFiles {
				fmt.Fprintf(out, "  = %s (kept existing)\n", f)
			}

			if syncLabels {
				tr, err := rootOpts.resolveTracker(cmd)
				if err != nil {
					return err
				}
				labels := tracker.DefaultSwarmLabels()
				if err := tr.EnsureLabels(cmd.Context(), labels); err != nil {
					return fmt.Errorf("failed to sync labels on %s: %w", tr.BackendName(), err)
				}
				fmt.Fprintf(out, "Synced %d swarm:* labels on %s\n", len(labels), tr.BackendName())
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing .swarm/ and template files")
	cmd.Flags().BoolVar(&syncLabels, "sync-labels", false, "Idempotently create swarm:* labels on the issue tracker")
	return cmd
}
