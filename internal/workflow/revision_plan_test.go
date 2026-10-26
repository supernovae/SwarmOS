package workflow

import (
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestRevisionTaskPromptScope(t *testing.T) {
	st := &lease.IssueState{RevisionPlan: &workspace.RevisionPlanContext{}}
	for _, tc := range []struct {
		name     string
		pr       int
		revision *lease.PRRevision
		want     bool
	}{
		{"issue", 0, nil, false}, {"PR", 1, &lease.PRRevision{Intent: "work"}, true}, {"rebase", 1, &lease.PRRevision{Intent: "rebase"}, false}, {"after submit", 1, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st.PRRevision = tc.revision
			got := revisionTaskPrompt("task", st, tc.pr)
			if strings.Contains(got, "REVISION_CONTEXT.md") != tc.want {
				t.Fatal(got)
			}
		})
	}
	st.RevisionPlan = nil
	if got := revisionTaskPrompt("first plan", st, 1); got != "first plan" {
		t.Fatal(got)
	}
}
