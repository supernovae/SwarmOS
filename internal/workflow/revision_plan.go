package workflow

import (
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workspace"
)

func prRevisionContext(st *lease.IssueState) *workspace.RevisionPlanContext {
	if st.PRRevision != nil && st.PRRevision.Intent != "work" {
		return nil
	}
	return st.RevisionPlan
}

func revisionTaskPrompt(prompt string, st *lease.IssueState, prNumber int) string {
	if prNumber != 0 && prRevisionContext(st) != nil {
		return prompt + "\nThis is a PR revision. Read .swarm/active/REVISION_CONTEXT.md and the original approved plan/test plan; follow the revision contract over the existing implementation."
	}
	return prompt
}
