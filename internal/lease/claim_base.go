package lease

import (
	"context"
	"fmt"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/workspace"
)

// ClaimBaseApprovalError identifies a fresh candidate requiring maintainer action.
type ClaimBaseApprovalError struct {
	IssueNumber int
	Reason      string
}

func (e *ClaimBaseApprovalError) Error() string {
	return fmt.Sprintf("issue #%d: %s; run swarm issue approve %d", e.IssueNumber, e.Reason, e.IssueNumber)
}
func (e *ClaimBaseApprovalError) Unwrap() error { return ErrApprovalRequired }

func freshClaimBase(st *IssueState, pr int) bool {
	if pr != 0 || st.ForkRepo != "" || st.Branch != "" || st.HeadSHA != "" ||
		st.PRNumber != 0 || st.PRURL != "" || st.PRRevision != nil ||
		st.SubmittedBaseSHA != "" || st.SubmittedHeadSHA != "" || st.DroppedCheckpoint != nil {
		return false
	}
	for _, a := range st.Artifacts {
		if a.Type != contract.ArtifactDesign && a.Type != contract.ArtifactTestPlan && a.Type != contract.ArtifactReview {
			return false
		}
	}
	return true
}

func (m *Manager) prepareClaimBase(ctx context.Context, n int, original *IssueState, wu *contract.WorkUnit, cfg *contract.SwarmConfig) (*IssueState, error) {
	base, err := m.trustedBase()
	if err != nil {
		return nil, err
	}
	if base == original.BaseSHA {
		return original, nil
	}
	refuse := func(reason string) (*IssueState, error) {
		return nil, &ClaimBaseApprovalError{IssueNumber: n, Reason: reason}
	}
	wf, err := harness.LoadWorkflowAt(m.repoRoot, wu.Workflow, original.BaseSHA)
	if err != nil {
		return nil, err
	}
	gate, err := contract.ResolveGate(wu.Gate, wf.DefaultGate, cfg.DefaultGate)
	if err != nil {
		return nil, err
	}
	if gate == contract.GateHuman || original.EffectiveGate == contract.GateHuman {
		return refuse("human gate requires approval of the new base")
	}
	tree, err := workspace.PolicyTreeDigest(ctx, m.repoRoot, base)
	if err != nil {
		return refuse("new policy tree unavailable")
	}
	oldTree, err := workspace.PolicyTreeDigest(ctx, m.repoRoot, original.BaseSHA)
	if err != nil || tree == "" || original.PolicyTreeDigest == "" || original.PolicyTreeDigest != tree || oldTree != tree {
		return refuse("committed policy tree changed or is missing")
	}
	ancestor, err := workspace.BaseIsAncestor(ctx, m.repoRoot, original.BaseSHA, base)
	if err != nil || !ancestor {
		return refuse("old base ancestry could not be verified")
	}
	nextCfg, err := harness.LoadConfigAt(m.repoRoot, base)
	if err != nil {
		return nil, err
	}
	policy := contract.DigestApprovedPolicy(base, nextCfg)
	st := *original
	st.Artifacts = append([]contract.Artifact(nil), original.Artifacts...)
	st.Attestations = append([]contract.RoleAttestation(nil), original.Attestations...)
	hasEvidence := len(st.Artifacts) > 0
	if hasEvidence {
		m.carryApprovalEvidence(ctx, &st, wu, base, policy, tree, true)
		if st.ApprovalTransition == nil || st.CarryReason != "approved plan carried forward" {
			return refuse(st.CarryReason)
		}
	} else {
		st.ApprovalTransition = nil
	}
	st.PreviousBaseSHA, st.ClaimPreviousBase, st.BaseSHA = original.BaseSHA, original.BaseSHA, base
	st.PolicyDigest, st.PolicyTreeDigest = policy, tree
	if !TaskApprovalCurrent(&st, wu, nextCfg) {
		return refuse("task approval no longer matches")
	}
	if hasEvidence {
		if err := verifyReviewBinding(&st, contract.GateAgent); err != nil {
			return refuse("carried review binding is invalid")
		}
		if err := VerifyApproval(&st, wu); err != nil {
			return refuse("carried approval is invalid")
		}
	}
	return &st, nil
}
