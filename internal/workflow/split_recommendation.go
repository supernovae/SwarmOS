package workflow

import (
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
)

// SplitRecommendationLine is the exact PLAN.md line a planner writes when the
// WorkUnit cannot finish within one claim. It only stops a run after planning;
// it never approves, decomposes, or changes the WorkUnit.
const SplitRecommendationLine = "Split recommendation: required"

// PlanRecommendsSplit reports whether plan has the split recommendation on a
// line of its own (trailing whitespace and CRLF allowed). Coordinator guidance
// is ignored, as are quoted, inline, prefixed, or differently cased mentions.
func PlanRecommendsSplit(plan string) bool {
	for _, line := range strings.Split(canonicalReport(plan), "\n") {
		if strings.TrimRight(line, " \t\r") == SplitRecommendationLine {
			return true
		}
	}
	return false
}

// splitRecommended reports whether the latest recorded design artifact
// recommends a split. Only the fenced artifact counts, never worktree files.
func splitRecommended(st *lease.IssueState) bool {
	if st == nil {
		return false
	}
	for i := len(st.Artifacts) - 1; i >= 0; i-- {
		if st.Artifacts[i].Type == contract.ArtifactDesign {
			return PlanRecommendsSplit(st.Artifacts[i].Body)
		}
	}
	return false
}
