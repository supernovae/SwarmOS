package cli

import (
	"fmt"
	"io"
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
	var asJSON bool
	var forkCoordinator bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize .swarm/ harness, GitHub templates, and multi-agent slash commands",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "repo-init", runErr) }()
			repoRoot := rootOpts.resolveRepoRoot()
			if swarmVersion == "" && Version != "" && !strings.Contains(Version, "dev") && !strings.Contains(Version, "snapshot") && !strings.HasSuffix(Version, "+dirty") {
				released := "v" + strings.TrimPrefix(Version, "v")
				if harness.ValidateSwarmVersionPin(released) == nil {
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

			syncedLabels := 0
			out := cmd.OutOrStdout()
			if asJSON {
				out = io.Discard
			}
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
				syncedLabels = len(labels)
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{"kind": "repo-init", "status": "initialized", "language": res.Stack.Language, "created": res.CreatedFiles, "kept": res.SkippedFiles, "warnings": res.Warnings, "synced_labels": syncedLabels})
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing .swarm/ and template files")
	cmd.Flags().BoolVar(&syncLabels, "sync-labels", false, "Idempotently create swarm:* labels on the issue tracker")
	cmd.Flags().StringVar(&swarmVersion, "swarm-version", "", "Resource-CLI-compatible immutable SwarmOS release (v0.6.0 or newer) or full commit SHA for generated CI (defaults to installed compatible release)")
	cmd.Flags().BoolVar(&forkCoordinator, "fork-coordinator", false, "Generate .github/workflows/swarm-fork-coordinator.yml for --fork contributor mode")
	cmd.Flags().StringVar(&templateDir, "template-dir", "", "Directory of custom GitHub Issue Forms (.yml/.yaml) and .swarm/{rules,skills}/*.md templates")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "List individual created and preserved files")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit created and preserved files as JSON")
	return cmd
}
