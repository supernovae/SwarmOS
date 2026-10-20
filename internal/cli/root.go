package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

type RootOptions struct {
	Backend  string
	Repo     string
	RepoRoot string
}

func NewRootCmd() *cobra.Command {
	opts := &RootOptions{}

	rootCmd := &cobra.Command{
		Use:           "swarm",
		Short:         "SwarmOS — Contract-driven multi-agent orchestration on GitHub Issues",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().StringVar(&opts.Backend, "backend", os.Getenv("SWARM_BACKEND"), "Tracker backend: github or local")
	rootCmd.PersistentFlags().StringVar(&opts.Repo, "repo", os.Getenv("SWARM_REPO"), "GitHub repository (owner/repo)")
	rootCmd.PersistentFlags().StringVar(&opts.RepoRoot, "root", "", "Repository root path (defaults to current working directory)")

	rootCmd.AddCommand(
		newInitCmd(opts),
		newDoctorCmd(opts),
		newIssueCmd(opts),
		newClaimCmd(opts),
		newWorkCmd(opts),
		newReviewCmd(opts),
		newValidateCmd(opts),
		newSubmitCmd(opts),
		newStatusCmd(opts),
		newRenewCmd(opts),
		newReleaseCmd(opts),
	)

	return rootCmd
}

func Execute() error {
	return NewRootCmd().Execute()
}

func (o *RootOptions) resolveRepoRoot() string {
	if o.RepoRoot != "" {
		return o.RepoRoot
	}
	return "."
}

func (o *RootOptions) resolveTracker(cmd *cobra.Command) (tracker.Tracker, error) {
	root := o.resolveRepoRoot()
	backend := strings.ToLower(strings.TrimSpace(o.Backend))
	if backend == "local" {
		return tracker.NewLocalTracker(root), nil
	}
	if backend == "github" {
		return tracker.NewGitHubTracker(o.Repo)
	}
	// Auto-detect: try GitHubTracker if remote or --repo is configured, else fallback gracefully to LocalTracker
	if o.Repo != "" || hasGitRemote(root) {
		gt, err := tracker.NewGitHubTracker(o.Repo)
		if err == nil {
			return gt, nil
		}
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Note: No GitHub remote configured; using local tracker (.swarm/local-tracker/state.json)")
	return tracker.NewLocalTracker(root), nil
}

func (o *RootOptions) resolveLeaseManager(cmd *cobra.Command) (tracker.Tracker, *lease.Manager, *contract.SwarmConfig, error) {
	root := o.resolveRepoRoot()
	tr, err := o.resolveTracker(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, _ := harness.LoadConfig(root)
	if cfg == nil {
		cfg = &contract.SwarmConfig{Version: 1, DefaultAgent: "claude"}
	}
	mgr := lease.NewManager(tr, cfg.Security)
	return tr, mgr, cfg, nil
}

func hasGitRemote(repoRoot string) bool {
	out, err := exec.Command("git", "-C", repoRoot, "remote").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func resolveIssueAndWorktree(repoRoot string, args []string) (int, string, error) {
	if len(args) > 0 {
		num, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
		if err != nil || num <= 0 {
			return 0, "", fmt.Errorf("invalid issue number %q", args[0])
		}
		wt := workspace.WorktreePath(repoRoot, num)
		if info, err := os.Stat(wt); err == nil && info.IsDir() {
			return num, wt, nil
		}
		return num, repoRoot, nil
	}
	// Check if current directory has .swarm/active/workunit.json
	if wu, err := workspace.LoadActiveWorkUnit(repoRoot); err == nil && wu.IssueNumber > 0 {
		return wu.IssueNumber, repoRoot, nil
	}
	// Scan .swarm/worktrees/issue-* if exactly one exists
	wtRoot := filepath.Join(repoRoot, ".swarm", "worktrees")
	entries, err := os.ReadDir(wtRoot)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "issue-") {
				if num, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "issue-")); err == nil && num > 0 {
					return num, filepath.Join(wtRoot, e.Name()), nil
				}
			}
		}
	}
	return 0, repoRoot, fmt.Errorf("issue number required (or run inside an active worktree)")
}
