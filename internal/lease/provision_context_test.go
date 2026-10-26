package lease

import (
	"testing"

	"github.com/supernovae/swarmos/internal/workspace"
)

func TestIssueStateProvisionContext(t *testing.T) {
	dropped := &workspace.DroppedCheckpoint{Branch: "swarm/issue-1-x", HeadSHA: "c"}
	for _, tc := range []struct {
		name     string
		st       IssueState
		fork     bool
		previous string
	}{
		{"untouched re-approval", IssueState{BaseSHA: "b", PreviousBaseSHA: "a"}, false, "a"},
		{"no previous base", IssueState{BaseSHA: "b"}, false, ""},
		{"same base", IssueState{BaseSHA: "b", PreviousBaseSHA: "b"}, false, ""},
		{"recorded checkpoint", IssueState{BaseSHA: "b", PreviousBaseSHA: "a", HeadSHA: "c"}, false, ""},
		{"fork mode", IssueState{BaseSHA: "b", PreviousBaseSHA: "a"}, true, ""},
		{"fork state", IssueState{BaseSHA: "b", PreviousBaseSHA: "a", ForkRepo: "me/repo"}, false, ""},
		{"pull request", IssueState{BaseSHA: "b", PreviousBaseSHA: "a", PRNumber: 3}, false, ""},
		{"dropped checkpoint", IssueState{BaseSHA: "b", PreviousBaseSHA: "a", DroppedCheckpoint: dropped}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := tc.st.ProvisionContext(tc.fork)
			if pc.PreviousBase != tc.previous || pc.Dropped != tc.st.DroppedCheckpoint {
				t.Fatalf("got %+v", pc)
			}
		})
	}
}
