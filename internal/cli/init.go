package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
)

func newInitCmd(rootOpts *RootOptions) *cobra.Command {
	var force bool
	var syncLabels bool
	var swarmVersion string
	var templateDir string
	var verbose bool
	var forkCoordinator bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize .swarm/ harness, GitHub templates, and multi-agent slash commands",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot := rootOpts.resolveRepoRoot()
			if swarmVersion == "" && !forkCoordinator && Version != "" && !strings.Contains(Version, "dev") && !strings.Contains(Version, "snapshot") {
				released := "v" + strings.TrimPrefix(Version, "v")
				if !strings.HasPrefix(released, "v0.1.") && !strings.HasPrefix(released, "v0.0.") {
					swarmVersion = released
				}
			}
			res, err := harness.InitRepository(harness.InitOptions{
				RepoRoot:        repoRoot,
				Force:           force,
				SwarmVersion:    swarmVersion,
				TemplateDir:     templateDir,
				ForkCoordinator: forkCoordinator,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Initialized %s harness: %d files created, %d kept\n", res.Stack.Language, len(res.CreatedFiles), len(res.SkippedFiles))
			if verbose {
				for _, file := range res.CreatedFiles {
					fmt.Fprintf(out, "  created %s\n", file)
				}
				for _, file := range res.SkippedFiles {
					fmt.Fprintf(out, "  kept %s\n", file)
				}
			}
			for _, warning := range res.Warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), warning)
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
	cmd.Flags().StringVar(&swarmVersion, "swarm-version", "", "Immutable SwarmOS release tag or full commit for generated CI")
	cmd.Flags().BoolVar(&forkCoordinator, "fork-coordinator", false, "Generate .github/workflows/swarm-fork-coordinator.yml for --fork contributor mode (requires --swarm-version)")
	cmd.Flags().StringVar(&templateDir, "template-dir", "", "Directory of custom GitHub Issue Forms (.yml/.yaml) and .swarm/{rules,skills}/*.md templates")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "List individual created and preserved files")
	return cmd
}
