package lease

import (
	"testing"

	"github.com/supernovae/swarmos/internal/workspace"
)

func TestIssueStateProvisionContext(t *testing.T) {
	t.Parallel()
	dropped := &workspace.DroppedCheckpoint{Branch: "swarm/issue-1-x", HeadSHA: "c"}
	for _, tc := range []struct {
		name     string
		st       IssueState
		fork     bool
		previous string
	}{
		{"untouched re-approval", IssueState{BaseSHA: "b", PreviousBaseSHA: "a"}, false, "a"},
		{"no previous base", IssueState{BaseSHA: "b"}, false, ""},
		{"same base", IssueState{BaseSHA: "b", PreviousBaseSHA: "b"}, false, "b"},
		{"recorded checkpoint", IssueState{BaseSHA: "b", PreviousBaseSHA: "a", HeadSHA: "c"}, false, ""},
		{"fork mode", IssueState{BaseSHA: "b", PreviousBaseSHA: "a"}, true, ""},
		{"fork state", IssueState{BaseSHA: "b", PreviousBaseSHA: "a", ForkRepo: "me/repo"}, false, ""},
		{"pull request", IssueState{BaseSHA: "b", PreviousBaseSHA: "a", PRNumber: 3}, false, ""},
		{"pull request revision", IssueState{BaseSHA: "b", PreviousBaseSHA: "a", PRRevision: &PRRevision{}}, false, ""},
		{"dropped checkpoint", IssueState{BaseSHA: "b", PreviousBaseSHA: "a", DroppedCheckpoint: dropped}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pc := tc.st.ProvisionContext(tc.fork)
			if pc.PreviousBase != tc.previous || pc.Dropped != tc.st.DroppedCheckpoint {
				t.Fatalf("got %+v", pc)
			}
		})
	}
}

func TestIssueStateProvisionContextFreshStart(t *testing.T) {
	t.Parallel()
	fresh := &workspace.FreshStart{Branch: "swarm/issue-1-restart-c", AbandonedBranch: "swarm/issue-1-x", AbandonedHeadSHA: "c"}
	for _, tc := range []struct {
		name string
		st   IssueState
		fork bool
		want *workspace.FreshStart
	}{
		{"same repository", IssueState{FreshStart: fresh}, false, fresh},
		{"fork mode", IssueState{FreshStart: fresh}, true, nil},
		{"fork state", IssueState{FreshStart: fresh, ForkRepo: "me/repo"}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.st.ProvisionContext(tc.fork).FreshStart; got != tc.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
