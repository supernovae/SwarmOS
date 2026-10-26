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
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"time"
)

type RootOptions struct {
	Backend         string
	Repo            string
	RepoRoot        string
	trackerOverride tracker.Tracker
	// githubTrackerFunc and remoteSlugFunc stand in for the GitHub API and
	// github.com URL parsing in tests; repository identity checks still run.
	githubTrackerFunc func(slug string) (tracker.Tracker, error)
	remoteSlugFunc    func(root, remote string) string
	Fork              bool
	ForkRemote        string
	// UpstreamRemote names the project remote; empty uses the node profile's
	// upstream_remote, then origin.
	UpstreamRemote string
	// profileUpstreamRemote is the node profile's upstream_remote, loaded
	// before each command runs.
	profileUpstreamRemote string
	forkPollInterval      time.Duration
	forkPollTimeout       time.Duration
	forkDispatcher        func(context.Context, int, *tracker.Comment) error
	forkVerifyFunc        func(ctx context.Context, forkSlug, expectedOwner string, requirePush bool) (string, error)
	nowFn                 func() time.Time
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
		if err := opts.loadUpstreamRemote(cmd); err != nil {
			return err
		}
		if opts.Fork {
			if err := opts.checkForkRemoteName(); err != nil {
				return err
			}
		}
		if opts.Fork {
			if strings.EqualFold(strings.TrimSpace(opts.Backend), "local") {
				return fmt.Errorf("--fork requires the github backend and cannot be used with --backend local")
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
	rootCmd.PersistentFlags().StringVar(&opts.ForkRemote, "fork-remote", opts.ForkRemote, `Writable fork remote for --fork coordination or upstream-authorized PR updates (default "fork", or "origin" when the upstream remote is not origin)`)
	rootCmd.PersistentFlags().StringVar(&opts.UpstreamRemote, "upstream-remote", opts.UpstreamRemote, `Git remote for the project repository on the github backend (default: the node profile's upstream_remote, then "origin")`)
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

// loadUpstreamRemote reads the node profile's upstream_remote unless the
// command saves the profile itself, and validates the selected name.
func (o *RootOptions) loadUpstreamRemote(cmd *cobra.Command) error {
	o.profileUpstreamRemote = ""
	nodeInit := cmd.Name() == "init" && cmd.Parent() != nil && cmd.Parent().Name() == "node"
	if !nodeInit && cmd.Name() != "version" {
		if strings.TrimSpace(o.UpstreamRemote) == "" {
			remote, err := node.LoadUpstreamRemote("")
			if err != nil {
				return fmt.Errorf("%w; fix or remove the profile, or pass --upstream-remote <project-remote>", err)
			}
			o.profileUpstreamRemote = remote
		}
	}
	upstream := o.effectiveUpstreamRemote()
	if err := workspace.ValidateRemoteName(upstream); err != nil {
		return fmt.Errorf("--upstream-remote: %w", err)
	}
	if upstream != workspace.DefaultUpstreamRemote && strings.TrimSpace(o.UpstreamRemote) != "" && strings.EqualFold(strings.TrimSpace(o.Backend), "local") {
		return fmt.Errorf("--upstream-remote requires the github backend and cannot be used with --backend local")
	}
	return nil
}

// effectiveUpstreamRemote is --upstream-remote, the node profile's
// upstream_remote, or origin, in that order.
func (o *RootOptions) effectiveUpstreamRemote() string {
	return workspace.UpstreamRemoteOrDefault(firstNonEmptyStr(o.UpstreamRemote, o.profileUpstreamRemote))
}

// forkRemoteDefaulted reports whether the fork remote was left to default.
func (o *RootOptions) forkRemoteDefaulted() bool { return strings.TrimSpace(o.ForkRemote) == "" }

// effectiveForkRemote is --fork-remote when given. Otherwise it is fork while
// origin is the upstream, and origin when another remote is the upstream;
// verifyForkRemote checks that origin before any write.
func (o *RootOptions) effectiveForkRemote() string {
	if r := strings.TrimSpace(o.ForkRemote); r != "" {
		return r
	}
	if o.effectiveUpstreamRemote() != workspace.DefaultUpstreamRemote {
		return workspace.DefaultUpstreamRemote
	}
	return workspace.DefaultForkRemote
}

// checkForkRemoteName rejects a fork remote that is the upstream remote.
func (o *RootOptions) checkForkRemoteName() error {
	forkRemote, upstream := o.effectiveForkRemote(), o.effectiveUpstreamRemote()
	if err := workspace.ValidateRemoteName(forkRemote); err != nil {
		return fmt.Errorf("--fork-remote: %w", err)
	}
	if !strings.EqualFold(forkRemote, upstream) {
		return nil
	}
	if upstream == workspace.DefaultUpstreamRemote {
		return fmt.Errorf("--fork-remote cannot be origin; configure a separate personal fork remote, or pass --upstream-remote <project-remote> when origin is your fork")
	}
	return fmt.Errorf("--fork-remote %s is also the upstream remote; pass --fork-remote <verified-personal-fork-remote>, or --upstream-remote <project-remote>", forkRemote)
}

// forkRemoteRemedy explains how to correct a rejected fork remote when the
// upstream remote is not origin; the origin-is-upstream layout keeps its
// established messages.
func (o *RootOptions) forkRemoteRemedy(err error) error {
	upstream := o.effectiveUpstreamRemote()
	if upstream == workspace.DefaultUpstreamRemote {
		return err
	}
	which := fmt.Sprintf("fork remote %q", o.effectiveForkRemote())
	if o.forkRemoteDefaulted() {
		which = fmt.Sprintf("default fork remote %q (used because the upstream remote is %q)", o.effectiveForkRemote(), upstream)
	}
	return fmt.Errorf("%s failed verification: %w; pass --fork-remote <verified-personal-fork-remote> naming your writable fork, or --upstream-remote <project-remote> if %s is not the project", which, err, upstream)
}

func (o *RootOptions) verifyForkRemote(ctx context.Context, root string, tr tracker.Tracker) (string, string, error) {
	forkRemote := o.effectiveForkRemote()
	upstream := o.effectiveUpstreamRemote()
	if err := o.checkForkRemoteName(); err != nil {
		return "", "", err
	}
	forkSlug, err := workspace.ResolveForkRemoteSlug(root, upstream, forkRemote)
	if err != nil {
		return "", "", o.forkRemoteRemedy(err)
	}
	if provider, ok := tr.(interface{ UpstreamRepo() string }); ok && strings.EqualFold(forkSlug, provider.UpstreamRepo()) {
		return "", "", o.forkRemoteRemedy(fmt.Errorf("fork remote %q resolves to %s, the same repository as upstream remote %q", forkRemote, forkSlug, upstream))
	}
	owner, err := tr.CurrentUser(ctx)
	if err != nil || strings.TrimSpace(owner) == "" {
		return "", "", o.forkRemoteRemedy(fmt.Errorf("resolve authenticated GitHub user for fork verification: %w", err))
	}
	if o.forkVerifyFunc != nil {
		canonical, err := o.forkVerifyFunc(ctx, forkSlug, owner, true)
		if err != nil {
			return "", "", o.forkRemoteRemedy(err)
		}
		return forkRemote, canonical, nil
	}
	if gh, ok := tr.(*tracker.GitHubTracker); ok {
		canonical, err := gh.VerifyForkRepository(ctx, forkSlug, owner, true)
		if err != nil {
			return "", "", o.forkRemoteRemedy(err)
		}
		return forkRemote, canonical, nil
	}
	return "", "", fmt.Errorf("tracker %s does not support GitHub fork verification", tr.BackendName())
}
func loadTrustedUpstreamConfig(root, upstreamRemote string) (*contract.SwarmConfig, error) {
	if baseSHA, err := workspace.ResolveBaseFrom(root, upstreamRemote); err == nil && baseSHA != "" {
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
		upstream := o.effectiveUpstreamRemote()
		detected := o.detectUpstreamSlug(root)
		if detected == "" {
			if upstream == workspace.DefaultUpstreamRemote {
				return nil, fmt.Errorf("GitHub coordination requires an origin remote identifying this repository")
			}
			if !hasNamedGitRemote(root, upstream) {
				return nil, workspace.MissingUpstreamRemoteError(upstream)
			}
			return nil, fmt.Errorf("upstream remote %q does not identify a GitHub repository; point it at the project with git remote set-url %s <project-url>, or pass --upstream-remote <project-remote>", upstream, upstream)
		}
		if o.Repo != "" && !strings.EqualFold(o.Repo, detected) {
			if upstream == workspace.DefaultUpstreamRemote {
				return nil, fmt.Errorf("repository identity mismatch: --repo %s does not match origin %s; if origin is your fork, pass --upstream-remote <project-remote>", o.Repo, detected)
			}
			return nil, fmt.Errorf("repository identity mismatch: --repo %s does not match upstream remote %s (%s); point %s at %s, or pass --upstream-remote <project-remote>", o.Repo, upstream, detected, upstream, o.Repo)
		}
		if o.githubTrackerFunc != nil {
			return o.githubTrackerFunc(detected)
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
		cfg, err := loadTrustedUpstreamConfig(root, o.effectiveUpstreamRemote())
		if err != nil {
			return nil, nil, nil, err
		}
		// Only the upstream default branch selects the ledger namespace.
		ns, err := workspace.TrustedLedgerNamespace(root, o.effectiveUpstreamRemote())
		if err != nil {
			return nil, nil, nil, fmt.Errorf("configure authoritative state store: %w", err)
		}
		layout, err := coordination.LayoutFor(ns)
		if err != nil {
			return nil, nil, nil, err
		}
		store := coordination.NewGitStoreWithLayout(root, o.effectiveUpstreamRemote(), layout)
		mgr := lease.NewManager(tr, cfg.Security)
		if o.nowFn != nil {
			mgr.SetNowFunc(o.nowFn)
		}
		mgr.SetUpstreamRemote(o.effectiveUpstreamRemote())
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
	if strings.HasPrefix(tr.BackendName(), "github") {
		mgr.SetUpstreamRemote(o.effectiveUpstreamRemote())
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

// detectUpstreamSlug reads the GitHub owner/repo of the upstream remote.
func (o *RootOptions) detectUpstreamSlug(root string) string {
	if o.remoteSlugFunc != nil {
		return o.remoteSlugFunc(root, o.effectiveUpstreamRemote())
	}
	return registry.DetectGitHubSlugFrom(root, o.effectiveUpstreamRemote())
}
func hasNamedGitRemote(repoRoot, name string) bool {
	return exec.Command("git", "-C", repoRoot, "remote", "get-url", name).Run() == nil
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
