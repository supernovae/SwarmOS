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
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/safefs"
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
func (m *Manager) ReconcileCheckpoint(ctx context.Context, n int, owner, expected, candidate string, dryRun bool, options ...validation.ValidateOptions) (*CheckpointReconciliation, error) {
	if dryRun {
		return m.PrepareCheckpoint(ctx, n, owner, expected, candidate, false)
	}
	return m.reconcileCheckpoint(ctx, n, owner, expected, candidate, false, false, false, "", options...)
}

// PrepareCheckpoint authenticates and observes recovery without acquiring a
// claim, fetching objects, opening locks, or running candidate checks.
func (m *Manager) PrepareCheckpoint(ctx context.Context, n int, owner, expected, candidate string, probe bool) (*CheckpointReconciliation, error) {
	return m.reconcileCheckpoint(ctx, n, owner, expected, candidate, true, true, probe, "")
}

func (m *Manager) reconcileCheckpoint(ctx context.Context, n int, owner, expected, candidate string, dryRun, prepare, probe bool, pinned string, options ...validation.ValidateOptions) (*CheckpointReconciliation, error) {
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
	if !dryRun {
		if err := m.denyResume(n); err != nil {
			return nil, err
		}
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
	boundExpected, boundCandidate, recovery, bindingErr := CheckpointPlanHeads(st)
	recoveryMismatch := recovery && (bindingErr != nil || boundExpected != expected || boundCandidate != candidate)
	if !prepare && recoveryMismatch {
		return nil, ErrApprovalRequired
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
	if !recovery && !additive && identity != "unchanged" {
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
	if recovery {
		copyState := *st
		if copyState.EffectiveGate == contract.GateNone {
			copyState.EffectiveGate = contract.GateAgent
		}
		reviewRequired = recoveryMismatch || VerifyApproval(&copyState, wu) != nil
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
	validationOpts := validation.ValidateOptions{BaseSHA: st.BaseSHA, ClaimID: st.Lease.ClaimID, CIMode: true}
	if len(options) > 1 {
		return nil, fmt.Errorf("one validation context required")
	}
	if len(options) == 1 {
		o := options[0]
		if (o.BaseSHA != "" && o.BaseSHA != st.BaseSHA) || (o.ClaimID != "" && o.ClaimID != st.Lease.ClaimID) {
			return nil, fmt.Errorf("%w: validation context mismatch", ErrClaimFence)
		}
		validationOpts.Version, validationOpts.LookupEnv = o.Version, o.LookupEnv
		validationOpts.DependencyPolicy, validationOpts.OnProgress = o.DependencyPolicy, o.OnProgress
		v := strings.TrimSpace(o.Version)
		validationOpts.CIMode = v == "" || v == "dev" || strings.HasSuffix(v, "+dirty")
	}
	// Clone a pinned candidate so checks cannot validate a moving live checkout.
	snapshot, err := os.MkdirTemp("", "swarm-checkpoint-validation-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(snapshot)
	preparationStarted := time.Now()
	if validationOpts.OnProgress != nil {
		validationOpts.OnProgress(progress.Event{Check: "snapshot preparation", Status: "started"})
	}
	err = func() error {
		if _, err = checkpointGit(ctx, m.repoRoot, "clone", "--shared", "--no-checkout", "--", m.repoRoot, snapshot); err != nil {
			return err
		}
		if _, err = checkpointGit(ctx, snapshot, "checkout", "--detach", candidate); err != nil {
			return err
		}
		if !validationOpts.CIMode {
			if err = prepareCheckpointValidation(ctx, snapshot, st); err != nil {
				return err
			}
		}
		return nil
	}()
	if validationOpts.OnProgress != nil {
		status := "PASS"
		if err != nil {
			status = "FAIL"
		}
		validationOpts.OnProgress(progress.Event{Check: "snapshot preparation", Status: status, StageElapsed: time.Since(preparationStarted)})
	}
	if err != nil {
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
	report, err := validation.Validate(ctx, snapshot, wu, validationOpts)
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
	if recovery {
		// The explicit reconciler consumes the pending recovery restriction. Keep
		// exact candidate/predecessor evidence on the authenticated review and rebase
		// record while allowing subsequent ordinary implementation checkpoints.
		for i := range st.Artifacts {
			if st.Artifacts[i].Type == contract.ArtifactDesign || st.Artifacts[i].Type == contract.ArtifactTestPlan {
				for key := range st.Artifacts[i].Metadata {
					if strings.HasPrefix(key, "checkpoint_plan_") {
						delete(st.Artifacts[i].Metadata, key)
					}
				}
			}
		}
	}
	st.LastRebase = &RebaseEvidence{OldHead: expected, NewHead: candidate, OldBase: st.BaseSHA, NewBase: st.BaseSHA, Status: "completed", PatchIdentity: identity, Verified: identityErr == nil}
	art := report.Artifact
	for k, v := range ReviewBindings(st) {
		art.Metadata[k] = v
	}
	if err = m.insertValidationArtifact(st, n, art, true); err != nil {
		return nil, err
	}
	stat, err := checkpointGit(ctx, m.repoRoot, "diff", "--stat", st.BaseSHA, candidate, "--")
	if err != nil {
		return nil, err
	}
	implementation := contract.Artifact{Version: 1, ID: fmt.Sprintf("art-%d-implementation", n), IssueNumber: n, Type: contract.ArtifactImplementation, Producer: "swarm:checkpoint-reconciler", Verdict: "PASS", Summary: "Explicit checkpoint reconciliation", Body: fmt.Sprintf("Reconciled checkpoint %s to %s.\n\n%s\n", expected, candidate, stat), CreatedAt: m.nowFn(), Metadata: ReviewBindings(st)}
	implementation.Metadata["head_sha"] = candidate
	implementation.Metadata["content_sha256"] = hash(implementation.Body)
	st.Artifacts = append(st.Artifacts, implementation)
	publicationStarted := time.Now()
	if validationOpts.OnProgress != nil {
		validationOpts.OnProgress(progress.Event{Check: "checkpoint publication", Status: "started"})
	}
	err = func() error {
		raw, err := marshalCoordinatorState(st)
		if err != nil {
			return err
		}
		if gitStore {
			_, err = publisher.CompareAndSwapAndReconcileCheckpoint(ctx, n, rev, raw, st.Branch, candidate, expected)
		} else {
			_, err = m.store.CompareAndSwap(ctx, n, rev, raw)
		}
		if err != nil {
			return fmt.Errorf("%w: checkpoint publication rejected: %w", ErrClaimFence, err)
		}
		return nil
	}()
	if validationOpts.OnProgress != nil {
		status := "PASS"
		if err != nil {
			status = "FAIL"
		}
		validationOpts.OnProgress(progress.Event{Check: "checkpoint publication", Status: status, StageElapsed: time.Since(publicationStarted)})
	}
	if err != nil {
		return nil, err
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
func (m *Manager) ReconcilePreparedCheckpoint(ctx context.Context, n int, p *CheckpointReconciliation, options ...validation.ValidateOptions) (*CheckpointReconciliation, error) {
	if p == nil || !p.prepared || p.ClaimRequired {
		return nil, ErrClaimFence
	}
	return m.reconcileCheckpoint(ctx, n, p.owner, p.expected, p.candidate, false, false, false, p.revision, options...)
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

// CheckpointPlanHeads reads only a coordinator-authored, matching report pair.
// Presence with invalid identity fails closed rather than using legacy plan text.
func CheckpointPlanHeads(st *IssueState) (expected, candidate string, present bool, err error) {
	var plan, test *contract.Artifact
	for i := range st.Artifacts {
		a := &st.Artifacts[i]
		if a.Type == contract.ArtifactDesign {
			plan = a
		}
		if a.Type == contract.ArtifactTestPlan {
			test = a
		}
		if (a.Type == contract.ArtifactDesign || a.Type == contract.ArtifactTestPlan) && a.Metadata["checkpoint_plan_origin"] != "" {
			present = true
		}
	}
	if !present {
		return
	}
	if plan == nil || test == nil {
		err = ErrApprovalRequired
		return
	}
	expected, candidate = plan.Metadata["checkpoint_plan_predecessor"], plan.Metadata["checkpoint_plan_candidate"]
	required := map[string]string{"checkpoint_plan_origin": "coordinator", "checkpoint_plan_claim": st.Lease.ClaimID, "checkpoint_plan_task": st.TaskDigest, "checkpoint_plan_policy": st.PolicyDigest, "checkpoint_plan_base": st.BaseSHA, "checkpoint_plan_predecessor": expected, "checkpoint_plan_candidate": candidate}
	if st.PolicyTreeDigest != "" {
		required["checkpoint_plan_policy_tree"] = st.PolicyTreeDigest
	}
	for k, v := range required {
		if v == "" || plan.Metadata[k] != v || test.Metadata[k] != v {
			err = ErrApprovalRequired
			return
		}
	}
	if len(expected) != 40 || len(candidate) != 40 || expected == candidate || (st.HeadSHA != expected && st.HeadSHA != candidate) || plan.Metadata["content_sha256"] != hash(plan.Body) || test.Metadata["content_sha256"] != hash(test.Body) {
		err = ErrApprovalRequired
	}
	return
}

// CheckPreparedPlan rechecks the exact read-only admission before outputs import.
func (m *Manager) CheckPreparedPlan(ctx context.Context, n int, p *CheckpointReconciliation) error {
	if p == nil || !p.prepared || p.ClaimRequired {
		return ErrClaimFence
	}
	fresh, err := m.PrepareCheckpoint(ctx, n, p.owner, p.expected, p.candidate, false)
	if err != nil {
		return err
	}
	if fresh.revision != p.revision || !sameCheckpointState(fresh.State, p.State) {
		return ErrClaimFence
	}
	return nil
}

// RecordCheckpointPlan atomically installs both reports under the prepared fence.
// Ordinary artifact writers cannot manufacture this coordinator-only identity.
func (m *Manager) RecordCheckpointPlan(ctx context.Context, n int, p *CheckpointReconciliation, plan, test contract.Artifact) (*IssueState, error) {
	if err := m.CheckPreparedPlan(ctx, n, p); err != nil {
		return nil, err
	}
	if plan.Type != contract.ArtifactDesign || test.Type != contract.ArtifactTestPlan {
		return nil, fmt.Errorf("expected design and test-plan pair")
	}
	return m.mutate(ctx, n, p.owner, func(st *IssueState) error {
		if !sameCheckpointState(st, p.State) {
			return ErrClaimFence
		}
		if directIssue(st) && st.HumanPlanReview != nil && !planReservationMatches(st, st.HumanPlanReview) {
			return ErrApprovalRequired
		}
		if st.PendingPlanReview != nil || (directIssue(st) && st.HumanPlanReview != nil) {
			oldPlan, oldTest := "", ""
			for _, a := range st.Artifacts {
				if a.Type == contract.ArtifactDesign {
					oldPlan = contract.CanonicalArtifactBody(a.Body)
				}
				if a.Type == contract.ArtifactTestPlan {
					oldTest = contract.CanonicalArtifactBody(a.Body)
				}
			}
			if oldPlan == contract.CanonicalArtifactBody(plan.Body) && oldTest == contract.CanonicalArtifactBody(test.Body) {
				return fmt.Errorf("checkpoint re-plan is unchanged after CHANGES_REQUESTED")
			}
		}
		pair := []contract.Artifact{plan, test}
		for i := range pair {
			a := &pair[i]
			if strings.TrimSpace(a.Body) == "" {
				return fmt.Errorf("empty checkpoint plan report")
			}
			a.Version = 1
			a.ID = fmt.Sprintf("art-%d-%s", n, a.Type)
			a.IssueNumber = n
			a.CreatedAt = m.nowFn()
			meta := map[string]string{}
			for k, v := range a.Metadata {
				if !strings.HasPrefix(k, "checkpoint_plan_") {
					meta[k] = v
				}
			}
			for k, v := range map[string]string{"origin": "coordinator", "claim": st.Lease.ClaimID, "task": st.TaskDigest, "policy": st.PolicyDigest, "base": st.BaseSHA, "predecessor": p.expected, "candidate": p.candidate} {
				meta["checkpoint_plan_"+k] = v
			}
			if st.PolicyTreeDigest != "" {
				meta["checkpoint_plan_policy_tree"] = st.PolicyTreeDigest
			}
			a.Metadata = meta
			RecordRoleAttestation(st, contract.RoleAttestation{Role: "planner", Agent: meta["agent"], Harness: meta["harness"], HarnessVersion: meta["harness_version"], Model: meta["model"], ExecutionMode: meta["execution_mode"], Authentication: meta["authentication"], Sandbox: meta["sandbox"], ArtifactType: string(a.Type), ArtifactDigest: hash(a.Body)})
			recordRoleRun(st, a)
			a.Metadata["content_sha256"] = hash(a.Body)
			if err := contract.ValidateArtifact(a); err != nil {
				return err
			}
		}
		kept := st.Artifacts[:0]
		for _, a := range st.Artifacts {
			if a.Type != contract.ArtifactDesign && a.Type != contract.ArtifactTestPlan && a.Type != contract.ArtifactReview {
				kept = append(kept, a)
			}
		}
		st.Artifacts = append(kept, pair...)
		markPlanPair(st)
		st.PlanReservation = nil
		return nil
	})
}

func (m *Manager) checkCheckpointPlanObservation(ctx context.Context, st *IssueState) error {
	expected, candidate, present, err := CheckpointPlanHeads(st)
	if !present {
		return nil
	}
	if err != nil {
		return err
	}
	if st.HeadSHA == candidate {
		return nil
	}
	if st.Lease.Status != "active" || !st.Lease.ExpiresAt.After(m.nowFn()) {
		return ErrClaimFence
	}
	remote := m.UpstreamRemote()
	_, local := m.store.(*coordination.FileStore)
	if local {
		remote = "."
	}
	o := workspace.InspectCheckpoint(ctx, m.repoRoot, remote, st.Lease.IssueNumber, st.Branch, expected)
	if o.Error != "" || o.Worktree.Error != "" || o.LocalBranch.Error != "" || (!local && (o.RemoteBranch.Error != "" || o.RemoteBranch.SHA != expected)) || o.Dirty || o.Rebasing || o.AttachedBranch != st.Branch || o.Worktree.SHA != candidate || o.LocalBranch.SHA != candidate {
		return ErrClaimFence
	}
	return nil
}

// Prepare ignored, untracked inputs from the exact state reviewed by the gate.
func prepareCheckpointValidation(ctx context.Context, snapshot string, st *IssueState) error {
	bindings := ReviewBindings(st)
	inputs := []struct {
		name, digest string
		kind         contract.ArtifactType
		body         string
	}{
		{name: ".swarm/active/PLAN.md", digest: "plan_digest", kind: contract.ArtifactDesign},
		{name: ".swarm/active/TEST_PLAN.md", digest: "test_plan_digest", kind: contract.ArtifactTestPlan},
	}
	for i := range inputs {
		in := &inputs[i]
		for _, a := range st.Artifacts {
			if a.Type == in.kind {
				in.body = a.Body
				if a.Metadata["content_sha256"] != hash(a.Body) {
					return ErrApprovalRequired
				}
			}
		}
		if in.body == "" || bindings[in.digest] != hash(in.body) {
			return ErrApprovalRequired
		}
		tracked, err := checkpointGit(ctx, snapshot, "ls-files", "--", in.name)
		if err != nil || tracked != "" {
			return fmt.Errorf("checkpoint validation input must be untracked: %s", in.name)
		}
		if _, err = checkpointGit(ctx, snapshot, "check-ignore", "--", in.name); err != nil {
			return fmt.Errorf("checkpoint validation input must be ignored: %s", in.name)
		}
	}
	for _, in := range inputs {
		if err := safefs.WriteFile(snapshot, in.name, []byte(in.body), 0600); err != nil {
			return err
		}
	}
	return nil
}
