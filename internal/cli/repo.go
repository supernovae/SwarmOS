package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newRepoCmd(rootOpts *RootOptions) *cobra.Command {
	repoCmd := &cobra.Command{
		Use:   "repo",
		Short: "Initialize, plan, validate, and register SwarmOS repositories",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	var cloneIfMissing bool
	var asJSON bool

	addCmd := &cobra.Command{
		Use:   "add [<owner/repo>] [<local-path>]",
		Short: "Register a repository with SwarmOS (optionally cloning it)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "repo-add", runErr) }()
			var slug, localPath string
			// A fresh clone names the project origin; an existing checkout
			// uses the selected upstream remote.
			upstreamRemote := rootOpts.effectiveUpstreamRemote()
			if len(args) == 1 {
				// Could be a local path or an owner/repo slug
				if info, err := os.Stat(args[0]); err == nil && info.IsDir() {
					localPath = args[0]
				} else {
					slug = args[0]
				}
			} else if len(args) == 2 {
				slug = args[0]
				localPath = args[1]
			}

			if localPath == "" {
				if slug != "" && cloneIfMissing {
					if err := registry.ValidateRepoSlug(slug); err != nil {
						return err
					}
					parts := strings.Split(slug, "/")
					localPath = filepath.Join(registry.DefaultCloneBaseDir(), parts[0], parts[1])
				} else {
					localPath = rootOpts.resolveRepoRoot()
				}
			}

			if cloneIfMissing {
				if _, err := os.Stat(filepath.Join(localPath, ".git")); err != nil {
					if slug == "" {
						return fmt.Errorf("<owner/repo> is required when using --clone")
					}
					if err := registry.ValidateRepoSlug(slug); err != nil {
						return err
					}
					if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
						return err
					}
					cloneURL := fmt.Sprintf("https://github.com/%s.git", slug)
					if out, err := exec.CommandContext(cmd.Context(), "git", "clone", cloneURL, localPath).CombinedOutput(); err != nil {
						return fmt.Errorf("git clone %s failed: %w (%s)", cloneURL, err, strings.TrimSpace(string(out)))
					}
					upstreamRemote = workspace.DefaultUpstreamRemote
				}
			}

			reg := registry.New("")
			entry, err := reg.AddFromRemote(slug, localPath, rootOpts.Backend, upstreamRemote)
			if err != nil {
				return err
			}

			// Ensure .swarm/ harness is initialized in the registered repository
			if _, err := os.Stat(filepath.Join(entry.Path, ".swarm", "config.yaml")); err != nil {
				if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: entry.Path, SwarmVersion: pinFor(entry.Path), UpstreamRemote: upstreamRemote}); err != nil {
					return fmt.Errorf("registered %s but could not initialize its .swarm/ harness (run swarm repo init --root %s): %w", entry.Repo, entry.Path, err)
				}
			}

			if asJSON {
				return writeJSON(cmd, map[string]any{"kind": "repo-add", "status": "registered", "repository": entry})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Registered repository %s -> %s (backend=%s)\n",
				entry.Repo, entry.Path, entry.Backend)
			return nil
		},
	}
	addCmd.Flags().BoolVar(&cloneIfMissing, "clone", false, "Clone repository from GitHub if not present locally")
	addCmd.Flags().BoolVar(&asJSON, "json", false, "Emit the registered repository as JSON")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all registered SwarmOS repositories",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "repo-list", runErr) }()
			reg := registry.New("")
			entries, err := reg.List()
			if err != nil {
				return err
			}
			if asJSON {
				type listed struct {
					registry.RepoEntry
					Harness string `json:"harness"`
				}
				repos := make([]listed, 0, len(entries))
				for _, e := range entries {
					repos = append(repos, listed{RepoEntry: e, Harness: harnessState(e.Path)})
				}
				return writeJSON(cmd, map[string]any{"kind": "repo-list", "repositories": repos})
			}
			out := cmd.OutOrStdout()
			if len(entries) == 0 {
				fmt.Fprintln(out, "No repositories registered. Run `swarm repo add` inside a repository to register it.")
				return nil
			}
			fmt.Fprintln(out, "REPOSITORY                   BACKEND   HARNESS   LOCAL PATH")
			fmt.Fprintln(out, "----------                   -------   -------   ----------")
			for _, e := range entries {
				fmt.Fprintf(out, "%-28s %-9s %-9s %s\n", e.Repo, e.Backend, harnessState(e.Path), e.Path)
			}
			return nil
		},
	}

	removeCmd := &cobra.Command{
		Use:   "remove <owner/repo>",
		Short: "Unregister a repository from SwarmOS (does not delete local files)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "repo-remove", runErr) }()
			reg := registry.New("")
			if err := reg.Remove(args[0]); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{"kind": "repo-remove", "status": "unregistered", "repo": args[0]})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Unregistered repository %s\n", args[0])
			return nil
		},
	}

	listCmd.Flags().BoolVar(&asJSON, "json", false, "Emit registered repositories as JSON")
	removeCmd.Flags().BoolVar(&asJSON, "json", false, "Emit the unregistered repository as JSON")
	repoCmd.AddCommand(addCmd, listCmd, removeCmd, newInitCmd(rootOpts), newOnboardCmd(), newDoctorCmd(rootOpts), newPlanCmd(rootOpts), newRepoValidateCmd(rootOpts), newLessonsCmd(rootOpts))
	return repoCmd
}

// harnessState reports whether a registered repository has a .swarm/ harness.
func harnessState(path string) string {
	if _, err := os.Stat(filepath.Join(path, ".swarm", "config.yaml")); err != nil {
		return "uninitialized"
	}
	return "ready"
}
