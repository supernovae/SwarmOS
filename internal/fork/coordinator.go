package fork

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	prservice "github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"

	"github.com/supernovae/swarmos/internal/gitcredential"
)

type IssueCommentEvent struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue struct {
		Number      int            `json:"number"`
		State       string         `json:"state,omitempty"`
		PullRequest map[string]any `json:"pull_request,omitempty"`
	} `json:"issue"`
	Comment struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"comment"`
}

func LoadEventFromFile(path string) (*IssueCommentEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read github event file: %w", err)
	}
	var ev IssueCommentEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("parse github event JSON: %w", err)
	}
	return &ev, nil
}

// actionTracker wraps an upstream Tracker so that CurrentUser returns the
// API-verified event actor and Authorize denies all maintainer operations.
type actionTracker struct {
	tracker.Tracker
	actor string
}

func (a *actionTracker) CurrentUser(_ context.Context) (string, error) {
	if strings.TrimSpace(a.actor) == "" {
		return "", fmt.Errorf("empty verified event actor")
	}
	return a.actor, nil
}

func (a *actionTracker) Authorize(_ context.Context, permission string) error {
	return fmt.Errorf("fork coordinator denies maintainer operation %q for %q", permission, a.actor)
}

type Coordinator struct {
	RepoRoot        string
	UpstreamRepo    string
	Tracker         tracker.Tracker
	NowFn           func() time.Time
	ForkGitURLFunc  func(forkRepo string) string
	VerifyForkFunc  func(ctx context.Context, forkSlug, expectedOwner string) (string, error)
	DefaultBaseFunc func(repoRoot string) (string, error)
}

func (c *Coordinator) now() time.Time {
	if c.NowFn != nil {
		return c.NowFn()
	}
	return time.Now()
}

func (c *Coordinator) forkGitURL(forkRepo string) string {
	if c.ForkGitURLFunc != nil {
		return c.ForkGitURLFunc(forkRepo)
	}
	host := strings.TrimSpace(os.Getenv("GH_HOST"))
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("https://%s/%s.git", host, strings.TrimSpace(forkRepo))
}

func (c *Coordinator) verifyForkRepo(ctx context.Context, forkSlug, expectedOwner string) (string, error) {
	if c.VerifyForkFunc != nil {
		return c.VerifyForkFunc(ctx, forkSlug, expectedOwner)
	}
	if gh, ok := c.Tracker.(*tracker.GitHubTracker); ok {
		return gh.VerifyForkRepository(ctx, forkSlug, expectedOwner, false)
	}
	return "", fmt.Errorf("tracker does not support fork repository verification")
}

func (c *Coordinator) verifyEventAndComment(ctx context.Context, ev *IssueCommentEvent) (*tracker.Comment, string, error) {
	if ev == nil {
		return nil, "", fmt.Errorf("nil event")
	}
	if ev.Action != "created" {
		return nil, "", fmt.Errorf("unsupported issue_comment action %q (only \"created\" is accepted)", ev.Action)
	}
	if ev.Issue.PullRequest != nil {
		return nil, "", fmt.Errorf("pull request comments are not accepted by fork coordinator")
	}
	if c.UpstreamRepo != "" && !strings.EqualFold(strings.TrimSpace(ev.Repository.FullName), strings.TrimSpace(c.UpstreamRepo)) {
		return nil, "", fmt.Errorf("event repository %q does not match upstream %q", ev.Repository.FullName, c.UpstreamRepo)
	}
	if ev.Issue.Number <= 0 || ev.Comment.ID <= 0 {
		return nil, "", fmt.Errorf("invalid issue number or comment ID in event")
	}
	if !ev.Comment.CreatedAt.IsZero() && !ev.Comment.UpdatedAt.IsZero() && !ev.Comment.CreatedAt.Equal(ev.Comment.UpdatedAt) {
		return nil, "", fmt.Errorf("edited fork request comments are rejected")
	}

	iss, err := c.Tracker.GetIssue(ctx, ev.Issue.Number)
	if err != nil || iss == nil {
		return nil, "", fmt.Errorf("verify issue #%d: %w", ev.Issue.Number, err)
	}
	if !strings.EqualFold(iss.State, "open") {
		return nil, "", fmt.Errorf("issue #%d is not open", ev.Issue.Number)
	}

	var apiComment *tracker.Comment
	if getter, ok := c.Tracker.(interface {
		GetComment(context.Context, int64) (*tracker.Comment, error)
	}); ok {
		apiComment, err = getter.GetComment(ctx, ev.Comment.ID)
		if err != nil {
			return nil, "", fmt.Errorf("verify comment %d via API: %w", ev.Comment.ID, err)
		}
	} else {
		comments, listErr := c.Tracker.ListComments(ctx, ev.Issue.Number)
		if listErr != nil {
			return nil, "", fmt.Errorf("list comments for #%d: %w", ev.Issue.Number, listErr)
		}
		for _, item := range comments {
			if item.ID == ev.Comment.ID {
				apiComment = item
				break
			}
		}
		if apiComment == nil {
			return nil, "", fmt.Errorf("comment %d not found on issue #%d", ev.Comment.ID, ev.Issue.Number)
		}
	}

	if apiComment.ID != ev.Comment.ID {
		return nil, "", fmt.Errorf("comment ID mismatch")
	}
	if !apiComment.CreatedAt.IsZero() && !apiComment.UpdatedAt.IsZero() && !apiComment.CreatedAt.Equal(apiComment.UpdatedAt) {
		return nil, "", fmt.Errorf("edited comment %d rejected", apiComment.ID)
	}
	if strings.TrimSpace(apiComment.Body) != strings.TrimSpace(ev.Comment.Body) {
		return nil, "", fmt.Errorf("comment %d body does not match event payload", apiComment.ID)
	}
	actor := strings.TrimSpace(apiComment.Author)
	if actor == "" || (ev.Comment.User.Login != "" && !strings.EqualFold(actor, strings.TrimSpace(ev.Comment.User.Login))) {
		return nil, "", fmt.Errorf("comment %d author %q does not match event user %q", apiComment.ID, actor, ev.Comment.User.Login)
	}
	if strings.EqualFold(actor, ActionsBotLogin) || strings.EqualFold(apiComment.AuthorType, "Bot") || strings.EqualFold(ev.Comment.User.Type, "Bot") {
		return nil, "", fmt.Errorf("bot accounts cannot issue fork worker requests")
	}
	return apiComment, actor, nil
}

func (c *Coordinator) HandleEvent(ctx context.Context, ev IssueCommentEvent) (*ForkResult, error) {
	if c == nil || c.Tracker == nil || strings.TrimSpace(c.RepoRoot) == "" {
		return nil, fmt.Errorf("fork coordinator is not configured")
	}
	if !IsRequestComment(ev.Comment.Body) {
		return nil, nil
	}

	apiComment, actor, err := c.verifyEventAndComment(ctx, &ev)
	if err != nil {
		return nil, err
	}
	req, err := ParseRequestComment(apiComment.Body)
	if err != nil {
		res := ForkResult{
			Version:     1,
			RequestID:   apiComment.ID,
			IssueNumber: ev.Issue.Number,
			Success:     false,
			Error:       err.Error(),
		}
		c.postResult(ctx, ev.Issue.Number, res)
		return &res, err
	}
	if req.IssueNumber != ev.Issue.Number {
		mismatchErr := fmt.Errorf("request issue_number %d does not match comment issue #%d", req.IssueNumber, ev.Issue.Number)
		res := ForkResult{
			Version:     1,
			RequestID:   apiComment.ID,
			IssueNumber: ev.Issue.Number,
			Operation:   req.Operation,
			Success:     false,
			Error:       mismatchErr.Error(),
		}
		c.postResult(ctx, ev.Issue.Number, res)
		return &res, mismatchErr
	}

	updatedState, rev, opErr := c.executeRequest(ctx, apiComment.ID, actor, *req)
	res := ForkResult{
		Version:     1,
		RequestID:   apiComment.ID,
		IssueNumber: req.IssueNumber,
		Operation:   req.Operation,
		Success:     opErr == nil,
		Revision:    rev,
	}
	if updatedState != nil {
		res.ClaimID = updatedState.Lease.ClaimID
	}
	if opErr != nil {
		res.Error = opErr.Error()
	}
	c.postResult(ctx, req.IssueNumber, res)
	return &res, opErr
}

func (c *Coordinator) postResult(ctx context.Context, issueNumber int, res ForkResult) {
	body, err := FormatResultComment(res)
	if err != nil {
		return
	}
	_, _ = c.Tracker.CreateComment(ctx, issueNumber, body)
}

// Operations lists every request operation the coordinator executes. The
// generated workflow admits exactly these before checkout; a test keeps the
// two in sync.
var Operations = []string{
	"acquire", "renew", "release", "handoff", "require-gate", "update-phase", "update-artifact", "record-checkpoint", "record-submission",
	"acquire-pr", "begin-pr-revision", "record-pr-head", "invalidate-pr-evidence", "complete-pr-revision",
}

func (c *Coordinator) executeRequest(ctx context.Context, requestID int64, actor string, req lease.ForkRequest) (*lease.IssueState, string, error) {
	readStore := coordination.NewGitStore(c.RepoRoot)
	rawCurrent, _, err := readStore.Load(ctx, req.IssueNumber)
	if err != nil {
		return nil, "", fmt.Errorf("%w: approve task first", lease.ErrApprovalRequired)
	}
	var currentState lease.IssueState
	if err := json.Unmarshal(rawCurrent, &currentState); err != nil {
		return nil, "", fmt.Errorf("decode current issue state: %w", err)
	}
	if currentState.LastForkRequestID >= requestID {
		return nil, "", fmt.Errorf("%w: comment %d <= last processed %d", coordination.ErrReplay, requestID, currentState.LastForkRequestID)
	}
	if strings.TrimSpace(currentState.BaseSHA) == "" {
		return nil, "", lease.ErrApprovalRequired
	}
	_ = workspace.FetchCommit(c.RepoRoot, "origin", currentState.BaseSHA)

	cfg, err := harness.LoadConfigAt(c.RepoRoot, currentState.BaseSHA)
	if err != nil {
		return nil, "", fmt.Errorf("load approved policy at %s: %w", currentState.BaseSHA, err)
	}

	idxStore := coordination.NewForkIndexedStore(c.RepoRoot, actor, cfg.MaxActiveForkClaims(), requestID)
	idxStore.SetNowFunc(c.now)

	actTracker := &actionTracker{Tracker: c.Tracker, actor: actor}
	mgr := lease.NewManager(actTracker, cfg.Security)
	mgr.SetStore(c.RepoRoot, idxStore)
	mgr.SetNowFunc(c.now)

	var updated *lease.IssueState
	switch req.Operation {
	case "acquire":
		canonicalFork, err := c.verifyForkRepo(ctx, req.ForkRepo, actor)
		if err != nil {
			return nil, "", err
		}
		agentName, phase, reqDur, err := forkClaimRequest(cfg, req)
		if err != nil {
			return nil, "", err
		}
		updated, err = mgr.AcquireFork(ctx, req.IssueNumber, actor, agentName, phase, canonicalFork, reqDur, cfg.Lease.MaxRenewals)
		if err != nil {
			return nil, "", err
		}

	case "acquire-pr":
		// Only the contributor whose fork holds the PR head may revise it, and
		// only for the PR the issue's authoritative state already links.
		if req.PRNumber <= 0 || currentState.PRNumber != req.PRNumber || strings.TrimSpace(currentState.ForkRepo) == "" {
			return nil, "", fmt.Errorf("PR #%d is not a fork contribution linked to issue #%d", req.PRNumber, req.IssueNumber)
		}
		if _, err := c.verifyForkRepo(ctx, currentState.ForkRepo, actor); err != nil {
			return nil, "", err
		}
		if _, err := c.verifyForkPR(ctx, &currentState, req.PRNumber); err != nil {
			return nil, "", err
		}
		agentName, phase, reqDur, err := forkClaimRequest(cfg, req)
		if err != nil {
			return nil, "", err
		}
		dur, maxRenewals, err := forkLeaseTerms(cfg, reqDur)
		if err != nil {
			return nil, "", err
		}
		updated, err = mgr.AcquirePR(ctx, req.IssueNumber, req.PRNumber, actor, agentName, phase, dur, maxRenewals)
		if err != nil {
			return nil, "", err
		}

	default:
		if strings.TrimSpace(req.ClaimID) == "" ||
			currentState.Lease.Status != "active" ||
			!currentState.Lease.ExpiresAt.After(c.now()) ||
			!strings.EqualFold(strings.TrimSpace(currentState.Lease.Owner), strings.TrimSpace(actor)) ||
			currentState.Lease.ClaimID != req.ClaimID ||
			strings.TrimSpace(currentState.ForkRepo) == "" {
			return nil, "", lease.ErrClaimFence
		}
		if _, err := c.verifyForkRepo(ctx, currentState.ForkRepo, actor); err != nil {
			return nil, "", err
		}
		mgr.BindClaim(req.IssueNumber, req.ClaimID)

		switch req.Operation {
		case "renew":
			dur, err := time.ParseDuration(cfg.Lease.Duration)
			if err != nil || dur <= 0 {
				return nil, "", fmt.Errorf("invalid approved lease duration")
			}
			if strings.TrimSpace(req.Duration) != "" {
				reqDur, err := time.ParseDuration(req.Duration)
				if err != nil || reqDur <= 0 {
					return nil, "", fmt.Errorf("invalid renew duration %q", req.Duration)
				}
				if reqDur < dur {
					dur = reqDur
				}
			}
			updated, err = mgr.Renew(ctx, req.IssueNumber, actor, dur)
			if err != nil {
				return nil, "", err
			}

		case "release":
			updated, err = mgr.Release(ctx, req.IssueNumber, actor, req.Reason)
			if err != nil {
				return nil, "", err
			}

		case "handoff":
			updated, err = mgr.Handoff(ctx, req.IssueNumber, actor, req.Phase, req.NextPhase)
			if err != nil {
				return nil, "", err
			}

		case "require-gate":
			updated, err = mgr.RequireGate(ctx, req.IssueNumber, actor, req.Gate)
			if err != nil {
				return nil, "", err
			}

		case "update-phase":
			updated, err = mgr.UpdatePhase(ctx, req.IssueNumber, actor, req.Phase, req.Status)
			if err != nil {
				return nil, "", err
			}

		case "update-artifact":
			if req.Artifact == nil {
				return nil, "", fmt.Errorf("update-artifact requires artifact payload")
			}
			art := *req.Artifact
			if strings.EqualFold(art.Metadata["kind"], "human") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(art.Producer)), "human:") {
				return nil, "", fmt.Errorf("human review artifacts cannot be submitted via fork coordinator")
			}
			if art.Type == contract.ArtifactReview {
				if art.Metadata == nil {
					art.Metadata = make(map[string]string)
				}
				art.Metadata["kind"] = "agent"
				art.Metadata["actor"] = actor
			}
			if art.Type == contract.ArtifactReview {
				updated, err = mgr.UpdateAdvisoryReview(ctx, req.IssueNumber, actor, art)
			} else {
				updated, err = mgr.UpdateArtifact(ctx, req.IssueNumber, actor, art)
			}
			if err != nil {
				return nil, "", err
			}

		case "record-checkpoint":
			expectedBranch := fmt.Sprintf("swarm/issue-%d-%s", req.IssueNumber, req.ClaimID)
			if req.Branch != expectedBranch {
				return nil, "", fmt.Errorf("fork checkpoint branch %q must match %q", req.Branch, expectedBranch)
			}
			if !isFullCommitSHA(req.HeadSHA) {
				return nil, "", fmt.Errorf("fork checkpoint head_sha %q must be a full commit SHA", req.HeadSHA)
			}
			if err := c.verifyForkCheckpointCommit(ctx, currentState.ForkRepo, req.Branch, req.HeadSHA, currentState.BaseSHA); err != nil {
				return nil, "", err
			}
			updated, err = mgr.RecordCheckpoint(ctx, req.IssueNumber, actor, req.Branch, req.HeadSHA)
			if err != nil {
				return nil, "", err
			}

		case "record-submission":
			if req.PRNumber <= 0 {
				return nil, "", fmt.Errorf("record-submission requires a positive pr_number")
			}
			if currentState.Branch == "" || currentState.HeadSHA == "" {
				return nil, "", fmt.Errorf("cannot record submission before recording a fork checkpoint")
			}
			pr, err := c.Tracker.GetPullRequest(ctx, req.PRNumber)
			if err != nil || pr == nil {
				return nil, "", fmt.Errorf("verify pull request #%d: %w", req.PRNumber, err)
			}
			if !pr.Draft {
				return nil, "", fmt.Errorf("pull request #%d must be an open Draft PR", req.PRNumber)
			}
			if !strings.EqualFold(strings.TrimSpace(pr.HeadRepo), strings.TrimSpace(currentState.ForkRepo)) {
				return nil, "", fmt.Errorf("pull request #%d head repo %q does not match claim fork %q", req.PRNumber, pr.HeadRepo, currentState.ForkRepo)
			}
			if c.UpstreamRepo != "" && !strings.EqualFold(strings.TrimSpace(pr.BaseRepo), strings.TrimSpace(c.UpstreamRepo)) {
				return nil, "", fmt.Errorf("pull request #%d base repo %q does not match upstream %q", req.PRNumber, pr.BaseRepo, c.UpstreamRepo)
			}
			if expectedBase, baseErr := c.resolveDefaultBase(); baseErr == nil && expectedBase != "" && pr.Base != expectedBase {
				return nil, "", fmt.Errorf("pull request #%d base branch %q does not match upstream base %q", req.PRNumber, pr.Base, expectedBase)
			}
			forkOwner := strings.Split(currentState.ForkRepo, "/")[0]
			headBranch := strings.TrimPrefix(pr.Head, forkOwner+":")
			if headBranch != currentState.Branch {
				return nil, "", fmt.Errorf("pull request #%d head branch %q does not match checkpoint branch %q", req.PRNumber, pr.Head, currentState.Branch)
			}
			if pr.HeadSHA != currentState.HeadSHA {
				return nil, "", fmt.Errorf("pull request #%d head SHA %q does not match checkpoint SHA %q", req.PRNumber, pr.HeadSHA, currentState.HeadSHA)
			}
			if strings.TrimSpace(pr.HTMLURL) == "" {
				return nil, "", fmt.Errorf("pull request #%d response is missing html_url", req.PRNumber)
			}
			updated, err = mgr.RecordSubmission(ctx, req.IssueNumber, actor, pr.Number, pr.HTMLURL)
			if err != nil {
				return nil, "", err
			}

		case "begin-pr-revision":
			if currentState.PRNumber != req.PRNumber || req.Branch != workspace.PRBranchName(req.PRNumber) || !isFullCommitSHA(req.ExpectedHeadSHA) || !isFullCommitSHA(req.HeadSHA) {
				return nil, "", fmt.Errorf("begin-pr-revision requires the linked PR, its managed branch, and full head SHAs")
			}
			pr, err := c.verifyForkPR(ctx, &currentState, req.PRNumber)
			if err != nil {
				return nil, "", err
			}
			if pr.HeadSHA != req.ExpectedHeadSHA {
				return nil, "", fmt.Errorf("pull request #%d head is %s, not %s", req.PRNumber, pr.HeadSHA, req.ExpectedHeadSHA)
			}
			// A new revision starts from the published head; local work is recorded later.
			if currentState.PRRevision == nil && req.HeadSHA != req.ExpectedHeadSHA {
				return nil, "", fmt.Errorf("a new PR revision must start at the published head %s", req.ExpectedHeadSHA)
			}
			updated, err = mgr.BeginPRRevision(ctx, req.IssueNumber, actor, req.PRNumber, req.ExpectedHeadSHA, req.HeadSHA, req.Branch, req.RevisionIntent)
			if err != nil {
				return nil, "", err
			}

		case "record-pr-head":
			// The local head cannot be inspected here; complete-pr-revision
			// verifies the commit that is actually published.
			if req.Branch != workspace.PRBranchName(req.PRNumber) || !isFullCommitSHA(req.HeadSHA) {
				return nil, "", fmt.Errorf("record-pr-head requires the managed PR branch and a full head SHA")
			}
			updated, err = mgr.RecordPRHead(ctx, req.IssueNumber, actor, req.PRNumber, req.Branch, req.HeadSHA)
			if err != nil {
				return nil, "", err
			}

		case "invalidate-pr-evidence":
			if req.RebaseContext != nil {
				updated, err = mgr.UpdateRebaseContext(ctx, req.IssueNumber, actor, req.PRNumber, *req.RebaseContext)
			} else {
				updated, err = mgr.InvalidatePRRevisionEvidence(ctx, req.IssueNumber, actor, req.PRNumber, req.RevisionIntent)
			}
			if err != nil {
				return nil, "", err
			}

		case "complete-pr-revision":
			rev := currentState.PRRevision
			if rev == nil || rev.Number != req.PRNumber || rev.RemoteHeadSHA != req.ExpectedHeadSHA || rev.LocalHeadSHA != req.HeadSHA || !isFullCommitSHA(req.HeadSHA) {
				return nil, "", fmt.Errorf("complete-pr-revision does not match the pending revision")
			}
			pr, err := c.verifyForkPR(ctx, &currentState, req.PRNumber)
			if err != nil {
				return nil, "", err
			}
			if pr.HeadSHA != req.HeadSHA {
				return nil, "", fmt.Errorf("pull request #%d head is %s, not the validated revision %s", req.PRNumber, pr.HeadSHA, req.HeadSHA)
			}
			if err := c.verifyForkCheckpointCommit(ctx, currentState.ForkRepo, currentState.Branch, req.HeadSHA, currentState.BaseSHA); err != nil {
				return nil, "", err
			}
			if err := c.verifyRevisionEvidence(ctx, req.IssueNumber, &currentState, req.HeadSHA); err != nil {
				return nil, "", err
			}
			// Project after GitHub verification but before completing the claim,
			// so projection failures remain retryable through the pending revision.
			if r := currentState.LastRebase; r != nil && r.Status == "completed" {
				r.NewHead = req.HeadSHA
				r.PatchIdentity = "unavailable"
				r.Verified = false
				if identity, e := workspace.RebasePatchIdentity(ctx, c.RepoRoot, r.OldBase, r.OldHead, r.NewBase, r.NewHead); e == nil {
					r.PatchIdentity = identity
					r.Verified = true
				}
			}
			if err = prservice.ProjectRebase(ctx, c.Tracker, &currentState, req.PRNumber); err != nil {
				return nil, "", fmt.Errorf("PR head published; retry pr submit to update rebase context: %w", err)
			}
			updated, err = mgr.CompletePRRevisionLocal(ctx, req.IssueNumber, actor, req.PRNumber, req.ExpectedHeadSHA, req.HeadSHA)
			if err != nil {
				return nil, "", err
			}

		default:
			return nil, "", fmt.Errorf("unsupported fork operation %q", req.Operation)
		}
	}

	// The mutation is already durable; the new revision is advisory for the
	// worker, so a failed re-read reports none rather than failing the request.
	_, newRev, revErr := readStore.Load(ctx, req.IssueNumber)
	if revErr != nil {
		return updated, "", nil
	}
	return updated, newRev, nil
}

// forkClaimRequest validates the agent and phase a contributor asks to claim with.
func forkClaimRequest(cfg *contract.SwarmConfig, req lease.ForkRequest) (agentName, phase string, duration time.Duration, err error) {
	if strings.TrimSpace(req.Duration) != "" {
		duration, err = time.ParseDuration(req.Duration)
		if err != nil || duration <= 0 {
			return "", "", 0, fmt.Errorf("invalid %s duration %q", req.Operation, req.Duration)
		}
	}
	agentName = strings.TrimSpace(req.Agent)
	if agentName == "" {
		agentName = cfg.DefaultAgent
	}
	if _, ok := cfg.Agents[agentName]; !ok {
		return "", "", 0, fmt.Errorf("agent %q is not in the approved agents allowlist", agentName)
	}
	phase = strings.TrimSpace(req.Phase)
	if phase == "" {
		phase = "all"
	}
	return agentName, phase, duration, nil
}

// forkLeaseTerms caps a requested lease at the approved policy, as AcquireFork does.
func forkLeaseTerms(cfg *contract.SwarmConfig, requested time.Duration) (time.Duration, int, error) {
	policy, err := time.ParseDuration(cfg.Lease.Duration)
	if err != nil || policy <= 0 {
		return 0, 0, fmt.Errorf("invalid approved lease duration")
	}
	if requested <= 0 || requested > policy {
		requested = policy
	}
	maxRenewals := cfg.Lease.MaxRenewals
	if !cfg.Lease.Renewable {
		maxRenewals = 0
	}
	return requested, maxRenewals, nil
}

// verifyForkPR confirms on GitHub that the PR is open, targets upstream, and
// still has its head on the issue's recorded fork branch.
func (c *Coordinator) verifyForkPR(ctx context.Context, st *lease.IssueState, number int) (*tracker.PullRequest, error) {
	pr, err := c.Tracker.GetPullRequest(ctx, number)
	if err != nil || pr == nil {
		return nil, fmt.Errorf("verify pull request #%d: %w", number, err)
	}
	if pr.Number != number || !strings.EqualFold(pr.State, "open") || pr.Merged {
		return nil, fmt.Errorf("pull request #%d must be open and unmerged", number)
	}
	if !strings.EqualFold(strings.TrimSpace(pr.HeadRepo), strings.TrimSpace(st.ForkRepo)) {
		return nil, fmt.Errorf("pull request #%d head repo %q does not match claim fork %q", number, pr.HeadRepo, st.ForkRepo)
	}
	if c.UpstreamRepo != "" && !strings.EqualFold(strings.TrimSpace(pr.BaseRepo), strings.TrimSpace(c.UpstreamRepo)) {
		return nil, fmt.Errorf("pull request #%d base repo %q does not match upstream %q", number, pr.BaseRepo, c.UpstreamRepo)
	}
	forkOwner := strings.Split(st.ForkRepo, "/")[0]
	if strings.TrimPrefix(pr.Head, forkOwner+":") != st.Branch {
		return nil, fmt.Errorf("pull request #%d head branch %q does not match recorded branch %q", number, pr.Head, st.Branch)
	}
	return pr, nil
}

// verifyRevisionEvidence requires the revision's plan approval to be current and
// a passing validation report for exactly the head being published.
func (c *Coordinator) verifyRevisionEvidence(ctx context.Context, issueNumber int, st *lease.IssueState, head string) error {
	issue, err := c.Tracker.GetIssue(ctx, issueNumber)
	if err != nil {
		return err
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		return err
	}
	if err := lease.VerifyApproval(st, wu); err != nil {
		return err
	}
	for _, art := range st.Artifacts {
		if art.Type == contract.ArtifactValidation && art.Metadata["head_sha"] == head && strings.EqualFold(art.Verdict, "PASS") {
			return nil
		}
	}
	return fmt.Errorf("no passing validation recorded for PR revision %s", head)
}

func (c *Coordinator) resolveDefaultBase() (string, error) {
	if c.DefaultBaseFunc != nil {
		return c.DefaultBaseFunc(c.RepoRoot)
	}
	out, err := runGitWithToken(context.Background(), c.RepoRoot, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		if b := strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"); b != "" {
			return b, nil
		}
	}
	return "", nil
}

func (c *Coordinator) verifyForkCheckpointCommit(ctx context.Context, forkRepo, branch, headSHA, baseSHA string) error {
	forkURL := c.forkGitURL(forkRepo)
	ref := "refs/heads/" + branch
	lsOut, err := runGitWithToken(ctx, c.RepoRoot, "ls-remote", "--refs", forkURL, ref)
	if err != nil {
		return fmt.Errorf("query fork branch %s on %s: %w", branch, forkRepo, err)
	}
	fields := strings.Fields(lsOut)
	if len(fields) < 2 || fields[0] != headSHA || fields[1] != ref {
		got := strings.TrimSpace(lsOut)
		if len(fields) > 0 {
			got = fields[0]
		}
		return fmt.Errorf("fork branch %s on %s resolves to %q, want %q", branch, forkRepo, got, headSHA)
	}
	if _, err := runGitWithToken(ctx, c.RepoRoot, "fetch", "--no-write-fetch-head", forkURL, headSHA); err != nil {
		return fmt.Errorf("fetch fork commit %s from %s: %w", headSHA, forkRepo, err)
	}
	if _, err := runGitWithToken(ctx, c.RepoRoot, "merge-base", "--is-ancestor", baseSHA, headSHA); err != nil {
		return fmt.Errorf("fork commit %s does not descend from approved base %s", headSHA, baseSHA)
	}
	return nil
}

func runGitWithToken(ctx context.Context, dir string, args ...string) (string, error) {
	cmd, redact, err := gitcredential.Command(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, redact(strings.TrimSpace(string(out))))
	}
	return redact(strings.TrimSpace(string(out))), nil
}

func isFullCommitSHA(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, r := range sha {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}
