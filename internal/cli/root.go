package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/fork"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"time"
)

type RootOptions struct {
	Backend          string
	Repo             string
	RepoRoot         string
	trackerOverride  tracker.Tracker
	Fork             bool
	ForkRemote       string
	forkPollInterval time.Duration
	forkPollTimeout  time.Duration
	forkDispatcher   func(context.Context, int, *tracker.Comment) error
	forkVerifyFunc   func(ctx context.Context, forkSlug, expectedOwner string, requirePush bool) (string, error)
	nowFn            func() time.Time
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
			if err := registry.ValidateRepoSlug(opts.Repo); err != nil {
				return err
			}
		}
		prCommand := cmd.Parent() != nil && cmd.Parent().Name() == "pr"
		if !opts.Fork && !prCommand && cmd.Flags().Changed("fork-remote") {
			return fmt.Errorf("--fork-remote requires --fork or a pr command")
		}
		if opts.Fork {
			if strings.EqualFold(strings.TrimSpace(opts.Backend), "local") {
				return fmt.Errorf("--fork requires the github backend and cannot be used with --backend local")
			}
			if strings.TrimSpace(opts.ForkRemote) == "" {
				opts.ForkRemote = workspace.DefaultForkRemote
			}
			if strings.EqualFold(strings.TrimSpace(opts.ForkRemote), "origin") {
				return fmt.Errorf("--fork-remote cannot be origin; configure a separate personal fork remote")
			}
			for child := cmd; child != nil && child.Parent() != nil; child = child.Parent() {
				if child.Parent().Name() == "issue" {
					switch child.Name() {
					case "create", "approve", "lint", "decompose", "epic":
						return fmt.Errorf("%s is maintainer-only and cannot be used with --fork", child.CommandPath())
					}
					break
				}
			}
		}
		return nil
	}
	rootCmd.AddGroup(
		&cobra.Group{ID: "resources", Title: "Resources:"},
		&cobra.Group{ID: "tools", Title: "Tools:"},
	)
	rootCmd.PersistentFlags().StringVar(&opts.Backend, "backend", firstNonEmptyStr(opts.Backend, os.Getenv("SWARM_BACKEND")), "Tracker backend: github or local")
	rootCmd.PersistentFlags().StringVar(&opts.Repo, "repo", firstNonEmptyStr(opts.Repo, os.Getenv("SWARM_REPO")), "GitHub repository (owner/repo)")
	rootCmd.PersistentFlags().StringVar(&opts.RepoRoot, "root", opts.RepoRoot, "Repository root path")
	rootCmd.PersistentFlags().BoolVar(&opts.Fork, "fork", opts.Fork, "Contribute via a personal GitHub fork and the upstream fork coordinator Action")
	rootCmd.PersistentFlags().StringVar(&opts.ForkRemote, "fork-remote", firstNonEmptyStr(opts.ForkRemote, workspace.DefaultForkRemote), "Writable fork remote for --fork coordination or upstream-authorized PR updates")
	rootCmd.AddCommand(newRepoCmd(opts), newNodeCmd(opts), newIssueCmd(opts), newPRCmd(opts), newForkCoordinatorCmd(opts), newVersionCmd())
	rootCmd.AddCommand(newInternalToolsCmd())
	for _, command := range rootCmd.Commands() {
		switch command.Name() {
		case "repo", "node", "issue", "pr":
			command.GroupID = "resources"
		default:
			command.GroupID = "tools"
		}
	}
	rootCmd.SetHelpCommandGroupID("tools")
	rootCmd.SetCompletionCommandGroupID("tools")
	return rootCmd
}
func Execute() error { return NewRootCmd().Execute() }
func (o *RootOptions) now() time.Time {
	if o != nil && o.nowFn != nil {
		return o.nowFn()
	}
	return time.Now()
}
func (o *RootOptions) effectiveForkRemote() string {
	if r := strings.TrimSpace(o.ForkRemote); r != "" {
		return r
	}
	return workspace.DefaultForkRemote
}
func (o *RootOptions) verifyForkRemote(ctx context.Context, root string, tr tracker.Tracker) (string, string, error) {
	forkRemote := o.effectiveForkRemote()
	if strings.EqualFold(forkRemote, "origin") {
		return "", "", fmt.Errorf("--fork-remote cannot be origin; configure a separate personal fork remote")
	}
	forkSlug, err := workspace.ResolveRemoteGitHubSlug(root, forkRemote)
	if err != nil {
		return "", "", err
	}
	owner, err := tr.CurrentUser(ctx)
	if err != nil || strings.TrimSpace(owner) == "" {
		return "", "", fmt.Errorf("resolve authenticated GitHub user for fork verification: %w", err)
	}
	if o.forkVerifyFunc != nil {
		canonical, err := o.forkVerifyFunc(ctx, forkSlug, owner, true)
		if err != nil {
			return "", "", err
		}
		return forkRemote, canonical, nil
	}
	if gh, ok := tr.(*tracker.GitHubTracker); ok {
		canonical, err := gh.VerifyForkRepository(ctx, forkSlug, owner, true)
		if err != nil {
			return "", "", err
		}
		return forkRemote, canonical, nil
	}
	return "", "", fmt.Errorf("tracker %s does not support GitHub fork verification", tr.BackendName())
}
func loadTrustedUpstreamConfig(root string) (*contract.SwarmConfig, error) {
	if baseSHA, err := workspace.ResolveBase(root); err == nil && baseSHA != "" {
		if cfg, loadErr := harness.LoadConfigAt(root, baseSHA); loadErr == nil && cfg != nil {
			return cfg, nil
		}
	}
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		return nil, fmt.Errorf("load SwarmOS configuration: %w", err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("missing .swarm/config.yaml; run swarm repo init")
	}
	return cfg, nil
}
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
		if o.Fork {
			return nil, fmt.Errorf("--fork requires the github backend and cannot be used with --backend local")
		}
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
	if o.Fork {
		if strings.EqualFold(tr.BackendName(), "local") {
			return nil, nil, nil, fmt.Errorf("--fork requires the github backend and cannot be used with --backend local")
		}
		_, verifiedForkSlug, err := o.verifyForkRemote(cmd.Context(), root, tr)
		if err != nil {
			return nil, nil, nil, err
		}
		cfg, err := loadTrustedUpstreamConfig(root)
		if err != nil {
			return nil, nil, nil, err
		}
		store := coordination.NewGitStore(root)
		mgr := lease.NewManager(tr, cfg.Security)
		if o.nowFn != nil {
			mgr.SetNowFunc(o.nowFn)
		}
		mgr.SetStore(root, store)
		mgr.SetWorkerGateway(&fork.Gateway{
			Tracker:      tr,
			Store:        store,
			ForkRepo:     verifiedForkSlug,
			PollInterval: o.forkPollInterval,
			PollTimeout:  o.forkPollTimeout,
			Dispatcher:   o.forkDispatcher,
			OnProgress:   forkProgressPrinter(progressWriter(cmd)),
		})
		return tr, mgr, cfg, nil
	}
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load SwarmOS configuration: %w", err)
	}
	if cfg == nil {
		return nil, nil, nil, fmt.Errorf("missing .swarm/config.yaml; run swarm repo init")
	}
	mgr := lease.NewManager(tr, cfg.Security)
	if o.nowFn != nil {
		mgr.SetNowFunc(o.nowFn)
	}
	if err := mgr.ConfigureStore(root); err != nil {
		return nil, nil, nil, fmt.Errorf("configure authoritative state store: %w", err)
	}
	return tr, mgr, cfg, nil
}
func (o *RootOptions) resolveTargetLeaseManager(cmd *cobra.Command, tgt registry.RepoEntry) (tracker.Tracker, *lease.Manager, *contract.SwarmConfig, error) {
	cp := *o
	cp.Backend = firstNonEmptyStr(o.Backend, tgt.Backend)
	cp.Repo = tgt.Repo
	cp.RepoRoot = tgt.Path
	return (&cp).resolveLeaseManager(cmd)
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

// parseNumberArg parses a positive issue or PR number, accepting a leading #.
func parseNumberArg(arg, kind string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s number %q", kind, arg)
	}
	return n, nil
}

// resolveIssueAndWorktree finds the checkout an issue command must operate on:
// the managed worktree for the issue, or the current checkout when it holds that
// issue's active bundle. It never guesses between several candidate worktrees.
func resolveIssueAndWorktree(repoRoot string, args []string) (int, string, error) {
	active := 0
	if wu, err := workspace.LoadActiveWorkUnit(repoRoot); err == nil && wu.IssueNumber > 0 {
		active = wu.IssueNumber
	}
	if len(args) > 0 {
		num, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return 0, "", err
		}
		wt := workspace.WorktreePath(repoRoot, num)
		if info, err := os.Stat(wt); err == nil && info.IsDir() {
			return num, wt, nil
		}
		if active == num {
			return num, repoRoot, nil
		}
		return 0, "", fmt.Errorf("no worktree for issue #%d at %s; claim it first with swarm issue claim %d", num, wt, num)
	}
	if active > 0 {
		return active, repoRoot, nil
	}
	wtRoot := filepath.Join(repoRoot, ".swarm", "worktrees")
	var candidates []int
	if entries, err := os.ReadDir(wtRoot); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "issue-") {
				if num, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "issue-")); err == nil && num > 0 {
					candidates = append(candidates, num)
				}
			}
		}
	}
	switch len(candidates) {
	case 0:
		return 0, repoRoot, fmt.Errorf("issue number required (or run inside an active worktree)")
	case 1:
		return candidates[0], workspace.WorktreePath(repoRoot, candidates[0]), nil
	default:
		sort.Ints(candidates)
		labels := make([]string, len(candidates))
		for i, num := range candidates {
			labels[i] = "#" + strconv.Itoa(num)
		}
		return 0, repoRoot, fmt.Errorf("several issue worktrees exist (%s); pass the issue number", strings.Join(labels, ", "))
	}
}
