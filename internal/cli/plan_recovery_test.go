package cli

import (
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/lease"
)

func TestPlanReviewRecovery(t *testing.T) {
	st := &lease.IssueState{PendingPlanReview: &lease.PlanRejection{}, NextPhaseReason: "CHANGES_REQUESTED"}
	st.Lease.IssueNumber = 12
	opts := &RootOptions{Backend: "github", Repo: "owner/repo", RepoRoot: "a'b", Fork: true, ForkRemote: "fork"}
	for _, resource := range []string{"issue", "pr"} {
		text := planReviewRecovery(opts, st, resource, 37)
		want := "issue review-plan 12"
		if resource == "pr" {
			want = "pr work 37 --phase review"
			if strings.Contains(text, "issue review-plan") {
				t.Fatal(text)
			}
		}
		for _, part := range []string{want, resource + " work 37 --phase plan", "--root 'a'\\''b'", "--fork", "per-revision cap"} {
			if !strings.Contains(text, part) {
				t.Fatalf("missing %q: %s", part, text)
			}
		}
	}
	if planReviewRecovery(opts, nil, "pr", 37) != "" || planReviewRecovery(opts, &lease.IssueState{}, "pr", 37) != "" {
		t.Fatal("spurious recovery")
	}
}
