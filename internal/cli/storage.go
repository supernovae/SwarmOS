package cli

import (
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
	"strconv"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/storage"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/tracker"
)

func newStorageCmd(opts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "storage", Short: "Inspect Swarm-managed host storage"}
	jsonOutput := false
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
		snapshot, scanErr := storage.Scan(cmd.Context(), storage.Options{UseRoot: useRoot, RegistryPath: opts.registryPath(), CacheRoot: filepath.Join(cache, "swarm"), ScratchRoot: os.TempDir(), SelectedRoot: root, Repository: opts.Repo, Backend: opts.Backend, Tracker: func(owner, root, backend string) (tracker.PullRequestVisitor, error) {
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
		if err := renderStorageReport(cmd.OutOrStdout(), snapshot, jsonOutput); err != nil {
			return err
		}
		return scanErr
	}}
	report.Flags().BoolVar(&jsonOutput, "json", false, "Print a JSON snapshot including unknown and incomplete observations")
	cmd.AddCommand(report, newStorageCleanupCmd(opts))
	return cmd
}
func renderStorageReport(w io.Writer, s storage.Snapshot, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	if _, err := fmt.Fprintf(w, "Storage report: complete=%t\nScope: %s\nAllocated bytes are observations, not reclaimable savings. Unknown use evidence retains storage.\n", s.Complete, s.Scope); err != nil {
		return err
	}
	for _, reason := range s.Reasons {
		if _, err := fmt.Fprintf(w, "INCOMPLETE: %s\n", reason); err != nil {
			return err
		}
	}
	number := func(v *int64) string {
		if v == nil {
			return "unknown"
		}
		return strconv.FormatInt(*v, 10)
	}
	for _, r := range s.Resources {
		active := "unknown"
		if r.Active != nil {
			active = strconv.FormatBool(*r.Active)
		}
		last := "unknown"
		if r.LastUse != nil {
			last = r.LastUse.UTC().Format("2006-01-02T15:04:05Z")
		}
		if _, err := fmt.Fprintf(w, "%s owner=%q repository=%q path=%q ref=%q\n  logical=%s allocated=%s active=%s last-use=%s eligibility=%s\n  %s; evidence=%s; usage=%s\n", r.Class, r.Owner, r.Repository, r.Path, r.Ref, number(r.LogicalBytes), number(r.AllocatedBytes), active, last, r.Eligibility, r.Reason, r.Evidence, r.UsageReason); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "Resources: %d; entries observed: %d; unique allocated bytes: %s (partial observed: %d)\n", len(s.Resources), s.Counters.Entries, number(s.UniqueAllocatedBytes), s.ObservedAllocatedBytes)
	return err
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
	var apply, asJSON, all bool
	cmd := &cobra.Command{Use: "cleanup", Short: "Preview task retirement and managed host storage reclamation", RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		report := storageCleanupReport{Host: storage.ReclamationReport{Resources: []storage.ReclamationResult{}}, Version: 1, Mode: "preview", Scopes: []storage.SelectedRepository{}, Tasks: []*bulkCleanupSummary{}, Incomplete: []string{}, Errors: []string{}}
		if apply {
			report.Mode = "apply"
		}
		defer func() {
			if runErr != nil {
				report.Errors = append(report.Errors, runErr.Error())
			}
			if asJSON {
				e := writeJSON(cmd, report)
				runErr = errors.Join(runErr, e)
				return
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Storage cleanup %s: eligibility is advisory until apply; byte deltas describe namespace observations, not physical savings.\n", report.Mode)
			for _, s := range report.Tasks {
				fmt.Fprintf(cmd.OutOrStdout(), "Repository %q at %q\n", s.Repository, s.Root)
				renderBulkCleanup(cmd.OutOrStdout(), s)
			}
			for _, r := range report.Host.Resources {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %s: %s\n", r.Resource.Class, r.Resource.Location, r.Outcome, r.Reason)
			}
			for _, r := range report.Incomplete {
				fmt.Fprintln(cmd.OutOrStdout(), "INCOMPLETE:", r)
			}
			for _, r := range report.Errors {
				fmt.Fprintln(cmd.OutOrStdout(), "ERROR:", r)
			}
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
		scopes, reasons, err := storage.DiscoverScopes(cmd.Context(), storage.Options{RegistryPath: opts.registryPath(), SelectedRoot: root, Repository: opts.Repo, Backend: opts.Backend, Remote: opts.effectiveUpstreamRemote()}, all)
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
		budget := storage.Budget{Limits: storage.DefaultLimits()}
		for _, scope := range scopes {
			if err := cmd.Context().Err(); err != nil {
				failures = append(failures, err)
				break
			}
			cp := *opts
			cp.RepoRoot = scope.Root
			cp.Repo = scope.Repository
			cp.Backend = scope.Backend
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
			b := &budget
			states, snapshotErr := storage.ReadRetentionSnapshot(cmd.Context(), scope.RepositoryScope, opts.effectiveUpstreamRemote(), apply, b, visitor)
			if snapshotErr != nil {
				inventoryFailures[scope.CommonDirectory] = snapshotErr
				failures = append(failures, snapshotErr)
				report.Incomplete = append(report.Incomplete, snapshotErr.Error())
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
				e = visitor.VisitPullRequests(cmd.Context(), tracker.InventoryBudget{Records: b.Limits.Records, Pages: 100, Bytes: 4 << 20, Reserve: b.Charge}, func(pr *tracker.PullRequest) error {
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
				m.SetStore(scope.Root, storagePreviewStore(states))
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
		reclaim := storage.ReclaimOptions{Scopes: authorized, CacheRoot: filepath.Join(cache, "swarm"), ScratchRoot: os.TempDir(), ControlRoot: control, Retention: storage.BackendRetention{Failures: inventoryFailures, Budget: &budget, Apply: apply, Remote: opts.effectiveUpstreamRemote(), Tracker: func(s storage.RepositoryScope) (tracker.PullRequestVisitor, error) {
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
			if !report.Host.Complete {
				report.Incomplete = append(report.Incomplete, "host reclamation has incomplete evidence; see retained resource reasons")
			}
		}
		for _, err := range inventoryFailures {
			failures = append(failures, err)
		}
		return errors.Join(failures...)
	}}
	cmd.Flags().BoolVar(&apply, "apply", false, "Retire eligible tasks and reclaim admitted idle storage with fresh evidence")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print one versioned JSON cleanup report")
	cmd.Flags().BoolVar(&all, "all-repos", false, "Select all registered repositories")
	return cmd
}

type storagePreviewStore map[int][]byte

func (s storagePreviewStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	data, ok := s[n]
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
