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
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

type RootOptions struct {
	Backend         string
	Repo            string
	RepoRoot        string
	trackerOverride tracker.Tracker
}

func NewRootCmd() *cobra.Command {
	return newRootCmdWithOptions(&RootOptions{})
}

func newRootCmdWithOptions(opts *RootOptions) *cobra.Command {
	rootCmd := &cobra.Command{Use: "swarm", Short: "SwarmOS — contract-driven multi-agent orchestration", SilenceUsage: true, SilenceErrors: true}
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if opts.Backend != "" && opts.Backend != "github" && opts.Backend != "local" {
			return fmt.Errorf("unknown tracker backend %q (must be github or local)", opts.Backend)
		}
		if opts.Repo != "" {
			return registry.ValidateRepoSlug(opts.Repo)
		}
		return nil
	}
	rootCmd.AddGroup(
		&cobra.Group{ID: "setup", Title: "Setup:"},
		&cobra.Group{ID: "tasks", Title: "Task lifecycle:"},
		&cobra.Group{ID: "operations", Title: "Operations:"},
	)
	rootCmd.PersistentFlags().StringVar(&opts.Backend, "backend", os.Getenv("SWARM_BACKEND"), "Tracker backend: github or local")
	rootCmd.PersistentFlags().StringVar(&opts.Repo, "repo", os.Getenv("SWARM_REPO"), "GitHub repository (owner/repo)")
	rootCmd.PersistentFlags().StringVar(&opts.RepoRoot, "root", "", "Repository root path")
	rootCmd.AddCommand(newInitCmd(opts), newDoctorCmd(opts), newRepoCmd(opts), newNodeCmd(opts), newIssueCmd(opts), newPlanCmd(opts), newClaimCmd(opts), newPullCmd(opts), newWorkCmd(opts), newStageCmd(opts), newReviewCmd(opts), newValidateCmd(opts), newSubmitCmd(opts), newStatusCmd(opts), newRenewCmd(opts), newReleaseCmd(opts), newVersionCmd())
	for _, command := range rootCmd.Commands() {
		switch command.Name() {
		case "init", "doctor", "repo", "version":
			command.GroupID = "setup"
		case "status", "node", "renew", "release":
			command.GroupID = "operations"
		default:
			command.GroupID = "tasks"
		}
	}
	rootCmd.SetHelpCommandGroupID("setup")
	rootCmd.SetCompletionCommandGroupID("setup")
	return rootCmd
}
func Execute() error { return NewRootCmd().Execute() }
func (o *RootOptions) resolveRepoRoot() string {
	if o.RepoRoot != "" {
		return o.RepoRoot
	}
	if o.Repo != "" {
		if entry, ok, err := registry.New("").Get(o.Repo); err == nil && ok {
			if info, statErr := os.Stat(entry.Path); statErr == nil && info.IsDir() {
				return entry.Path
			}
		}
	}
	return "."
}
func (o *RootOptions) resolveTracker(cmd *cobra.Command) (tracker.Tracker, error) {
	if o.trackerOverride != nil {
		return o.trackerOverride, nil
	}
	root := o.resolveRepoRoot()
	backend := strings.ToLower(strings.TrimSpace(o.Backend))
	if backend == "" && o.Repo != "" {
		if entry, ok, err := registry.New("").Get(o.Repo); err == nil && ok && entry.Backend != "" {
			backend = strings.ToLower(entry.Backend)
		}
	}
	if backend == "" && (o.Repo != "" || hasGitRemote(root)) {
		backend = "github"
	}
	switch backend {
	case "local":
		return tracker.NewLocalTracker(root), nil
	case "github":
		detected := registry.DetectGitHubSlug(root)
		if detected == "" {
			return nil, fmt.Errorf("GitHub coordination requires an origin remote identifying this repository")
		}
		if o.Repo != "" && !strings.EqualFold(o.Repo, detected) {
			return nil, fmt.Errorf("repository identity mismatch: --repo %s does not match origin %s", o.Repo, detected)
		}
		return tracker.NewGitHubTracker(detected)
	case "":
		// Offline operation is never selected as a fallback.
		return nil, fmt.Errorf("tracker backend is required; pass --backend local for offline use or --backend github")
	default:
		return nil, fmt.Errorf("unknown tracker backend %q (must be github or local)", o.Backend)
	}
}
func (o *RootOptions) resolveLeaseManager(cmd *cobra.Command) (tracker.Tracker, *lease.Manager, *contract.SwarmConfig, error) {
	root := o.resolveRepoRoot()
	tr, err := o.resolveTracker(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load SwarmOS configuration: %w", err)
	}
	if cfg == nil {
		return nil, nil, nil, fmt.Errorf("missing .swarm/config.yaml; run swarm init")
	}
	mgr := lease.NewManager(tr, cfg.Security)
	if err := mgr.ConfigureStore(root); err != nil {
		return nil, nil, nil, fmt.Errorf("configure authoritative state store: %w", err)
	}
	return tr, mgr, cfg, nil
}
func (o *RootOptions) resolveTargetLeaseManager(cmd *cobra.Command, tgt registry.RepoEntry) (tracker.Tracker, *lease.Manager, *contract.SwarmConfig, error) {
	return (&RootOptions{Backend: firstNonEmptyStr(o.Backend, tgt.Backend), Repo: tgt.Repo, RepoRoot: tgt.Path}).resolveLeaseManager(cmd)
}
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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
	if wu, err := workspace.LoadActiveWorkUnit(repoRoot); err == nil && wu.IssueNumber > 0 {
		return wu.IssueNumber, repoRoot, nil
	}
	wtRoot := filepath.Join(repoRoot, ".swarm", "worktrees")
	if entries, err := os.ReadDir(wtRoot); err == nil {
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
