package lease

import (
	"fmt"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workspace"
)

// captureRevisionPlan runs only inside the fenced coordinator transition.
func captureRevisionPlan(st *IssueState) error {
	if c := st.RevisionPlan; c != nil && (c.TaskDigest != st.TaskDigest || c.PolicyDigest != st.PolicyDigest || c.PolicyTreeDigest != st.PolicyTreeDigest || c.BaseSHA != st.BaseSHA) {
		st.RevisionPlan = nil
		return nil
	}
	var plan, test string
	for _, a := range st.Artifacts {
		switch a.Type {
		case contract.ArtifactReview:
			if a.Verdict == "CHANGES_REQUESTED" {
				return nil
			}
		case contract.ArtifactDesign:
			plan = a.Body
		case contract.ArtifactTestPlan:
			test = a.Body
		}
	}
	if plan == "" || test == "" || st.TaskDigest == "" || st.PolicyDigest == "" {
		return nil
	}
	if st.EffectiveGate != contract.GateNone && verifyReviewBinding(st, st.EffectiveGate) != nil {
		return nil
	}
	p := workspace.PlanPair{Plan: plan, TestPlan: test, PlanDigest: hash(plan), TestPlanDigest: hash(test)}
	if st.RevisionPlan == nil {
		st.RevisionPlan = &workspace.RevisionPlanContext{Original: p, TaskDigest: st.TaskDigest, PolicyDigest: st.PolicyDigest, PolicyTreeDigest: st.PolicyTreeDigest, BaseSHA: st.BaseSHA}
		return nil
	}
	c := st.RevisionPlan
	if c.Original.PlanDigest == p.PlanDigest && c.Original.TestPlanDigest == p.TestPlanDigest {
		return nil
	}
	for _, old := range c.Accepted {
		if old.PlanDigest == p.PlanDigest && old.TestPlanDigest == p.TestPlanDigest {
			return nil
		}
	}
	// Refuse to discard earlier applied context or grow history indefinitely.
	if len(c.Accepted) >= 16 {
		return fmt.Errorf("revision context limit reached; a new full-plan WorkUnit is required")
	}
	c.Accepted = append(c.Accepted, p)
	return nil
}
