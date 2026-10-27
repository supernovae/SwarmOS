package pr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

type Service struct {
	Version    string
	workWait   func(context.Context) error
	OnProgress func(progress.Event)
	// Progress is an optional CLI-owned session spanning repository resolution.
	Progress *progress.Session

	Root       string
	Tracker    tracker.Tracker
	Manager    *lease.Manager
	Fork       bool
	ForkRemote string
	// UpstreamRemote names the project remote; empty uses the lease
	// manager's, which defaults to origin.
	UpstreamRemote string
	// Now overrides the clock used for lease expiry checks (tests).
	Now func() time.Time
	// NewRunner builds role runners for work and rebase conflict resolution;
	// nil uses the configured runner from approved policy.
	NewRunner func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error)
	// LookupEnv resolves execution mode and agent environment for PR work,
	// conflict resolution, plan review and validation; nil reads the process
	// environment.
	LookupEnv func(string) (string, bool)
	// NodeProfilePath locates the node profile for engines; empty uses the
	// default path.
	NodeProfilePath string
	// ResolveLog receives coordinator-owned conflict resolution progress.
	ResolveLog io.Writer
}
type Options struct {
	Number       int
	Work         workflow.WorkOptions
	Instructions string
	Continue     bool
	Abort        bool
	Resolve      bool
}
type Result struct {
	WorkPreflight *WorkPreflightResult          `json:"work_preflight,omitempty"`
	SubmitCommand string                        `json:"submit_command,omitempty"`
	CIFailures    bool                          `json:"ci_failures,omitempty"`
	PR            *tracker.PullRequest          `json:"pr"`
	IssueNumber   int                           `json:"issue_number"`
	State         *lease.IssueState             `json:"state,omitempty"`
	Worktree      string                        `json:"worktree,omitempty"`
	Branch        string                        `json:"branch,omitempty"`
	HeadSHA       string                        `json:"head_sha,omitempty"`
	BaseSHA       string                        `json:"base_sha,omitempty"`
	Checks        []tracker.CheckRunResult      `json:"checks,omitempty"`
	CodeScanning  *tracker.CodeScanningEvidence `json:"code_scanning,omitempty"`
	Reviews       []tracker.PullRequestReview   `json:"reviews,omitempty"`
	Status        string                        `json:"status"`
	Dirty         bool                          `json:"dirty"`
	Rebasing      bool                          `json:"rebasing"`
	Validation    *validation.ValidationReport  `json:"validation,omitempty"`
	Preflight     *workflow.PreflightResult     `json:"preflight,omitempty"`
	Work          *workflow.WorkResult          `json:"work,omitempty"`
	// RoleRuns is the issue's validated per-run telemetry, set by Status.
	RoleRuns []contract.RoleRunTelemetry `json:"role_runs,omitempty"`
	// Repair is the trusted summary of the issue's failed validation, set by
	// Status from the recorded artifact; status never reruns checks.
	Repair *validation.RepairSummary `json:"repair,omitempty"`
	// LocalDroppedPaths is the fork contributor coordinator's own comparison,
	// never upstream evidence; upstream records it when the head is published.
	LocalDroppedPaths *LocalDroppedPaths `json:"local_dropped_paths,omitempty"`
}

// LocalDroppedPaths labels a changed-path comparison made from local Git objects.
type LocalDroppedPaths struct {
	Source string   `json:"source"`
	Paths  []string `json:"paths"`
}

const localDroppedSource = "contributor coordinator (local Git objects; upstream records it at swarm --fork pr submit)"

// diagnoseLocalDroppedPaths compares a completed fork rebase that upstream
// could not inspect yet, using the commits in the local PR worktree.
func (s *Service) diagnoseLocalDroppedPaths(ctx context.Context, r *Result) {
	if r == nil {
		return
	}
	r.LocalDroppedPaths = nil
	if r.State == nil || r.State.ForkRepo == "" || r.Worktree == "" || r.Rebasing {
		return
	}
	ev := r.State.LastRebase
	if ev == nil || ev.Status != "completed" || ev.DroppedPathsVerified || ev.NewHead == "" {
		return
	}
	local := *ev
	local.VerifyDroppedPaths(ctx, r.Worktree)
	if local.DroppedPathsVerified {
		r.LocalDroppedPaths = &LocalDroppedPaths{Source: localDroppedSource, Paths: local.DroppedPaths}
	}
}

func headBranch(pr *tracker.PullRequest) string {
	if i := strings.IndexByte(pr.Head, ':'); i >= 0 {
		return pr.Head[i+1:]
	}
	return pr.Head
}

func revisionHead(st *lease.IssueState) string {
	if st != nil && st.PRRevision != nil {
		return st.PRRevision.LocalHeadSHA
	}
	return ""
}

// linked accepts only the authoritative issue coordination state, never PR
// descriptions, titles, naming conventions or unverified tracker hints. Hints
// only choose which issue's state to read first; a hinted issue that does not
// record this PR falls back to scanning every issue.
func (s *Service) linked(ctx context.Context, number int) (*Result, error) {
	r, err := s.linkedInspection(ctx, number)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(r.PR.State, "open") || r.PR.Merged {
		return nil, fmt.Errorf("PR #%d must be open and unmerged", number)
	}
	return r, nil
}

func (s *Service) linkedInspection(ctx context.Context, number int) (*Result, error) {
	if number <= 0 {
		return nil, fmt.Errorf("positive PR number required")
	}
	pr, err := s.Tracker.GetPullRequest(ctx, number)
	if err != nil {
		return nil, err
	}
	if pr == nil || pr.Number != number {
		return nil, fmt.Errorf("PR #%d identity mismatch", number)
	}
	var result *Result
	for _, hint := range linkHints(pr) {
		issue, st, err := s.linkedState(ctx, hint, number)
		if err != nil {
			return nil, err
		}
		if st != nil {
			if result, err = s.verifyLink(pr, issue, st); err != nil {
				return nil, err
			}
			break
		}
	}
	if result == nil {
		issues, err := s.Tracker.ListIssues(ctx, tracker.IssueFilter{State: "all"})
		if err != nil {
			return nil, err
		}
		for i := range issues {
			st, stateErr := s.Manager.GetCurrentState(ctx, issues[i].Number)
			if errors.Is(stateErr, coordination.ErrNotFound) {
				continue
			}
			if stateErr != nil {
				return nil, fmt.Errorf("read issue #%d authoritative state: %w", issues[i].Number, stateErr)
			}
			if st == nil || st.PRNumber != number {
				continue
			}
			if result != nil {
				return nil, fmt.Errorf("PR #%d has multiple authoritative issue links", number)
			}
			if result, err = s.verifyLink(pr, issues[i], st); err != nil {
				return nil, err
			}
		}
	}
	if result == nil {
		return nil, fmt.Errorf("PR #%d is not linked to an approved WorkUnit", number)
	}
	if !strings.EqualFold(pr.State, "open") || pr.Merged {
		result.Status = "closed-unmerged"
		if pr.Merged {
			result.Status = "merged"
		}
		return result, nil
	}
	if result.State.PRRevision != nil {
		rev := result.State.PRRevision
		if rev.Number != number {
			return nil, fmt.Errorf("PR revision identity mismatch")
		}
		result.HeadSHA, result.Branch, result.Status = rev.LocalHeadSHA, rev.Branch, "pending"
		if rev.RemoteHeadSHA != rev.LocalHeadSHA && pr.HeadSHA == rev.LocalHeadSHA {
			result.Status = StatusPublishedPending
		} else if pr.HeadSHA != rev.RemoteHeadSHA {
			result.Status = StatusDiverged
		}
	} else if result.State.HeadSHA != "" && pr.HeadSHA != result.State.HeadSHA {
		result.Status = StatusUnverifiedHead
	}
	return result, nil
}

var closingReference = regexp.MustCompile(`(?im)^\s*(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

// linkHints lists candidate issue numbers for a PR: the tracker's recorded
// link, then the closing reference SwarmOS writes into draft PR bodies.
func linkHints(pr *tracker.PullRequest) []int {
	var hints []int
	if pr.IssueNumber > 0 {
		hints = append(hints, pr.IssueNumber)
	}
	if m := closingReference.FindStringSubmatch(pr.Body); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n != pr.IssueNumber {
			hints = append(hints, n)
		}
	}
	return hints
}

// linkedState returns the hinted issue and its state only when that state
// authoritatively records the PR.
func (s *Service) linkedState(ctx context.Context, issueNumber, prNumber int) (*tracker.Issue, *lease.IssueState, error) {
	st, err := s.Manager.GetCurrentState(ctx, issueNumber)
	if errors.Is(err, coordination.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read issue #%d authoritative state: %w", issueNumber, err)
	}
	if st == nil || st.PRNumber != prNumber {
		return nil, nil, nil
	}
	issue, err := s.Tracker.GetIssue(ctx, issueNumber)
	if err != nil {
		return nil, nil, err
	}
	return issue, st, nil
}

// verifyLink checks a PR against the approved state of the issue that records it.
func (s *Service) verifyLink(pr *tracker.PullRequest, issue *tracker.Issue, st *lease.IssueState) (*Result, error) {
	number := pr.Number
	cfg, err := harness.LoadConfigAt(s.Root, st.BaseSHA)
	if err != nil {
		return nil, err
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		return nil, err
	}
	if !lease.TaskApprovalCurrent(st, wu, cfg) {
		return nil, &lease.TaskApprovalError{IssueNumber: issue.Number}
	}
	if st.Branch == "" || pr.HeadSHA == "" || pr.BaseSHA == "" {
		return nil, fmt.Errorf("PR #%d lacks exact branch/head/base identity", number)
	}
	if headBranch(pr) != st.Branch {
		return nil, fmt.Errorf("PR #%d head branch differs from its linked issue", number)
	}
	if pr.IssueNumber != 0 && pr.IssueNumber != issue.Number {
		return nil, fmt.Errorf("PR #%d issue identity mismatch", number)
	}
	if provider, ok := s.Tracker.(interface{ UpstreamRepo() string }); ok {
		upstream := provider.UpstreamRepo()
		if pr.BaseRepo == "" || !strings.EqualFold(pr.BaseRepo, upstream) || pr.HeadRepo == "" {
			return nil, fmt.Errorf("PR repository identity differs from upstream %s", upstream)
		}
		if !strings.EqualFold(pr.HeadRepo, upstream) {
			if st.ForkRepo == "" || !strings.EqualFold(pr.HeadRepo, st.ForkRepo) {
				return nil, fmt.Errorf("fork PR head repository differs from authoritative issue fork repository")
			}
		} else if pr.Head != st.Branch {
			return nil, fmt.Errorf("same-repository PR head must match linked branch")
		}
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve PR workspace root: %w", err)
	}
	return &Result{PR: pr, IssueNumber: issue.Number, State: st, Worktree: workspace.PRWorktreePath(root, number), Branch: workspace.PRBranchName(number), HeadSHA: pr.HeadSHA, BaseSHA: st.BaseSHA, Status: "open"}, nil
}

// codeScanning reads open alerts for the PR merge ref. Backends without the
// capability report none; any reader error becomes the fixed unavailable
// report so raw API text never reaches output and never fails the command.
func (s *Service) codeScanning(ctx context.Context, number int) *tracker.CodeScanningEvidence {
	evidence := tracker.CodeScanningEvidence{Ref: tracker.CodeScanningMergeRef(number)}
	if reader, ok := s.Tracker.(tracker.CodeScanningEvidenceReader); ok {
		got, err := reader.GetCodeScanningAlerts(ctx, number)
		if err != nil {
			got = tracker.CodeScanningEvidence{Unavailable: true}
		}
		got.Ref = evidence.Ref
		evidence = got
	}
	evidence = evidence.Normalize()
	return &evidence
}

func (s *Service) Status(ctx context.Context, number int) (*Result, error) {
	r, err := s.linkedInspection(ctx, number)
	if err != nil {
		return nil, err
	}
	r.RoleRuns = lease.RoleRuns(r.State)
	if r.State != nil {
		r.Repair = validation.RepairFromArtifact(r.State.Artifacts, r.State.BaseSHA)
	}
	if r.Status == "merged" || r.Status == "closed-unmerged" {
		r.Worktree = ""
		return r, nil
	}
	if _, statErr := os.Lstat(r.Worktree); os.IsNotExist(statErr) {
		r.Worktree = ""
	} else if statErr != nil {
		return nil, statErr
	} else {
		head, dirty, rebasing, e := workspace.StatusPRHead(ctx, r.Worktree)
		if e != nil {
			return nil, fmt.Errorf("inspect managed PR worktree: %w", e)
		}
		if r.State.PRRevision != nil && head != r.State.PRRevision.LocalHeadSHA && !rebasing {
			return nil, fmt.Errorf("local PR head differs from recorded checkpoint")
		}
		r.HeadSHA = head
		r.Dirty, r.Rebasing = dirty, rebasing
		if r.Status != StatusDiverged && r.Status != StatusUnverifiedHead {
			if rebasing {
				r.Status = "rebasing"
			} else if dirty {
				r.Status = "dirty"
			}
		}
	}
	s.diagnoseLocalDroppedPaths(ctx, r)
	r.Checks, err = s.Tracker.GetCheckStatus(ctx, r.PR.HeadSHA)
	if err != nil {
		return r, err
	}
	for _, c := range r.Checks {
		if tracker.CurrentCIFailure(c, r.PR.HeadSHA) {
			r.CIFailures = true
		}
	}
	if len(r.Checks) == 0 {
		r.Checks = recordedValidationChecks(r.State, r.PR.HeadSHA)
	}
	r.CodeScanning = s.codeScanning(ctx, number)
	if reviewer, ok := s.Tracker.(interface {
		GetPullRequestReviews(context.Context, int) ([]tracker.PullRequestReview, error)
	}); ok {
		r.Reviews, err = reviewer.GetPullRequestReviews(ctx, number)
		if err != nil {
			return r, err
		}
		for i := range r.Reviews {
			r.Reviews[i].Current = r.Reviews[i].CommitSHA != "" && r.Reviews[i].CommitSHA == r.PR.HeadSHA
		}
	}
	return r, nil
}

// contributorRemote selects the contributor's own fork as the publish target in
// --fork mode. The fork must be the one the issue recorded for this PR; the
// upstream coordinator re-verifies ownership before any state changes.
func (s *Service) contributorRemote(r *Result) (string, error) {
	provider, ok := s.Tracker.(interface{ UpstreamRepo() string })
	if !ok {
		return "", fmt.Errorf("--fork PR revisions require the github backend")
	}
	if strings.EqualFold(r.PR.HeadRepo, provider.UpstreamRepo()) || r.State == nil || !strings.EqualFold(r.PR.HeadRepo, r.State.ForkRepo) {
		return "", fmt.Errorf("--fork can only revise a PR whose head is in the contributor fork recorded for issue #%d", r.IssueNumber)
	}
	upstream := s.upstreamRemote()
	if s.ForkRemote == "" || strings.EqualFold(s.ForkRemote, upstream) {
		return "", fmt.Errorf("--fork PR revisions require --fork-remote naming your fork %s", r.PR.HeadRepo)
	}
	slug, err := workspace.ResolveForkRemoteSlug(s.Root, upstream, s.ForkRemote)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(slug, r.PR.HeadRepo) {
		return "", fmt.Errorf("fork remote %s is %s, but PR #%d's head is in %s", s.ForkRemote, slug, r.PR.Number, r.PR.HeadRepo)
	}
	return s.ForkRemote, nil
}

// PublishVisibilityTimeout bounds how long pr submit waits for GitHub to report
// a pushed head on the PR; the API updates a PR's head shortly after the push.
var (
	PublishVisibilityTimeout = 30 * time.Second
	publishPollInterval      = 2 * time.Second
)

// awaitPublishedHead re-reads the PR until it reports head, stops changing for
// another reason, or PublishVisibilityTimeout passes.
func (s *Service) awaitPublishedHead(ctx context.Context, number int, head string) (*tracker.PullRequest, error) {
	deadline := time.Now().Add(PublishVisibilityTimeout)
	for {
		observed, err := s.Tracker.GetPullRequest(ctx, number)
		if err != nil || observed == nil || observed.HeadSHA == head || !strings.EqualFold(observed.State, "open") || observed.Merged || !time.Now().Before(deadline) {
			return observed, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(publishPollInterval):
		}
	}
}

// RecordedValidationCheck names the check synthesized from SwarmOS's own
// validation evidence when the tracker reports no CI checks (local backend, or
// a repository without CI).
const RecordedValidationCheck = "swarm-validation"

func recordedValidationChecks(st *lease.IssueState, head string) []tracker.CheckRunResult {
	if st == nil || head == "" {
		return nil
	}
	for i := len(st.Artifacts) - 1; i >= 0; i-- {
		art := st.Artifacts[i]
		if art.Type != contract.ArtifactValidation || art.Metadata["head_sha"] != head {
			continue
		}
		conclusion := "failure"
		if strings.EqualFold(art.Verdict, "PASS") {
			conclusion = "success"
		}
		return []tracker.CheckRunResult{{Name: RecordedValidationCheck, Status: "completed", Conclusion: conclusion}}
	}
	return nil
}

// upstreamRemote names the project remote holding approved bases and
// same-repository PR heads.
func (s *Service) upstreamRemote() string {
	if strings.TrimSpace(s.UpstreamRemote) != "" {
		return strings.TrimSpace(s.UpstreamRemote)
	}
	if s.Manager != nil {
		return s.Manager.UpstreamRemote()
	}
	return workspace.DefaultUpstreamRemote
}

func (s *Service) remote(ctx context.Context, r *Result) (string, error) {
	if s.Fork {
		return s.contributorRemote(r)
	}
	remote := ""
	if provider, ok := s.Tracker.(interface{ UpstreamRepo() string }); ok {
		upstream := s.upstreamRemote()
		remote = upstream
		if !strings.EqualFold(r.PR.HeadRepo, provider.UpstreamRepo()) {
			if s.ForkRemote == "" || strings.EqualFold(s.ForkRemote, upstream) {
				return "", fmt.Errorf("fork PR head requires an explicit writable remote matching %s", r.PR.HeadRepo)
			}
			slug, err := workspace.ResolveForkRemoteSlug(s.Root, upstream, s.ForkRemote)
			if err != nil {
				return "", err
			}
			if !strings.EqualFold(slug, r.PR.HeadRepo) {
				return "", fmt.Errorf("configured head remote %s does not match PR head repository %s", slug, r.PR.HeadRepo)
			}
			remote = s.ForkRemote
		}
		if err := s.Tracker.Authorize(ctx, "approve-task"); err != nil {
			return "", fmt.Errorf("upstream write authorization required: %w", err)
		}
	}
	return remote, nil
}

// withClaim serializes the issue across CLI and PR commands and binds the exact
// persisted token before any fenced operation. A completed issue is re-claimed
// for PR work; an active issue can only be resumed by its original owner.
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// withClaim runs fn under the issue's fenced claim. Long-running validation and
// publication pass renew=true so the lease is kept alive until fn returns; the
// workflow engine renews its own claim during pr work.
func (s *Service) withClaim(ctx context.Context, r *Result, renew bool, reporter *workflow.ProgressReporter, fn func(context.Context, string, *lease.IssueState) (*Result, error)) (*Result, error) {
	lock, err := lease.LockClaim(s.Root, r.IssueNumber)
	if err != nil {
		return nil, err
	}
	defer lease.UnlockClaim(lock)
	owner, err := s.Tracker.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	if owner == "" {
		return nil, fmt.Errorf("authenticated claim owner required")
	}
	st, err := s.Manager.GetCurrentState(ctx, r.IssueNumber)
	if err != nil {
		return nil, err
	}
	if st.PRNumber != r.PR.Number || (st.PRRevision != nil && st.PRRevision.RemoteHeadSHA != r.PR.HeadSHA && st.PRRevision.LocalHeadSHA != r.PR.HeadSHA) {
		return nil, fmt.Errorf("PR coordination state changed")
	}
	cfg, err := harness.LoadConfigAt(s.Root, st.BaseSHA)
	if err != nil {
		return nil, err
	}
	duration, err := time.ParseDuration(cfg.Lease.Duration)
	if err != nil {
		return nil, err
	}
	if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(s.now()) {
		token, err := lease.LoadClaimToken(s.Root, r.IssueNumber)
		if err != nil {
			return nil, fmt.Errorf("load active claim: %w", err)
		}
		if token == "" || st.Lease.ClaimID != token {
			return nil, lease.ErrClaimFence
		}
		err = reporter.ReportSetupPhase(ctx, r.IssueNumber, "claim", func() error { st, err = s.Manager.Resume(ctx, r.IssueNumber, owner, token); return err })
		if err != nil {
			return nil, err
		}
	} else {
		err = reporter.ReportSetupPhase(ctx, r.IssueNumber, "claim", func() error {
			st, err = s.Manager.AcquirePR(ctx, r.IssueNumber, r.PR.Number, owner, cfg.DefaultAgent, "all", duration, cfg.Lease.MaxRenewals)
			return err
		})
		if err != nil {
			return nil, err
		}
		if err = lease.SaveClaimToken(s.Root, r.IssueNumber, st.Lease.ClaimID, st.BaseSHA); err != nil {
			return nil, err
		}
	}
	if !renew {
		return fn(ctx, owner, st)
	}
	heartbeat := s.Manager.StartHeartbeat(ctx, r.IssueNumber, owner, duration)
	defer heartbeat.Stop()
	result, err := fn(heartbeat.Context(), owner, st)
	if renewErr := heartbeat.Err(); renewErr != nil {
		return nil, errors.Join(err, fmt.Errorf("claim renewal failed: %w", renewErr))
	}
	return result, err
}

// withCleanPRMarkers runs only after PreparePR has verified the managed worktree.
// Remove temporary instruction pointers before inspecting cleanliness and again
// after execution; unrelated edits remain visible to PRHead and Git.
func withCleanPRMarkers(path string, run func() (*Result, error)) (result *Result, err error) {
	if cleanErr := workspace.CleanEphemeralHarnessMarkers(path); cleanErr != nil {
		return nil, fmt.Errorf("clean PR harness markers: %w", cleanErr)
	}
	defer func() {
		if cleanErr := workspace.CleanEphemeralHarnessMarkers(path); cleanErr != nil {
			err = errors.Join(err, fmt.Errorf("clean PR harness markers: %w", cleanErr))
		}
	}()
	return run()
}

// Result statuses for a PR whose remote head no longer matches what SwarmOS
// recorded. Unverified heads carry commits pushed outside SwarmOS since the
// last verified submission; the next pr work revision re-plans and re-validates
// from that head.
const (
	StatusDiverged         = "diverged"
	StatusPublishedPending = "published-pending-reconciliation"
	StatusUnverifiedHead   = "unverified-head"
)

// openForMutation resolves the linked PR and its write remote. A PR whose
// remote head moved outside the recorded revision is always refused; one whose
// publication awaits reconciliation is refused unless the caller reconciles it.
func (s *Service) openForMutation(ctx context.Context, number int, reconciles bool) (*Result, string, error) {
	r, err := s.linked(ctx, number)
	if err != nil {
		return nil, "", err
	}
	if !strings.EqualFold(r.PR.State, "open") || r.PR.Merged {
		return nil, "", fmt.Errorf("PR #%d must be open and unmerged", number)
	}
	if r.Status == StatusPublishedPending && !reconciles {
		return nil, "", fmt.Errorf("PR head has been published; run swarm pr submit %d to reconcile state", number)
	}
	if r.Status == StatusDiverged {
		return nil, "", fmt.Errorf("PR remote head changed outside the recorded revision; preserve local work and reconcile the PR before mutation")
	}
	remote, err := s.remote(ctx, r)
	if err != nil {
		return nil, "", err
	}
	return r, remote, nil
}

// preparePR checks out the managed PR worktree at localHead and verifies it is
// the worktree recorded for this PR.
func (s *Service) preparePR(ctx context.Context, r *Result, remote, localHead string, requireBaseAncestor bool) (string, string, error) {
	baseRemote := ""
	if remote != "" && remote != s.upstreamRemote() {
		if _, ok := s.Tracker.(interface{ UpstreamRepo() string }); ok {
			baseRemote = s.upstreamRemote()
		}
	}
	return workspace.PreparePR(ctx, s.Root, workspace.PRWorkspaceOptions{Number: r.PR.Number, Remote: remote, BaseRemote: baseRemote, HeadBranch: headBranch(r.PR), HeadSHA: r.PR.HeadSHA, BaseSHA: r.BaseSHA, LocalHeadSHA: localHead, AllowHarnessMarkers: true, RequireBaseAncestor: requireBaseAncestor})
}

// requireApproval re-reads the issue and requires the claimed state to still
// approve its current WorkUnit and plan.
func (s *Service) requireApproval(ctx context.Context, r *Result, st *lease.IssueState) (*contract.WorkUnit, error) {
	issue, err := s.Tracker.GetIssue(ctx, r.IssueNumber)
	if err != nil {
		return nil, err
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		return nil, err
	}
	if err = lease.VerifyApproval(st, wu); err != nil {
		return nil, fmt.Errorf("PR plan approval required: %w", err)
	}
	return wu, nil
}

func (s *Service) Work(ctx context.Context, opts Options) (*Result, error) {
	r, remote, err := s.openForMutation(ctx, opts.Number, false)
	if err != nil {
		return nil, err
	}
	if err = s.checkWork(ctx, r); err != nil {
		return r, err
	}
	if len(r.WorkPreflight.Blockers) > 0 {
		r.Status = "blocked"
		if opts.Work.InspectOnly {
			return r, nil
		}
		return r, r.WorkPreflight.stopError(r)
	}
	opts.Work.Version = s.Version
	opts.Work.RepoRoot = s.Root
	opts.Work.IssueNumber = r.IssueNumber
	opts.Work.PRNumber = opts.Number
	opts.Work.PRWorktree = r.Worktree
	opts.Work.Instructions = opts.Instructions
	if opts.Work.Phase == "" {
		opts.Work.Phase = "all"
	}
	opts.Work.FreshPRRevision = r.State.PRRevision == nil
	cfg, err := harness.LoadConfigAt(s.Root, r.BaseSHA)
	if err != nil {
		return nil, err
	}
	for _, override := range []struct{ role, name string }{{"planner", opts.Work.PlannerAgent}, {"reviewer", opts.Work.ReviewerAgent}, {"developer", opts.Work.DeveloperAgent}, {"tester", opts.Work.TesterAgent}} {
		if override.name != "" {
			if _, e := agent.PreflightWithOptions(override.name, cfg, s.Tracker.BackendName() == "local", override.role, s.runnerOptions(ctx)); e != nil {
				return nil, fmt.Errorf("%s agent preflight: %w", override.role, e)
			}
		}
	}
	engine := s.newEngine(cfg)
	engine.NewRunner = s.NewRunner
	pre, preflightErr := engine.Preflight(ctx, opts.Work)
	if preflightErr != nil {
		return nil, preflightErr
	}
	if opts.Work.InspectOnly {
		r.Preflight = pre
		if len(pre.Blockers) > 0 {
			r.Status = "blocked"
		} else {
			r.Status = "ready"
		}
		return r, nil
	}
	return s.withClaim(ctx, r, false, opts.Work.SetupProgress, func(ctx context.Context, owner string, st *lease.IssueState) (*Result, error) {
		var path, branch string
		e := opts.Work.SetupProgress.ReportSetupPhase(ctx, r.IssueNumber, "workspace", func() error {
			var setupErr error
			path, branch, setupErr = s.preparePR(ctx, r, remote, revisionHead(r.State), false)
			return setupErr
		})
		if e != nil {
			return nil, e
		}
		return withCleanPRMarkers(path, func() (*Result, error) {
			head, dirty, rebasing, e := workspace.PRHead(ctx, path)
			if e != nil {
				return nil, e
			}
			if rebasing {
				return nil, PausedRebaseError(ctx, path, opts.Number)
			}
			if dirty {
				return nil, fmt.Errorf("PR worktree must be clean and not rebasing before work")
			}
			st, e = s.Manager.BeginPRRevision(ctx, r.IssueNumber, owner, opts.Number, r.PR.HeadSHA, head, branch)
			if e != nil {
				return nil, e
			}
			opts.Work.FreshPRRevision = false
			opts.Work.Owner = owner
			opts.Work.ClaimID = st.Lease.ClaimID
			opts.Work.PRWorktree = path
			opts.Work.OnClaim = func(st *lease.IssueState) error {
				return lease.SaveClaimToken(s.Root, r.IssueNumber, st.Lease.ClaimID, st.BaseSHA)
			}
			checks, ciErr := s.Tracker.GetCheckStatus(ctx, r.PR.HeadSHA)
			if ciErr != nil {
				return nil, ciErr
			}
			opts.Work.CIEvidence = nil
			for _, c := range checks {
				if tracker.CurrentCIFailure(c, r.PR.HeadSHA) {
					opts.Work.CIEvidence = append(opts.Work.CIEvidence, tracker.CheckEvidence{Check: c})
				}
			}
			// Reserve metadata before downloading any diagnostic payload.
			if _, ciErr = workspace.RenderCIEvidence(opts.Work.CIEvidence); ciErr != nil {
				return nil, ciErr
			}
			sort.SliceStable(opts.Work.CIEvidence, func(i, j int) bool { return opts.Work.CIEvidence[i].Check.ID < opts.Work.CIEvidence[j].Check.ID })
			logBudget, annotationBudget := 128<<10, 64<<10
			for i, entry := range opts.Work.CIEvidence {
				evidence := tracker.CheckEvidence{Check: entry.Check, Unavailable: "Check details unavailable for this backend."}
				if reader, ok := s.Tracker.(tracker.CheckEvidenceReader); ok {
					evidence, ciErr = reader.GetCheckEvidence(ctx, entry.Check)
					if ciErr != nil {
						return nil, ciErr
					}
				}
				if len(evidence.LogTail) > logBudget {
					evidence.LogTail = evidence.LogTail[len(evidence.LogTail)-logBudget:]
					evidence.Truncated = true
				}
				logBudget -= len(evidence.LogTail)
				bounded := []string{}
				for _, text := range evidence.Annotations {
					if len(text) > annotationBudget {
						text = text[:annotationBudget]
						evidence.Truncated = true
					}
					if text != "" {
						bounded = append(bounded, text)
						annotationBudget -= len(text)
					}
				}
				evidence.Annotations = bounded
				opts.Work.CIEvidence[i] = evidence
			}
			// Alerts are read once per run, after the metadata preflight, and
			// attached to the lowest check ID so they render exactly once.
			r.CodeScanning = nil
			if len(opts.Work.CIEvidence) != 0 {
				r.CodeScanning = s.codeScanning(ctx, opts.Number)
				opts.Work.CIEvidence[0].CodeScanning = r.CodeScanning
			}
			evidence := tracker.ReviewEvidence{Unavailable: "Review evidence is unavailable for this backend."}
			if reader, ok := s.Tracker.(tracker.ReviewEvidenceReader); ok {
				evidence, e = reader.GetReviewEvidence(ctx, opts.Number)
				if e != nil {
					return nil, e
				}
			}
			opts.Work.ReviewEvidence = evidence
			worked, e := engine.ExecuteWork(ctx, opts.Work)
			if worked != nil {
				r.Work = worked
				r.State = worked.State
				r.HeadSHA = worked.HeadSHA
				r.Status = "pending"
				if worked.ClaimBudgetPause != nil || worked.SplitRecommended {
					r.Status = worked.Outcome()
				}
			}
			return r, e
		})
	})
}

func (s *Service) Rebase(ctx context.Context, opts Options) (*Result, error) {
	if opts.Continue && opts.Abort {
		return nil, fmt.Errorf("--continue and --abort are mutually exclusive")
	}
	if opts.Resolve && opts.Abort {
		return nil, fmt.Errorf("--resolve cannot be combined with --abort")
	}
	r, remote, err := s.openForMutation(ctx, opts.Number, false)
	if err != nil {
		return nil, err
	}
	if opts.Resolve && !s.Fork && r.State.RebaseApprovalAudit != "" {
		if _, err = s.Manager.ProjectRebaseApprovalAudit(ctx, r.IssueNumber); err != nil {
			return nil, err
		}
		r, remote, err = s.openForMutation(ctx, opts.Number, false)
		if err != nil {
			return nil, err
		}
	}
	if !opts.Abort && r.PR.BaseSHA != r.BaseSHA {
		if !opts.Resolve || s.Fork {
			return nil, &lease.TaskApprovalError{IssueNumber: r.IssueNumber, Err: fmt.Errorf("PR base tip differs from approved issue baseline: swarm issue approve %d then retry (release active claim first if needed)", r.IssueNumber)}
		}
		// Preflight before changing approval or Git state.
		earlyResolver, resolveErr := s.newResolver(ctx, r, opts)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if err = s.preflightRebaseReview(ctx, r, opts, earlyResolver); err != nil {
			return nil, err
		}
		if policyReviewNeeded(r.State, earlyResolver.unit) {
			return nil, fmt.Errorf("plan review required: swarm pr work %d --phase review", opts.Number)
		}
		if _, err = s.Manager.ReapproveRebase(ctx, r.IssueNumber, opts.Number, r.BaseSHA, r.PR.BaseSHA, r.PR.HeadSHA); err != nil {
			return nil, &lease.TaskApprovalError{IssueNumber: r.IssueNumber, Err: fmt.Errorf("routine reapproval: %w; run swarm issue approve %d", err, r.IssueNumber)}
		}
		r, remote, err = s.openForMutation(ctx, opts.Number, false)
		if err != nil {
			return nil, err
		}
	}
	if opts.Continue || opts.Abort {
		// Check before claiming so a mistaken --continue/--abort leaves no lease behind.
		if _, statErr := os.Lstat(r.Worktree); statErr != nil {
			return nil, fmt.Errorf("no managed PR rebase in progress")
		}
		if _, _, rebasing, e := workspace.PRHead(ctx, r.Worktree); e != nil || !rebasing {
			return nil, fmt.Errorf("no managed PR rebase in progress")
		}
	}
	if !opts.Abort && r.PR.BaseSHA != r.BaseSHA {
		return nil, &lease.TaskApprovalError{IssueNumber: r.IssueNumber, Err: fmt.Errorf("PR target moved during approval: swarm issue approve %d", r.IssueNumber)}
	}
	var rv *resolver
	if opts.Resolve {
		// Policy, agent selection, and the runner come from the approved base and
		// fail closed before any claim or rebase mutation.
		if rv, err = s.newResolver(ctx, r, opts); err != nil {
			return nil, err
		}
		if err = s.preflightRebaseReview(ctx, r, opts, rv); err != nil {
			return nil, err
		}
	}
	result, err := s.withClaim(ctx, r, opts.Resolve, nil, func(ctx context.Context, owner string, st *lease.IssueState) (*Result, error) {
		path, branch, e := s.preparePR(ctx, r, remote, revisionHead(r.State), false)
		if e != nil {
			return nil, e
		}
		return withCleanPRMarkers(path, func() (*Result, error) {
			head, dirty, rebasing, e := workspace.PRHead(ctx, path)
			if e != nil {
				return nil, e
			}
			if opts.Continue || opts.Abort {
				if !rebasing {
					return nil, fmt.Errorf("no managed PR rebase in progress")
				}
			} else if rebasing {
				return nil, &ConflictError{PRNumber: opts.Number, Paused: true, Err: fmt.Errorf("rebase already in progress; run swarm pr rebase %d --continue or --abort", opts.Number)}
			} else if dirty {
				return nil, fmt.Errorf("PR worktree must be clean before starting rebase")
			}
			if opts.Resolve {
				r.Worktree, r.Branch = path, branch
				st, e = s.reviewRebasePlan(ctx, r, owner, st, opts, rv)
				if e != nil {
					return r, e
				}
				r.State = st
			}
			if opts.Resolve && !rebasing && st.PRRevision != nil && st.PRRevision.Intent == "rebase" && st.LastRebase != nil && st.LastRebase.Status == "completed" && st.LastRebase.NewBase == st.BaseSHA && st.LastRebase.NewHead == head {
				r.State = st
				r.HeadSHA = head
				return s.finishResolvedRebase(ctx, r, owner, rv)
			}
			if !rebasing {
				st, e = s.Manager.BeginPRRevision(ctx, r.IssueNumber, owner, opts.Number, r.PR.HeadSHA, head, branch, "rebase")
				if e != nil {
					return nil, e
				}
			}
			if !opts.Abort {
				if st, e = s.Manager.InvalidatePRRevisionEvidence(ctx, r.IssueNumber, owner, opts.Number, "rebase"); e != nil {
					return nil, e
				}
			}
			// A resolution is recorded only after Git has moved past its commit.
			var pending *lease.RebaseResolutionReport
			pause := func(cause error) (*Result, error) {
				r.Status = "rebasing"
				conflicts, readErr := workspace.RebaseConflicts(ctx, path)
				if readErr != nil {
					return r, fmt.Errorf("%w; read conflicts: %v", cause, readErr)
				}
				updated, recordErr := s.Manager.UpdateRebaseContext(ctx, r.IssueNumber, owner, opts.Number, lease.RebaseContext{ConflictedPaths: conflicts, Status: "in-progress", Resolution: pending})
				if recordErr != nil {
					return r, fmt.Errorf("%w; record conflicts: %v", cause, recordErr)
				}
				r.State = updated
				if rv != nil {
					cause = fmt.Errorf("%w\n%s", cause, resolveGuidance(opts.Number, conflicts))
				}
				if len(conflicts) > 0 {
					cause = &ConflictError{PRNumber: opts.Number, Paused: true, Manual: rv != nil, Err: cause}
				}
				return r, cause
			}
			if rv != nil && opts.Continue {
				// Resolve only what is still unmerged; hand-staged files stay as they are.
				res, resolveErr := s.resolveStop(ctx, rv, path, r.IssueNumber, owner, st)
				if resolveErr != nil {
					return pause(resolveErr)
				}
				pending = res
			}
			newHead, e := workspace.RebasePR(ctx, path, r.PR.BaseSHA, opts.Continue, opts.Abort)
			for e != nil && rv != nil {
				conflicts, readErr := workspace.RebaseConflicts(ctx, path)
				if readErr != nil || len(conflicts) == 0 {
					// Not a conflict stop: leave Git's state for the operator.
					pending = nil
					break
				}
				updated, recordErr := s.Manager.UpdateRebaseContext(ctx, r.IssueNumber, owner, opts.Number, lease.RebaseContext{ConflictedPaths: conflicts, Status: "in-progress", Resolution: pending})
				if recordErr != nil {
					return r, fmt.Errorf("%w; record conflicts: %v", e, recordErr)
				}
				r.State, st, pending = updated, updated, nil
				res, resolveErr := s.resolveStop(ctx, rv, path, r.IssueNumber, owner, st)
				if resolveErr != nil {
					return pause(resolveErr)
				}
				pending = res
				newHead, e = workspace.RebasePR(ctx, path, r.PR.BaseSHA, true, false)
			}
			if e != nil {
				return pause(e)
			}
			if newHead != "" {
				st, e = s.Manager.RecordPRHead(ctx, r.IssueNumber, owner, opts.Number, branch, newHead)
				if e != nil {
					return nil, e
				}
				r.State = st
				r.HeadSHA = newHead
			}
			status := "completed"
			if opts.Abort {
				status = "aborted"
			}
			st, e = s.Manager.UpdateRebaseContext(ctx, r.IssueNumber, owner, opts.Number, lease.RebaseContext{Status: status, Resolution: pending})
			if e != nil {
				return r, e
			}
			r.State = st
			r.Status = "pending"
			r.Worktree, r.Branch = path, branch
			if opts.Resolve {
				return s.finishResolvedRebase(ctx, r, owner, rv)
			}
			return r, nil
		})
	})
	s.diagnoseLocalDroppedPaths(ctx, result)
	return result, err
}

// newResolver selects and preflights the developer agent from the approved
// base's policy. Nothing here reads the PR worktree or a mid-rebase tree.
func (s *Service) newResolver(ctx context.Context, r *Result, opts Options) (*resolver, error) {
	cfg, err := harness.LoadConfigAt(s.Root, r.BaseSHA)
	if err != nil {
		return nil, err
	}
	spec, err := harness.LoadValidationSpecAt(s.Root, r.BaseSHA)
	if err != nil {
		return nil, err
	}
	local := s.Tracker.BackendName() == "local"
	name := workflow.ResolveRoleAgent(cfg, "developer", opts.Work)
	if name == "" {
		return nil, fmt.Errorf("no configured developer agent")
	}
	if _, err := agent.PreflightWithOptions(name, cfg, local, "developer", s.runnerOptions(ctx)); err != nil {
		return nil, fmt.Errorf("developer agent preflight: %w", err)
	}
	newRunner := s.NewRunner
	if newRunner == nil {
		newRunner = func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error) {
			return agent.NewConfiguredRunnerWithOptions(name, cfg, local, s.runnerOptions(ctx))
		}
	}
	runner, err := newRunner(name, cfg, local)
	if err != nil {
		return nil, fmt.Errorf("developer agent preflight: %w", err)
	}
	issue, err := s.Tracker.GetIssue(ctx, r.IssueNumber)
	if err != nil {
		return nil, err
	}
	unit, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		return nil, err
	}
	mode := "headless"
	if !opts.Work.Headless {
		mode = "interactive"
	}
	deps, err := s.newEngine(cfg).AgentDependencies(ctx, cfg, s.Root, r.BaseSHA)
	if err != nil {
		return nil, err
	}
	return &resolver{runner: runner, agentName: name, mode: mode, local: local, unit: unit, protected: spec.ProtectedPaths, log: s.ResolveLog, deps: deps}, nil
}

func (s *Service) Validate(ctx context.Context, number int) (result *Result, runErr error) {
	p := s.Progress
	if p == nil {
		p = progress.Start(ctx, 0, number, s.OnProgress)
		defer func() {
			status := "completed"
			if runErr != nil {
				status = "failed"
			}
			p.Finish(status)
		}()
	}

	r, remote, err := s.openForMutation(ctx, number, true)
	if err != nil {
		return nil, err
	}
	return s.validate(ctx, r, remote, p, false, true)
}

// validate runs required checks on the pending revision under the renewed claim
// and records the report as that revision's validation evidence. fastPass runs
// changed-package tests first; only explicit pr validate requests it.
func (s *Service) validate(ctx context.Context, r *Result, remote string, p *progress.Session, reuse, fastPass bool) (*Result, error) {
	number := r.PR.Number
	if r.State.PRRevision == nil {
		return nil, fmt.Errorf("no pending PR revision to validate")
	}
	// Inspect only an existing worktree: a paused rebase must finish first.
	if _, _, rebasing, err := workspace.InspectPRHead(ctx, r.Worktree); err == nil && rebasing {
		return nil, PausedRebaseError(ctx, r.Worktree, number)
	}
	return s.withClaim(ctx, r, true, nil, func(ctx context.Context, owner string, st *lease.IssueState) (*Result, error) {
		if st.PRRevision == nil || st.PRRevision.Number != number {
			return nil, fmt.Errorf("pending PR revision changed during validation")
		}
		path, branch, e := s.preparePR(ctx, r, remote, st.PRRevision.LocalHeadSHA, true)
		if e != nil {
			return nil, e
		}
		if path != r.Worktree || branch != st.PRRevision.Branch {
			return nil, fmt.Errorf("PR worktree identity differs from recorded revision")
		}
		return withCleanPRMarkers(path, func() (*Result, error) {
			return s.validateClaim(ctx, r, owner, st, p, reuse, fastPass)

		})
	})
}

// validateClaim shares the existing fenced claim with rebase and repair.
func (s *Service) validateClaim(ctx context.Context, r *Result, owner string, st *lease.IssueState, p *progress.Session, reuse, fastPass bool) (*Result, error) {
	head, dirty, rebasing, e := workspace.PRHead(ctx, r.Worktree)
	if e != nil {
		return nil, e
	}
	if rebasing {
		return nil, PausedRebaseError(ctx, r.Worktree, r.PR.Number)
	}
	if dirty || head != st.PRRevision.LocalHeadSHA {
		return nil, fmt.Errorf("PR worktree is dirty, rebasing, or differs from recorded checkpoint")
	}
	wu, e := s.requireApproval(ctx, r, st)
	if e != nil {
		return nil, e
	}
	if e = restoreApprovedPlan(r.Worktree, st); e != nil {
		return r, e
	}
	var observe func(progress.Event)
	if p != nil {
		p.Next("validation")
		observe = p.Observe
	}
	opts := validation.ValidateOptions{Version: s.Version, ClaimID: st.Lease.ClaimID, BaseSHA: st.BaseSHA, OnProgress: observe, ForkMode: s.Fork || s.Manager.HasWorkerGateway(), GoFastPass: fastPass, LookupEnv: s.LookupEnv, DependencyMode: s.dependencyMode()}
	var report *validation.ValidationReport
	var valErr error
	if reuse {
		report, valErr = validation.Reuse(ctx, r.Worktree, opts, owner, st.Artifacts)
	}
	if report == nil && valErr == nil {
		report, valErr = validation.Validate(ctx, r.Worktree, wu, opts)
	}
	checkedHead, changed, activeRebase, checkErr := workspace.PRHead(ctx, r.Worktree)
	if checkErr != nil {
		return nil, checkErr
	}
	if changed || activeRebase || checkedHead != head {
		return nil, fmt.Errorf("PR candidate changed during validation")
	}
	if report != nil {
		report.Artifact.Metadata = lease.TesterMetadata(st, report.Artifact.Metadata)
		lease.PreserveTesterGuidance(st, &report.Artifact)
		r.Validation = report
		st, e = s.Manager.RecordTrustedValidation(ctx, r.IssueNumber, owner, report.Artifact)
		if e != nil {
			return r, e
		}
		r.State = st
	}
	r.HeadSHA = head
	r.Status = "pending"
	return r, valErr
}

func (s *Service) Submit(ctx context.Context, number int) (result *Result, runErr error) {
	p := s.Progress
	if p == nil {
		p = progress.Start(ctx, 0, number, s.OnProgress)
		defer func() {
			status := "completed"
			if runErr != nil {
				status = "failed"
			}
			p.Finish(status)
		}()
	}

	r, remote, err := s.openForMutation(ctx, number, true)
	if err != nil {
		return nil, err
	}
	if r.State.PRRevision == nil {
		if r.PR.HeadSHA != r.State.HeadSHA {
			return nil, fmt.Errorf("PR head changed since the last verified submission; run swarm pr work %d before submitting", number)
		}
		r.Status = "submitted"
		if !s.Fork {
			p.Next("review evidence update")
			if e := ProjectReviewEvidence(ctx, s.Tracker, r.State, number); e != nil {
				return r, fmt.Errorf("PR submitted; retry swarm pr submit %d to update review evidence: %w", number, e)
			}
			p.Next("rebase metadata update")
			if e := ProjectRebase(ctx, s.Tracker, r.State, number); e != nil {
				return r, fmt.Errorf("PR submitted; retry swarm pr submit %d to update rebase context: %w", number, e)
			}
		}
		return r, nil
	}
	if err = s.refuseUnimplementedRevision(ctx, r); err != nil {
		return r, err
	}
	r, err = s.validate(ctx, r, remote, p, true, false)
	if err != nil {
		return r, err
	}
	// Validation acquired the issue lock and released it; acquire again for the
	// final head comparison, exact-old-head push, and coordination CAS.
	return s.withClaim(ctx, r, false, nil, func(ctx context.Context, owner string, st *lease.IssueState) (*Result, error) {
		rev := st.PRRevision
		if rev == nil || (rev.RemoteHeadSHA != r.PR.HeadSHA && rev.LocalHeadSHA != r.PR.HeadSHA) {
			return nil, fmt.Errorf("PR revision changed during validation")
		}
		head, dirty, rebasing, e := workspace.PRHead(ctx, r.Worktree)
		if e != nil {
			return nil, e
		}
		if dirty || rebasing || head != rev.LocalHeadSHA || r.Validation == nil || !r.Validation.Passed || r.Validation.HeadSHA != head {
			return nil, fmt.Errorf("PR candidate changed after validation")
		}
		if _, e = s.requireApproval(ctx, r, st); e != nil {
			return nil, e
		}
		fresh, e := s.Tracker.GetPullRequest(ctx, number)
		if e != nil {
			return nil, e
		}
		if fresh == nil || !strings.EqualFold(fresh.State, "open") || fresh.Merged || fresh.Head != r.PR.Head || fresh.HeadRepo != r.PR.HeadRepo || fresh.Base != r.PR.Base {
			// The target branch may advance; the revision is validated against
			// the approved base and GitHub tests the merge. A retarget is not.
			return nil, fmt.Errorf("PR changed remotely before publish; refusing stale head")
		}
		if fresh.HeadSHA != head {
			if fresh.HeadSHA != rev.RemoteHeadSHA {
				return nil, fmt.Errorf("PR head moved concurrently; refusing stale publish")
			}
			if remote == s.upstreamRemote() {
				p.Next("publish/submission record")
				st, e = s.Manager.CompletePRRevision(ctx, r.IssueNumber, owner, number, rev.RemoteHeadSHA, head)
				if e != nil {
					return nil, e
				}
			} else {
				if e = s.Manager.CheckClaim(ctx, r.IssueNumber, owner); e != nil {
					return nil, e
				}
				p.Next("publish")
				if remote == "" {
					local, ok := s.Tracker.(*tracker.LocalTracker)
					if !ok {
						return nil, fmt.Errorf("local PR publication requires local tracker")
					}
					e = local.PublishPRHead(ctx, number, rev.RemoteHeadSHA, head)
				} else {
					e = workspace.PublishPR(ctx, s.Root, remote, headBranch(fresh), rev.RemoteHeadSHA, head)
				}
				if e != nil {
					return nil, e
				}
				observed, observeErr := s.awaitPublishedHead(ctx, number, head)
				if observeErr != nil {
					return nil, observeErr
				}
				if observed == nil || observed.HeadSHA != head || !strings.EqualFold(observed.State, "open") || observed.Merged {
					return nil, fmt.Errorf("PR head or state changed after publication; coordination remains pending (rerun swarm pr submit %d to reconcile)", number)
				}
				p.Next("submission record")
				st, e = s.Manager.CompletePRRevisionLocal(ctx, r.IssueNumber, owner, number, rev.RemoteHeadSHA, head)
				if e != nil {
					return nil, fmt.Errorf("PR head published but state reconciliation is required: %w", e)
				}
			}
		} else {
			// Retry after a local or fork head update succeeded but its state CAS failed.
			p.Next("submission record")
			st, e = s.Manager.CompletePRRevisionLocal(ctx, r.IssueNumber, owner, number, rev.RemoteHeadSHA, head)
			if e != nil {
				return nil, e
			}
		}
		r.PR.HeadSHA = head
		r.State = st
		r.HeadSHA = head
		r.Status = "submitted"
		if !s.Fork {
			p.Next("review evidence update")
			if e := ProjectReviewEvidence(ctx, s.Tracker, st, number); e != nil {
				return r, fmt.Errorf("PR submitted; retry swarm pr submit %d to update review evidence: %w", number, e)
			}
			p.Next("rebase metadata update")
			if e := ProjectRebase(ctx, s.Tracker, st, number); e != nil {
				return r, fmt.Errorf("PR submitted; retry swarm pr submit %d to update rebase context: %w", number, e)
			}
		}
		return r, nil
	})
}

// refuseUnimplementedRevision stops submit of a development revision whose
// approved plan has no implementation yet: the recorded, published, and
// worktree heads are unchanged and no passing implementation is bound to them.
// Completing it would consume the plan, so this check reads state only and
// runs before any claim or fenced write.
func (s *Service) refuseUnimplementedRevision(ctx context.Context, r *Result) error {
	rev := r.State.PRRevision
	if rev == nil || rev.Intent == "rebase" || rev.RemoteHeadSHA != rev.LocalHeadSHA || r.PR.HeadSHA != rev.LocalHeadSHA {
		return nil
	}
	hasPlan := false
	for _, art := range r.State.Artifacts {
		switch {
		case art.Type == contract.ArtifactDesign:
			hasPlan = true
		case art.Type == contract.ArtifactImplementation && art.Verdict == "PASS" && art.Metadata["head_sha"] == rev.LocalHeadSHA && implementationBound(r.State, art):
			return nil
		}
	}
	if !hasPlan {
		return nil
	}
	if _, err := s.requireApproval(ctx, r, r.State); errors.Is(err, lease.ErrApprovalRequired) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Lstat(r.Worktree); err == nil {
		head, dirty, rebasing, e := workspace.PRHead(ctx, r.Worktree)
		if e != nil {
			return fmt.Errorf("inspect managed PR worktree: %w", e)
		}
		// Other candidates keep the existing checkpoint rejection.
		if dirty || rebasing || head != rev.LocalHeadSHA {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	command := "swarm"
	if s.Fork {
		command += " --fork"
	}
	return fmt.Errorf("PR #%d has no new implementation to publish; its approved revision plan is kept. Run %s pr work %d --phase implement", r.PR.Number, command, r.PR.Number)
}

// implementationBound reports whether an implementation artifact was produced
// for the current approved plan and task.
func implementationBound(st *lease.IssueState, art contract.Artifact) bool {
	for key, value := range lease.ReviewBindings(st) {
		if key != "review_claim_id" && (value == "" || art.Metadata[key] != value) {
			return false
		}
	}
	return true
}

func (s *Service) runnerOptions(ctx context.Context) agent.RunnerOptions {
	return agent.RunnerOptions{LookupEnv: s.LookupEnv, Inspections: sandbox.InspectionSessionFrom(ctx)}
}

// dependencyMode is the node profile's dependency cache mode. A malformed
// profile keeps the default per-repository caches.
func (s *Service) dependencyMode() sandbox.DependencyCacheMode {
	mode, _ := node.DependencyCacheMode(s.NodeProfilePath)
	return mode
}

func (s *Service) newEngine(cfg *contract.SwarmConfig) *workflow.Engine {
	return workflow.NewEngineWithManagerOptions(s.Root, s.Tracker, s.Manager, cfg, false, "", workflow.EngineOptions{LookupEnv: s.LookupEnv, NodeProfilePath: s.NodeProfilePath})
}
