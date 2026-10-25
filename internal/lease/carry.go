package lease

import (
	"context"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workspace"
)

func reviewBaseMatches(st *IssueState, review *contract.Artifact) bool {
	md := review.Metadata
	if md["base_sha"] == st.BaseSHA && md["policy_digest"] == st.PolicyDigest {
		return true
	}
	b := st.ApprovalTransition
	return b != nil && b.OriginBase != "" && b.OriginPolicy != "" &&
		b.OriginBase == md["base_sha"] && b.OriginPolicy == md["policy_digest"] &&
		b.TargetBase == st.BaseSHA && b.TargetPolicy == st.PolicyDigest &&
		b.TaskDigest == st.TaskDigest && b.PolicyTreeDigest != "" &&
		b.PolicyTreeDigest == st.PolicyTreeDigest && b.PolicyTreeDigest == md["policy_tree_digest"] &&
		b.ReviewDigest == hash(review.Body)
}

// carryApproval runs inside coordinator approval or checked fresh acquisition, before replacing
// the old baseline. All inputs come from trusted Git objects and stored state.
func (m *Manager) carryApproval(ctx context.Context, st *IssueState, wu *contract.WorkUnit, base, policy, tree string) {
	m.carryApprovalEvidence(ctx, st, wu, base, policy, tree, false)
}

func (m *Manager) carryApprovalEvidence(ctx context.Context, st *IssueState, wu *contract.WorkUnit, base, policy, tree string, requireReview bool) {
	st.CarryReason = ""
	st.ChangedPolicyPaths = nil
	var review *contract.Artifact
	for i := range st.Artifacts {
		if st.Artifacts[i].Type == contract.ArtifactReview {
			review = &st.Artifacts[i]
		}
	}
	switch {
	case st.TaskDigest != contract.DigestWorkUnit(wu):
		st.CarryReason = "WorkUnit changed"
	case !requireReview && st.EffectiveGate == contract.GateNone && st.PolicyTreeDigest != "" && st.PolicyTreeDigest == tree:
		clearHeadEvidence(st)
		st.CarryReason = "approved plan carried forward"
		return
	case st.PolicyTreeDigest == "" || review == nil || review.Metadata["policy_tree_digest"] == "":
		st.CarryReason = "review lacks policy tree metadata"
	case st.PolicyTreeDigest != tree:
		st.CarryReason = "policy files changed"
		paths, err := workspace.ChangedPolicyPaths(ctx, m.repoRoot, st.BaseSHA, base)
		if err != nil {
			st.CarryReason = "policy files changed (changed paths unavailable; previous base could not be compared)"
		} else {
			st.ChangedPolicyPaths = paths
		}
		// Retain exact plan bytes as stale evidence for an explicit review
		// under the newly approved policy. VerifyApproval still rejects the
		// old base/policy bindings; this does not carry its verdict forward.
		if st.PRNumber > 0 && st.ForkRepo == "" && st.CarryReason == "policy files changed" {
			st.ApprovalTransition = nil
			clearHeadEvidence(st)
			return
		}
	case (requireReview && verifyReviewBinding(st, contract.GateAgent) != nil) || VerifyApproval(st, wu) != nil:
		st.CarryReason = "plan approval is missing or stale"
	default:
		ancestor, err := workspace.BaseIsAncestor(ctx, m.repoRoot, st.BaseSHA, base)
		if err != nil {
			st.CarryReason = "base ancestry could not be verified"
		} else if !ancestor {
			st.CarryReason = "base not an ancestor"
		} else {
			st.ApprovalTransition = &ApprovalTransition{OriginBase: review.Metadata["base_sha"], OriginPolicy: review.Metadata["policy_digest"], TargetBase: base, TargetPolicy: policy, TaskDigest: st.TaskDigest, PolicyTreeDigest: tree, ReviewDigest: hash(review.Body)}
			clearHeadEvidence(st)
			st.CarryReason = "approved plan carried forward"
			return
		}
	}
	invalidatePRRevisionEvidence(st)
}

func clearHeadEvidence(st *IssueState) {
	kept := st.Artifacts[:0]
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactDesign || a.Type == contract.ArtifactTestPlan || a.Type == contract.ArtifactReview {
			kept = append(kept, a)
		}
	}
	st.Artifacts = kept
	atts := st.Attestations[:0]
	for _, a := range st.Attestations {
		for _, art := range st.Artifacts {
			if a.ArtifactType == string(art.Type) && a.ArtifactDigest == hash(art.Body) {
				atts = append(atts, a)
				break
			}
		}
	}
	st.Attestations = atts
}

// rebaseEvidence rechecks the current authenticated plan. Revision intent is
// merely a request to retain eligible evidence, never an approval grant.
func (m *Manager) rebaseEvidence(ctx context.Context, st *IssueState, n int) {
	issue, err := m.tracker.GetIssue(ctx, n)
	if err == nil {
		wu, parseErr := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
		if parseErr == nil && st.PolicyTreeDigest != "" && VerifyApproval(st, wu) == nil {
			if st.EffectiveGate == contract.GateNone {
				clearHeadEvidence(st)
				st.CarryReason = "approved plan carried forward"
				return
			}
			for _, a := range st.Artifacts {
				if a.Type == contract.ArtifactReview && a.Metadata["policy_tree_digest"] == st.PolicyTreeDigest {
					clearHeadEvidence(st)
					if st.CarryReason == "policy files changed" || st.CarryReason == "approved plan re-reviewed" {
						st.CarryReason = "approved plan re-reviewed"
					} else {
						st.CarryReason = "approved plan carried forward"
					}
					return
				}
			}
		}
	}
	if st.CarryReason == "" || st.CarryReason == "approved plan carried forward" {
		st.CarryReason = "plan approval is missing or stale"
	}
	invalidatePRRevisionEvidence(st)
}

func revisionIntent(intent []string) string {
	if len(intent) == 1 && intent[0] == "rebase" {
		return "rebase"
	}
	return "work"
}
