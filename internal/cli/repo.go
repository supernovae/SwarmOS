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
)

func newRepoCmd(rootOpts *RootOptions) *cobra.Command {
	repoCmd := &cobra.Command{
		Use:   "repo",
		Short: "Initialize, plan, validate, and register SwarmOS repositories",
	}

	var cloneIfMissing bool

	addCmd := &cobra.Command{
		Use:   "add [<owner/repo>] [<local-path>]",
		Short: "Register a repository with SwarmOS (optionally cloning it)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var slug, localPath string
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
				}
			}

			reg := registry.New("")
			entry, err := reg.Add(slug, localPath, rootOpts.Backend)
			if err != nil {
				return err
			}

			// Ensure .swarm/ harness is initialized in the registered repository
			if _, err := os.Stat(filepath.Join(entry.Path, ".swarm", "config.yaml")); err != nil {
				_, _ = harness.InitRepository(harness.InitOptions{RepoRoot: entry.Path})
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Registered repository %s -> %s (backend=%s)\n",
				entry.Repo, entry.Path, entry.Backend)
			return nil
		},
	}
	addCmd.Flags().BoolVar(&cloneIfMissing, "clone", false, "Clone repository from GitHub if not present locally")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all registered SwarmOS repositories",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg := registry.New("")
			entries, err := reg.List()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(entries) == 0 {
				fmt.Fprintln(out, "No repositories registered. Run `swarm repo add` inside a repository to register it.")
				return nil
			}
			fmt.Fprintln(out, "REPOSITORY                   BACKEND   HARNESS   LOCAL PATH")
			fmt.Fprintln(out, "----------                   -------   -------   ----------")
			for _, e := range entries {
				harnessState := "ready"
				if _, err := os.Stat(filepath.Join(e.Path, ".swarm", "config.yaml")); err != nil {
					harnessState = "uninitialized"
				}
				fmt.Fprintf(out, "%-28s %-9s %-9s %s\n", e.Repo, e.Backend, harnessState, e.Path)
			}
			return nil
		},
	}

	removeCmd := &cobra.Command{
		Use:   "remove <owner/repo>",
		Short: "Unregister a repository from SwarmOS (does not delete local files)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg := registry.New("")
			if err := reg.Remove(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Unregistered repository %s\n", args[0])
			return nil
		},
	}

	repoCmd.AddCommand(addCmd, listCmd, removeCmd, newInitCmd(rootOpts), newDoctorCmd(rootOpts), newPlanCmd(rootOpts), newRepoValidateCmd(rootOpts))
	return repoCmd
}
