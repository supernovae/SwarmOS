package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workspace"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/storage"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/tracker"
)

func newStorageCmd(opts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "storage", Short: "Inspect and clean up Swarm-managed host storage"}
	jsonOutput := false
	details := false
	report := &cobra.Command{Use: "report", Short: "Report storage observations without changing storage or workflow state", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		root := opts.RepoRoot
		if root != "" {
			root, err = filepath.Abs(root)
			if err != nil {
				return err
			}
		}
		useRoot, err := storageuse.ControlRoot()
		if err != nil {
			return err
		}
		snapshot, scanErr := storage.Scan(cmd.Context(), storage.Options{UseRoot: useRoot, RegistryPath: opts.registryPath(), CacheRoot: filepath.Join(cache, "swarm"), ScratchRoot: os.TempDir(), SelectedRoot: root, Repository: opts.Repo, Tracker: func(owner, root, backend string) (tracker.PullRequestVisitor, error) {
			if backend == "local" {
				return tracker.NewLocalTracker(root), nil
			}
			if backend != "github" {
				return nil, fmt.Errorf("repository backend unavailable for retention")
			}
			var tr tracker.Tracker
			var err error
			if opts.trackerOverride != nil {
				tr = opts.trackerOverride
			} else if opts.githubTrackerFunc != nil {
				tr, err = opts.githubTrackerFunc(owner)
			} else {
				tr, err = tracker.NewGitHubTracker(owner)
			}
			if err != nil {
				return nil, err
			}
			visitor, ok := tr.(tracker.PullRequestVisitor)
			if !ok {
				return nil, fmt.Errorf("bounded tracker unavailable")
			}
			return visitor, nil
		}})
		if err := renderStorageReport(cmd.OutOrStdout(), snapshot, jsonOutput, details, storageReportNext(opts, details)); err != nil {
			return err
		}
		return scanErr
	}}
	report.Flags().BoolVar(&jsonOutput, "json", false, "Print a JSON snapshot including unknown and incomplete observations")
	report.Flags().BoolVar(&details, "details", false, "List every resource and its observations")
	cmd.AddCommand(report, newStorageCleanupCmd(opts))
	return cmd
}
func renderStorageReport(w io.Writer, s storage.Snapshot, asJSON, details bool, next string) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	var text bytes.Buffer
	renderStorageSummary(&text, s, details)
	fmt.Fprintf(&text, "Next: %s\n", next)
	_, err := w.Write(text.Bytes())
	return err
}

func storageReportNext(opts *RootOptions, details bool) string {
	next := "swarm storage report --details"
	if details {
		next = "swarm storage cleanup"
	}
	return next + outputSelectors(opts)
}

func outputSelectors(opts *RootOptions) string {
	s := ""
	if opts.RepoRoot != "" {
		s += " --root " + shellArg(opts.RepoRoot)
	}
	if opts.UpstreamRemote != "" {
		s += " --upstream-remote " + shellArg(opts.UpstreamRemote)
	}
	if opts.Repo != "" {
		s += " --repo " + shellArg(opts.Repo)
	}
	return s
}
func shellArg(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func observedBytes(n *int64) string {
	if n == nil {
		return "unknown"
	}
	return humanBytes(*n)
}
func renderStorageSummary(w io.Writer, s storage.Snapshot, details bool) {
	fmt.Fprintf(w, "Storage report\nScope: %s\n", s.Scope)
	type total struct {
		count, unknownLogical, unknownAllocated int
		logical, allocated                      int64
	}
	classes := map[string]*total{}
	var kept reasonTally
	for _, r := range s.Resources {
		t := classes[r.Class]
		if t == nil {
			t = &total{}
			classes[r.Class] = t
		}
		t.count++
		if r.LogicalBytes == nil {
			t.unknownLogical++
		} else {
			t.logical += *r.LogicalBytes
		}
		if r.AllocatedBytes == nil {
			t.unknownAllocated++
		} else {
			t.allocated += *r.AllocatedBytes
		}
		if r.Eligibility != "eligible" {
			kept.add(r.Reason)
		}
	}
	keys := []string{}
	for k := range classes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t := classes[k]
		fmt.Fprintf(w, "  %s: %d resources; logical %s observed (%d unknown); allocated %s observed (%d unknown)\n", k, t.count, humanBytes(t.logical), t.unknownLogical, humanBytes(t.allocated), t.unknownAllocated)
	}
	fmt.Fprintf(w, "Resources: %d; unique allocated: %s; partial observed: %s\nAllocated sizes are observations, not reclaimable savings.\n", len(s.Resources), observedBytes(s.UniqueAllocatedBytes), humanBytes(s.ObservedAllocatedBytes))
	kept.render(w, "  ")
	if !s.Complete || len(s.Reasons) > 0 {
		fmt.Fprintln(w, "Report incomplete:")
		seen := map[string]bool{}
		for _, r := range s.Reasons {
			if !seen[r] {
				fmt.Fprintln(w, "  "+r)
				seen[r] = true
			}
		}
	}
	if details {
		for _, r := range s.Resources {
			fmt.Fprintf(w, "%s: %s; %s\n", r.Class, r.Eligibility, r.Reason)
			for _, v := range []struct{ name, value string }{{"owner", r.Owner}, {"repository", r.Repository}, {"path", r.Path}, {"ref", r.Ref}, {"evidence", r.Evidence}, {"usage", r.UsageReason}} {
				if v.value != "" {
					fmt.Fprintf(w, "  %s: %s\n", v.name, v.value)
				}
			}
			active := "unknown"
			if r.Active != nil {
				active = fmt.Sprint(*r.Active)
			}
			last := "unknown"
			if r.LastUse != nil {
				last = r.LastUse.UTC().Format("2006-01-02")
			}
			fmt.Fprintf(w, "  logical: %s; allocated: %s; active: %s; last use: %s\n", observedBytes(r.LogicalBytes), observedBytes(r.AllocatedBytes), active, last)
		}
	}
}

type storageCleanupReport struct {
	Version    int                          `json:"version"`
	Mode       string                       `json:"mode"`
	Scopes     []storage.SelectedRepository `json:"scopes"`
	Tasks      []*bulkCleanupSummary        `json:"tasks"`
	Host       storage.ReclamationReport    `json:"host"`
	Incomplete []string                     `json:"incomplete_reasons"`
	Errors     []string                     `json:"errors"`
	Totals     bulkCleanupCounts            `json:"task_totals"`
}

func newStorageCleanupCmd(opts *RootOptions) *cobra.Command {
	var apply, asJSON, all, details bool
	cmd := &cobra.Command{Use: "cleanup", Short: "Preview, or with --apply remove, finished task worktrees and idle Swarm caches", RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		report := storageCleanupReport{Host: storage.ReclamationReport{Resources: []storage.ReclamationResult{}}, Version: 1, Mode: "preview", Scopes: []storage.SelectedRepository{}, Tasks: []*bulkCleanupSummary{}, Incomplete: []string{}, Errors: []string{}}
		if apply {
			report.Mode = "apply"
		}
		defer func() {
			if runErr != nil {
				seen := map[string]bool{}
				for _, cause := range strings.Split(runErr.Error(), "\n") {
					cause = strings.TrimSpace(cause)
					if cause != "" && !seen[cause] {
						report.Errors = append(report.Errors, cause)
						seen[cause] = true
					}
				}
			}
			if asJSON {
				e := writeJSON(cmd, report)
				runErr = errors.Join(runErr, e)
				return
			}
			renderStorageCleanup(cmd.OutOrStdout(), report, details)
		}()
		if len(args) > 0 {
			return errors.New("storage cleanup takes no positional arguments")
		}
		if opts.Fork {
			return errors.New("storage cleanup requires an upstream operator")
		}
		if all && (opts.RepoRoot != "" || opts.Repo != "") {
			return errors.New("--all-repos conflicts with --root and --repo")
		}
		root := opts.RepoRoot
		if root != "" {
			var err error
			root, err = filepath.Abs(root)
			if err != nil {
				return err
			}
		}
		scopes, reasons, err := storage.DiscoverScopes(cmd.Context(), storage.Options{RegistryPath: opts.registryPath(), SelectedRoot: root, Repository: opts.Repo, Remote: opts.effectiveUpstreamRemote()}, all)
		if all && err == nil && len(reasons) == 0 && len(scopes) == 0 {
			return errors.New("no repositories are registered; run swarm repo add inside a repository, or run swarm storage cleanup without --all-repos")
		}
		report.Scopes = scopes
		report.Incomplete = reasons
		failures := []error{}
		for _, reason := range reasons {
			failures = append(failures, errors.New(reason))
		}
		if err != nil {
			failures = append(failures, err)
		}
		authorized := []storage.RepositoryScope{}
		providers := map[string]tracker.PullRequestVisitor{}
		inventoryFailures := map[string]error{}

		for _, scope := range scopes {
			if err := cmd.Context().Err(); err != nil {
				failures = append(failures, err)
				break
			}
			cp := *opts
			cp.RepoRoot = scope.Root
			cp.Repo = scope.Repository
			var tr tracker.Tracker
			var m *lease.Manager
			var err error
			if apply {
				tr, m, _, err = cp.resolveLeaseManager(cmd)
				if err == nil && m.HasWorkerGateway() {
					err = errors.New("cleanup is unavailable to agent workers")
				}
				if err == nil {
					err = tr.Authorize(cmd.Context(), "approve-task")
				}
			} else {
				tr, err = cp.resolveTracker(cmd)
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", scope.Root, err))
				continue
			}
			visitor, ok := tr.(tracker.PullRequestVisitor)
			if !ok {
				failures = append(failures, errors.New("bounded tracker unavailable"))
				continue
			}
			providers[scope.CommonDirectory] = visitor
			authorized = append(authorized, scope.RepositoryScope)
			b := &storage.Budget{Limits: storage.DefaultLimits()}
			states, snapshotErr := storage.ReadRetentionSnapshot(cmd.Context(), scope.RepositoryScope, opts.effectiveUpstreamRemote(), apply, b, visitor)
			if snapshotErr != nil {
				inventoryFailures[scope.CommonDirectory] = snapshotErr
				failures = append(failures, snapshotErr)
				report.Incomplete = append(report.Incomplete, snapshotErr.Error())
			}
			if snapshotErr != nil && (apply || len(states) == 0) {
				summary := &bulkCleanupSummary{Repository: scope.Repository, Root: scope.Root, Kind: "cleanup-bulk", Mode: report.Mode, Status: "failed", Error: snapshotErr.Error(), Issues: []bulkCleanupIssue{}, Unresolved: []lease.UnresolvedCleanupWorktree{}}
				worktrees, discoveryErr := workspace.DiscoverCleanupWorktrees(cmd.Context(), scope.Root)
				if discoveryErr != nil {
					failures = append(failures, discoveryErr)
				}
				for _, w := range worktrees {
					summary.Unresolved = append(summary.Unresolved, lease.UnresolvedCleanupWorktree{CleanupWorktree: w, Reason: snapshotErr.Error()})
					summary.Totals.Retained++
				}
				if discoveryErr == nil && len(worktrees) == 0 && apply {
					summary.Status = "completed"
				}
				report.Totals.Retained += summary.Totals.Retained
				report.Tasks = append(report.Tasks, summary)
				continue
			}
			if !apply {
				var e error
				pt := &storagePreviewTracker{Tracker: tr, states: states}
				e = visitor.VisitPullRequests(cmd.Context(), tracker.InventoryBudget{Records: b.Limits.Records, Pages: 100, Bytes: 128 << 20, Reserve: b.Charge, Workspace: b.Workspace}, func(pr *tracker.PullRequest) error {
					if pr == nil {
						return errors.New("invalid PR evidence")
					}
					if e := b.Charge(4096); e != nil {
						return e
					}
					pt.prs = append(pt.prs, pr)
					return nil
				})
				if e != nil {
					inventoryFailures[scope.CommonDirectory] = e
					report.Incomplete = append(report.Incomplete, e.Error())
					failures = append(failures, e)
					continue
				}
				var previewTracker tracker.Tracker = pt
				if scope.Backend == "github" {
					previewTracker = &storageUpstreamPreview{pt, scope.Repository}
				}
				m = lease.NewManager(previewTracker, contract.SecurityConfig{})
				issueErrors := map[int]error{}
				if snapshotErr != nil {
					for n, data := range states {
						if data != nil {
							continue
						}
						for _, cause := range strings.Split(snapshotErr.Error(), "\n") {
							if strings.HasPrefix(cause, fmt.Sprintf("issue %d:", n)) {
								issueErrors[n] = errors.New(cause)
								break
							}
						}
					}
				}
				m.SetStore(scope.Root, storagePreviewStore{states, issueErrors})
			}
			ctx := cmd.Context()
			if !apply {
				ctx = gitcredential.WithLocalObservation(ctx)
			}
			summary, e := executeBulkCleanup(ctx, scope.Root, tr, m, apply)
			summary.Repository = scope.Repository
			summary.Root = scope.Root
			report.Tasks = append(report.Tasks, summary)
			report.Totals.Eligible += summary.Totals.Eligible
			report.Totals.Retired += summary.Totals.Retired
			report.Totals.Retained += summary.Totals.Retained
			report.Totals.Failed += summary.Totals.Failed
			if e != nil {
				failures = append(failures, e)
			}
		}
		cache, err := os.UserCacheDir()
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		control, err := storageuse.ControlRoot()
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		reclaim := storage.ReclaimOptions{Scopes: authorized, CacheRoot: filepath.Join(cache, "swarm"), ScratchRoot: os.TempDir(), ControlRoot: control, Retention: storage.BackendRetention{Failures: inventoryFailures, Apply: apply, Remote: opts.effectiveUpstreamRemote(), Tracker: func(s storage.RepositoryScope) (tracker.PullRequestVisitor, error) {
			if err := inventoryFailures[s.CommonDirectory]; err != nil {
				return nil, err
			}
			tr := providers[s.CommonDirectory]
			if tr == nil {
				return nil, errors.New("repository not authorized")
			}
			return tr, nil
		}}}
		if len(authorized) > 0 {
			if apply {
				report.Host, err = storage.ApplyReclamation(cmd.Context(), reclaim)
			} else {
				report.Host, err = storage.PreviewReclamation(cmd.Context(), reclaim)
			}
			if err != nil {
				failures = append(failures, err)
			}
			report.Incomplete = append(report.Incomplete, report.Host.DiscoveryReasons...)
			if !report.Host.Complete {
				report.Incomplete = append(report.Incomplete, "host reclamation has incomplete evidence; see retained resource reasons")
			}
		}
		for _, err := range inventoryFailures {
			failures = append(failures, err)
		}
		unique := []error{}
		seen := map[string]bool{}
		for _, failure := range failures {
			for _, cause := range strings.Split(failure.Error(), "\n") {
				cause = strings.TrimSpace(cause)
				if strings.HasPrefix(cause, "issue #") {
					if _, rest, ok := strings.Cut(cause, ": "); ok && strings.HasPrefix(rest, "issue ") {
						cause = rest
					}
				}
				if cause != "" && !seen[cause] {
					unique = append(unique, errors.New(cause))
					seen[cause] = true
				}
			}
		}
		if cmd.Context().Err() != nil {
			return cmd.Context().Err()
		}
		return errors.Join(unique...)
	}}
	cmd.Flags().BoolVar(&apply, "apply", false, "Retire eligible tasks and reclaim admitted idle storage with fresh evidence")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print one versioned JSON cleanup report")
	cmd.Flags().BoolVar(&all, "all-repos", false, "Select all registered repositories")
	cmd.Flags().BoolVar(&details, "details", false, "List every task resource and host cache with its outcome and reason")
	return cmd
}

type storagePreviewStore struct {
	states   map[int][]byte
	failures map[int]error
}

func (s storagePreviewStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := s.failures[n]; err != nil {
		return nil, "", err
	}
	data, ok := s.states[n]
	if !ok {
		return nil, "", coordination.ErrNotFound
	}
	return data, "preview", nil
}
func (s storagePreviewStore) CompareAndSwap(context.Context, int, string, []byte) (string, error) {
	return "", errors.New("preview is read-only")
}

type storagePreviewTracker struct {
	tracker.Tracker
	states map[int][]byte
	prs    []*tracker.PullRequest
}

func (t *storagePreviewTracker) ListPullRequests(context.Context, tracker.PullRequestFilter) ([]*tracker.PullRequest, error) {
	return t.prs, nil
}
func (t *storagePreviewTracker) ListIssues(context.Context, tracker.IssueFilter) ([]*tracker.Issue, error) {
	out := make([]*tracker.Issue, 0, len(t.states))
	for n := range t.states {
		out = append(out, &tracker.Issue{Number: n})
	}
	return out, nil
}

type storageUpstreamPreview struct {
	*storagePreviewTracker
	owner string
}

func (t *storageUpstreamPreview) UpstreamRepo() string { return t.owner }
