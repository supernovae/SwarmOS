package lease

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

type checkpointReconciler interface {
	CompareAndSwapAndReconcileCheckpoint(context.Context, int, string, []byte, string, string, string) (string, error)
}

type CheckpointReconciliation struct {
	State              *IssueState               `json:"state"`
	Observation        workspace.CheckpointState `json:"checkpoint"`
	DryRun             bool                      `json:"dry_run"`
	ValidationRequired bool                      `json:"validation_required"`
	ReviewRequired     bool                      `json:"review_required"`
	PatchIdentity      string                    `json:"patch_identity"`
}

// ReconcileCheckpoint accepts only an explicit, clean local task candidate. It
// never retries publication against a newer state or remote branch.
func (m *Manager) ReconcileCheckpoint(ctx context.Context, n int, owner, expected, candidate string, dryRun bool) (*CheckpointReconciliation, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("checkpoint reconciliation is upstream-only; use the fork PR revision workflow")
	}
	publisher, ok := m.store.(checkpointReconciler)
	if !ok {
		return nil, fmt.Errorf("checkpoint reconciliation requires a Git-backed upstream store; local stores are unsupported")
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
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if err = m.checkStateClaim(ctx, n, owner, st); err != nil {
		return nil, err
	}
	if st.ForkRepo != "" || st.PRNumber != 0 || st.PRRevision != nil || st.DroppedCheckpoint != nil {
		return nil, fmt.Errorf("checkpoint reconciliation requires upstream issue work without submitted PR or pending recovery; use the PR revision workflow for linked PRs")
	}
	if st.HeadSHA != expected || st.Branch == "" {
		return nil, fmt.Errorf("%w: authoritative checkpoint %s differs from expected %s", ErrClaimFence, st.HeadSHA, expected)
	}
	inspect := func() (workspace.CheckpointState, error) {
		o := workspace.InspectCheckpoint(ctx, m.repoRoot, m.UpstreamRemote(), n, st.Branch, expected)
		if o.Error != "" || o.Worktree.Error != "" || o.LocalBranch.Error != "" || o.RemoteBranch.Error != "" ||
			o.Dirty || o.Rebasing || o.AttachedBranch != st.Branch || o.Worktree.SHA != candidate || o.LocalBranch.SHA != candidate || o.RemoteBranch.SHA != expected {
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
		if dryRun {
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
	result := &CheckpointReconciliation{State: st, Observation: observation, DryRun: dryRun, ValidationRequired: true, ReviewRequired: reviewRequired, PatchIdentity: identity}
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
	if _, err = publisher.CompareAndSwapAndReconcileCheckpoint(ctx, n, rev, raw, st.Branch, candidate, expected); err != nil {
		return nil, fmt.Errorf("%w: checkpoint publication rejected: %w", ErrClaimFence, err)
	}
	m.project(ctx, n, st)
	result.State = st
	result.ValidationRequired = false
	return result, nil
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
