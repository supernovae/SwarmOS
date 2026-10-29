package lease

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

type checkpointReconciler interface {
	CompareAndSwapAndReconcileCheckpoint(context.Context, int, string, []byte, string, string, string) (string, error)
}

type CheckpointReconciliation struct {
	ClaimRequired         bool `json:"claim_required"`
	BoundaryProbeRequired bool `json:"boundary_probe_required"`
	revision              string
	owner                 string
	expected              string
	candidate             string
	prepared              bool
	State                 *IssueState               `json:"state"`
	Observation           workspace.CheckpointState `json:"checkpoint"`
	DryRun                bool                      `json:"dry_run"`
	ValidationRequired    bool                      `json:"validation_required"`
	ReviewRequired        bool                      `json:"review_required"`
	PatchIdentity         string                    `json:"patch_identity"`
}

// ReconcileCheckpoint accepts only an explicit, clean local task candidate. It
// never retries publication against a newer state or remote branch.
func (m *Manager) ReconcileCheckpoint(ctx context.Context, n int, owner, expected, candidate string, dryRun bool) (*CheckpointReconciliation, error) {
	if dryRun {
		return m.PrepareCheckpoint(ctx, n, owner, expected, candidate, false)
	}
	return m.reconcileCheckpoint(ctx, n, owner, expected, candidate, false, false, false, "")
}

// PrepareCheckpoint authenticates and observes recovery without acquiring a
// claim, fetching objects, opening locks, or running candidate checks.
func (m *Manager) PrepareCheckpoint(ctx context.Context, n int, owner, expected, candidate string, probe bool) (*CheckpointReconciliation, error) {
	return m.reconcileCheckpoint(ctx, n, owner, expected, candidate, true, true, probe, "")
}

func (m *Manager) reconcileCheckpoint(ctx context.Context, n int, owner, expected, candidate string, dryRun, prepare, probe bool, pinned string) (*CheckpointReconciliation, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("checkpoint reconciliation is upstream-only; use the fork PR revision workflow")
	}
	publisher, gitStore := m.store.(checkpointReconciler)
	_, localStore := m.store.(*coordination.FileStore)
	if !gitStore && !localStore {
		return nil, fmt.Errorf("checkpoint reconciliation requires an upstream Git store or local FileStore")
	}
	for _, sha := range []string{expected, candidate} {
		if len(sha) != 40 {
			return nil, fmt.Errorf("expected-head and candidate-head must be full commit SHAs")
		}
		if _, err := hex.DecodeString(sha); err != nil {
			return nil, fmt.Errorf("invalid commit SHA %q", sha)
		}
	}
	if expected == candidate {
		return nil, fmt.Errorf("candidate must differ from expected predecessor")
	}
	var st *IssueState
	var rev string
	var err error
	if prepare {
		st, rev, err = m.loadCheckpointPreview(ctx, n)
	} else {
		st, rev, err = m.load(ctx, n)
	}
	if err != nil {
		return nil, err
	}
	if pinned != "" && rev != pinned {
		return nil, ErrClaimFence
	}
	claimRequired := false
	if prepare {
		actor, e := m.tracker.CurrentUser(ctx)
		if e != nil {
			return nil, e
		}
		if owner == "" || actor != owner {
			return nil, ErrClaimFence
		}
		if err = m.checkApprovedState(ctx, n, st); err != nil {
			return nil, err
		}
		cfg, e := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
		if e != nil {
			return nil, e
		}
		if !matchesCollaboratorAllowlist(owner, cfg.Security.ApprovedCollaborators) {
			return nil, fmt.Errorf("actor %q is not in security.approved_collaborators", owner)
		}
		claimRequired = st.Lease.Status != "active" || !st.Lease.ExpiresAt.After(m.nowFn())
		if !claimRequired {
			if err = m.checkStateClaim(ctx, n, owner, st); err != nil {
				return nil, err
			}
		}
	} else if err = m.checkStateClaim(ctx, n, owner, st); err != nil {
		return nil, err
	}
	if st.ForkRepo != "" || st.PRNumber != 0 || st.PRRevision != nil || st.DroppedCheckpoint != nil {
		return nil, fmt.Errorf("checkpoint reconciliation requires upstream issue work without submitted PR or pending recovery; use the PR revision workflow for linked PRs")
	}
	if st.HeadSHA != expected || st.Branch == "" {
		return nil, fmt.Errorf("%w: authoritative checkpoint %s differs from expected %s", ErrClaimFence, st.HeadSHA, expected)
	}
	inspect := func() (workspace.CheckpointState, error) {
		remote := m.UpstreamRemote()
		if localStore {
			remote = "."
		}
		o := workspace.InspectCheckpoint(ctx, m.repoRoot, remote, n, st.Branch, expected)
		if localStore {
			o.RemoteBranch = workspace.HeadObservation{}
		}
		if o.Error != "" || o.Worktree.Error != "" || o.LocalBranch.Error != "" || o.RemoteBranch.Error != "" ||
			o.Dirty || o.Rebasing || o.AttachedBranch != st.Branch || o.Worktree.SHA != candidate || o.LocalBranch.SHA != candidate || gitStore && o.RemoteBranch.SHA != expected {
			return o, fmt.Errorf("%w: reconciliation preconditions failed: %s", ErrClaimFence, o.String())
		}
		return o, nil
	}
	observation, err := inspect()
	if err != nil {
		return nil, err
	}
	// Fetch only immutable object retention, never reset or fetch into task refs.
	if _, err = checkpointGit(ctx, m.repoRoot, "cat-file", "-e", expected+"^{commit}"); err != nil {
		if dryRun || localStore {
			return nil, fmt.Errorf("predecessor %s unavailable locally; fetch %s before inspecting lineage", expected, m.LedgerLayout().CheckpointRef(n, expected))
		}
		if _, err = checkpointGit(ctx, m.repoRoot, "fetch", "--no-write-fetch-head", m.UpstreamRemote(), m.LedgerLayout().CheckpointRef(n, expected)); err != nil {
			return nil, fmt.Errorf("predecessor %s unavailable: %w", expected, err)
		}
	}
	ancestor, err := workspace.BaseIsAncestor(ctx, m.repoRoot, st.BaseSHA, candidate)
	if err != nil || !ancestor {
		return nil, fmt.Errorf("candidate %s must descend from approved base %s", candidate, st.BaseSHA)
	}
	additive, err := workspace.BaseIsAncestor(ctx, m.repoRoot, expected, candidate)
	if err != nil {
		return nil, err
	}
	if (claimRequired || localStore) && !additive {
		return nil, fmt.Errorf("candidate must descend from predecessor %s for fresh or local reconciliation", expected)
	}
	identity, identityErr := workspace.RebasePatchIdentity(ctx, m.repoRoot, st.BaseSHA, expected, st.BaseSHA, candidate)
	if identityErr != nil {
		identity = "unavailable"
	}
	issue, err := m.tracker.GetIssue(ctx, n)
	if err != nil {
		return nil, err
	}
	wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
	if err != nil {
		return nil, err
	}
	if prepare {
		if err = m.VerifyDependencies(ctx, wu); err != nil {
			return nil, err
		}
	}
	approvalErr := VerifyApproval(st, wu)
	reviewRequired := approvalErr != nil
	if !additive && identity == "unchanged" && st.EffectiveGate != contract.GateNone {
		eligible := false
		for _, a := range st.Artifacts {
			if a.Type == contract.ArtifactReview && st.PolicyTreeDigest != "" && a.Metadata["policy_tree_digest"] == st.PolicyTreeDigest {
				eligible = true
			}
		}
		reviewRequired = reviewRequired || !eligible
	}
	if !additive && identity != "unchanged" {
		// Changed patches require an explicitly reviewed replacement plan. Naming
		// the candidate in that plan binds the existing authenticated review format
		// to this replacement without treating patch identity as authorization.
		copyState := *st
		if copyState.EffectiveGate == contract.GateNone {
			copyState.EffectiveGate = contract.GateAgent
		}
		named := false
		for _, a := range st.Artifacts {
			if a.Type == contract.ArtifactDesign && strings.Contains(a.Body, candidate) {
				named = true
			}
		}
		reviewRequired = !named || VerifyApproval(&copyState, wu) != nil
	}
	var boundaryProbeRequired bool
	if prepare {
		boundaryProbeRequired, err = validation.CheckCheckpointBoundary(ctx, m.repoRoot, st.BaseSHA, probe)
		if err != nil {
			return nil, err
		}
	}
	result := &CheckpointReconciliation{ClaimRequired: claimRequired, BoundaryProbeRequired: boundaryProbeRequired, revision: rev, owner: owner, expected: expected, candidate: candidate, prepared: prepare, State: st, Observation: observation, DryRun: dryRun, ValidationRequired: true, ReviewRequired: reviewRequired, PatchIdentity: identity}
	if dryRun {
		return result, nil
	}
	if reviewRequired {
		return result, fmt.Errorf("%w: stage a replacement plan naming candidate %s and obtain fresh plan review with swarm issue review-plan %d before reconciling", ErrApprovalRequired, candidate, n)
	}
	// Clone a pinned candidate so checks cannot validate a moving live checkout.
	snapshot, err := os.MkdirTemp("", "swarm-checkpoint-validation-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(snapshot)
	if _, err = checkpointGit(ctx, m.repoRoot, "clone", "--shared", "--no-checkout", "--", m.repoRoot, snapshot); err != nil {
		return nil, err
	}
	if _, err = checkpointGit(ctx, snapshot, "checkout", "--detach", candidate); err != nil {
		return nil, err
	}
	// Snapshot preparation can race reapproval or another fenced state write.
	// Check again before executing any required check under the pinned policy.
	beforeValidation, beforeRevision, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if beforeRevision != rev {
		return nil, fmt.Errorf("%w: issue state changed before validation", ErrClaimFence)
	}
	if err = m.checkStateClaim(ctx, n, owner, beforeValidation); err != nil {
		return nil, err
	}
	report, err := validation.Validate(ctx, snapshot, wu, validation.ValidateOptions{BaseSHA: st.BaseSHA, CIMode: true})
	if err != nil {
		return result, err
	}
	validatedHead, err := checkpointGit(ctx, snapshot, "rev-parse", "HEAD")
	if err != nil || validatedHead != candidate || report.HeadSHA != candidate {
		return nil, fmt.Errorf("%w: candidate changed during validation", ErrClaimFence)
	}
	status, err := checkpointGit(ctx, snapshot, "--no-optional-locks", "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return nil, fmt.Errorf("%w: validation modified the candidate snapshot", ErrClaimFence)
	}
	fresh, freshRev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if freshRev != rev {
		return nil, fmt.Errorf("%w: issue state changed during validation", ErrClaimFence)
	}
	if err = m.checkStateClaim(ctx, n, owner, fresh); err != nil {
		return nil, err
	}
	if _, err = inspect(); err != nil {
		return nil, err
	}
	preserveCheckpointHeads(st, st.HeadSHA, st.SubmittedHeadSHA, candidate)
	clearHeadEvidence(st)
	st.HeadSHA = candidate
	st.LastRebase = &RebaseEvidence{OldHead: expected, NewHead: candidate, OldBase: st.BaseSHA, NewBase: st.BaseSHA, Status: "completed", PatchIdentity: identity, Verified: identityErr == nil}
	art := report.Artifact
	for k, v := range ReviewBindings(st) {
		art.Metadata[k] = v
	}
	art.Metadata["content_sha256"] = hash(art.Body)
	st.Artifacts = append(st.Artifacts, art)
	stat, err := checkpointGit(ctx, m.repoRoot, "diff", "--stat", st.BaseSHA, candidate, "--")
	if err != nil {
		return nil, err
	}
	implementation := contract.Artifact{Version: 1, ID: fmt.Sprintf("art-%d-implementation", n), IssueNumber: n, Type: contract.ArtifactImplementation, Producer: "swarm:checkpoint-reconciler", Verdict: "PASS", Summary: "Explicit checkpoint reconciliation", Body: fmt.Sprintf("Reconciled checkpoint %s to %s.\n\n%s\n", expected, candidate, stat), CreatedAt: m.nowFn(), Metadata: ReviewBindings(st)}
	implementation.Metadata["head_sha"] = candidate
	implementation.Metadata["content_sha256"] = hash(implementation.Body)
	st.Artifacts = append(st.Artifacts, implementation)
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	if gitStore {
		_, err = publisher.CompareAndSwapAndReconcileCheckpoint(ctx, n, rev, raw, st.Branch, candidate, expected)
	} else {
		_, err = m.store.CompareAndSwap(ctx, n, rev, raw)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: checkpoint publication rejected: %w", ErrClaimFence, err)
	}
	m.project(ctx, n, st)
	result.State = st
	result.ValidationRequired = false
	return result, nil
}

// AcquirePreparedCheckpoint uses ordinary admission and never retries a newer
// observation. The returned claim is still subject to all reconciliation fences.
func (m *Manager) AcquirePreparedCheckpoint(ctx context.Context, n int, p *CheckpointReconciliation) (*IssueState, error) {
	if p == nil || !p.prepared || !p.ClaimRequired {
		return nil, ErrClaimFence
	}
	fresh, err := m.PrepareCheckpoint(ctx, n, p.owner, p.expected, p.candidate, true)
	if err != nil {
		return nil, err
	}
	if fresh.revision != p.revision || !sameCheckpointState(fresh.State, p.State) {
		return nil, fmt.Errorf("%w: prepared observation changed", ErrClaimFence)
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, p.State.BaseSHA)
	if err != nil {
		return nil, err
	}
	duration, err := time.ParseDuration(cfg.Lease.Duration)
	if err != nil {
		return nil, err
	}
	agent := cfg.Roles["planner"]
	if agent == "" {
		agent = cfg.DefaultAgent
	}
	st, err := m.Acquire(ctx, n, p.owner, agent, "plan", duration, cfg.Lease.MaxRenewals)
	if err != nil {
		return nil, err
	}
	before, after := *p.State, *st
	before.Lease, after.Lease = contract.Lease{}, contract.Lease{}
	before.ClaimBudget, after.ClaimBudget = nil, nil
	before.ProjectionCommentID, after.ProjectionCommentID = 0, 0
	if !sameCheckpointState(&before, &after) {
		_, releaseErr := m.Release(ctx, n, p.owner, "checkpoint admission observation changed")
		return nil, errors.Join(fmt.Errorf("%w: acquisition changed checkpoint identity", ErrClaimFence), releaseErr)
	}
	// Recheck approved policy after acquisition; reconciliation must never validate
	// with a policy that changed between preparation and normal Acquire.
	if err = m.checkStateClaim(ctx, n, p.owner, st); err != nil {
		_, releaseErr := m.Release(ctx, n, p.owner, "checkpoint admission approval changed")
		return nil, errors.Join(err, releaseErr)
	}
	current, revision, err := m.load(ctx, n)
	// The response may contain an unpersisted comment projection ID. Comments
	// are projections, so compare all persisted authority against the response.
	response := *st
	if current != nil {
		response.ProjectionCommentID = current.ProjectionCommentID
	}
	if err != nil || !sameCheckpointState(current, &response) {
		_, releaseErr := m.Release(ctx, n, p.owner, "checkpoint admission state changed")
		return nil, errors.Join(fmt.Errorf("%w: acquired state changed", ErrClaimFence), err, releaseErr)
	}
	p.State, p.revision, p.ClaimRequired = current, revision, false
	return st, nil
}

// ReconcilePreparedCheckpoint pins the post-admission revision through the
// existing validation and atomic publication path.
func (m *Manager) ReconcilePreparedCheckpoint(ctx context.Context, n int, p *CheckpointReconciliation) (*CheckpointReconciliation, error) {
	if p == nil || !p.prepared || p.ClaimRequired {
		return nil, ErrClaimFence
	}
	return m.reconcileCheckpoint(ctx, n, p.owner, p.expected, p.candidate, false, false, false, p.revision)
}

func (m *Manager) loadCheckpointPreview(ctx context.Context, n int) (*IssueState, string, error) {
	reader, ok := m.store.(interface {
		LoadCheckpointPreview(context.Context, int) ([]byte, string, error)
	})
	if !ok {
		return nil, "", fmt.Errorf("store does not support read-only checkpoint inspection")
	}
	raw, rev, err := reader.LoadCheckpointPreview(ctx, n)
	if err != nil {
		return nil, "", err
	}
	var st IssueState
	if err = json.Unmarshal(raw, &st); err != nil {
		return nil, "", err
	}
	if (st.Lease.IssueNumber != 0 && st.Lease.IssueNumber != n) || st.Cleanup != nil {
		return nil, "", ErrClaimFence
	}
	return &st, rev, nil
}

func sameCheckpointState(a, b *IssueState) bool {
	left, le := json.Marshal(a)
	right, re := json.Marshal(b)
	return le == nil && re == nil && bytes.Equal(left, right)
}

func checkpointGit(ctx context.Context, root string, args ...string) (string, error) {
	cmd, redact, err := gitcredential.Command(ctx, filepath.Clean(root), args...)
	if err != nil {
		return "", err
	}
	var out []byte
	if gitcredential.IsNetwork(args) {
		out, err = gitcredential.Network(ctx, filepath.Clean(root), true, args...)
	} else {
		out, err = cmd.CombinedOutput()
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", redact(strings.Join(args, " ")), err, redact(strings.TrimSpace(string(out))))
	}
	return strings.TrimSpace(string(out)), nil
}
