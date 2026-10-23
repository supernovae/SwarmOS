package cli

import (
	"fmt"
	"io"
	"regexp"
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
		Long: `Initialize or upgrade the SwarmOS harness. Rerun it after installing a new swarm:
files that are still exactly as SwarmOS generated them are updated, files you
customized are kept and listed, and --force replaces them. Generated workflows
install this build of swarm (or keep the repository's existing pin when run
from a development build). --fork-coordinator adds the fork coordinator
workflow; once present, later runs keep it current.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "repo-init", runErr) }()
			repoRoot := rootOpts.resolveRepoRoot()
			pin, pinNote := resolveSwarmPin(swarmVersion, Version, Commit, repoRoot)
			res, err := harness.InitRepository(harness.InitOptions{
				RepoRoot:        repoRoot,
				Force:           force,
				SwarmVersion:    pin,
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
			fmt.Fprintf(out, "Initialized %s harness: %d created, %d updated, %d already current, %d customized and kept\n", res.Stack.Language, len(res.CreatedFiles), len(res.UpdatedFiles), len(res.CurrentFiles), len(res.SkippedFiles))
			if verbose {
				for _, file := range res.CreatedFiles {
					fmt.Fprintf(out, "  created %s\n", file)
				}
				for _, file := range res.UpdatedFiles {
					fmt.Fprintf(out, "  updated %s\n", file)
				}
			}
			for _, file := range res.SkippedFiles {
				fmt.Fprintf(out, "  kept customized %s (review it against this version, or rerun with --force to replace it)\n", file)
			}
			if pinNote != "" && (len(res.CreatedFiles) > 0 || len(res.UpdatedFiles) > 0) {
				res.Warnings = append(res.Warnings, pinNote)
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
				return writeJSON(cmd, map[string]any{"kind": "repo-init", "status": "initialized", "language": res.Stack.Language, "created": res.CreatedFiles, "updated": res.UpdatedFiles, "current": res.CurrentFiles, "kept": res.SkippedFiles, "warnings": res.Warnings, "synced_labels": syncedLabels})
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing .swarm/ and template files")
	cmd.Flags().BoolVar(&syncLabels, "sync-labels", false, "Idempotently create swarm:* labels on the issue tracker")
	cmd.Flags().StringVar(&swarmVersion, "swarm-version", "", "Override the SwarmOS release (v0.6.0 or newer) or full commit SHA that generated workflows install (defaults to this build)")
	cmd.Flags().BoolVar(&forkCoordinator, "fork-coordinator", false, "Generate .github/workflows/swarm-fork-coordinator.yml for --fork contributor mode")
	cmd.Flags().StringVar(&templateDir, "template-dir", "", "Directory of custom GitHub Issue Forms (.yml/.yaml) and .swarm/{rules,skills}/*.md templates")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "List individual created and preserved files")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit created and preserved files as JSON")
	return cmd
}

// resolveSwarmPin chooses the SwarmOS version generated workflows install: an
// explicit override, this release, the exact commit of a clean build, or the
// pin the repository's workflows already use.
func resolveSwarmPin(override, version, commit, repoRoot string) (pin, note string) {
	if override != "" {
		return override, ""
	}
	if !strings.HasSuffix(version, "+dirty") {
		named := strings.Contains(version, "dev") || strings.Contains(version, "snapshot")
		if release := "v" + strings.TrimPrefix(version, "v"); !named && harness.ValidateSwarmVersionPin(release) == nil {
			return release, ""
		}
		if fullCommit.MatchString(commit) {
			return commit, fmt.Sprintf("generated workflows install swarm at commit %s from this development build; push that commit before CI runs", commit)
		}
	}
	return harness.ExistingSwarmPin(repoRoot), ""
}

var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// pinFor resolves the default workflow pin for repoRoot without an override.
func pinFor(repoRoot string) string {
	pin, _ := resolveSwarmPin("", Version, Commit, repoRoot)
	return pin
}
