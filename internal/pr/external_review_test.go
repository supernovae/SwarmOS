package pr

import (
	"context"
	"testing"

	"github.com/supernovae/swarmos/internal/tracker"
)

// hostedReviewTracker reports reviews and checks the way GitHub-hosted agents
// such as review bots leave them on a pull request.
type hostedReviewTracker struct {
	*tracker.LocalTracker
	reviews []tracker.PullRequestReview
	checks  []tracker.CheckRunResult
}

func (h *hostedReviewTracker) GetPullRequestReviews(context.Context, int) ([]tracker.PullRequestReview, error) {
	return h.reviews, nil
}

func (h *hostedReviewTracker) GetCheckStatus(context.Context, string) ([]tracker.CheckRunResult, error) {
	return h.checks, nil
}

func TestStatusMarksHostedReviewsCurrentOrStale(t *testing.T) {
	s, _, number, base := fixture(t)
	local := s.Tracker.(*tracker.LocalTracker)
	s.Tracker = &hostedReviewTracker{
		LocalTracker: local,
		reviews: []tracker.PullRequestReview{
			{Author: "review-bot[bot]", State: "COMMENTED", CommitSHA: base},
			{Author: "maintainer", State: "APPROVED", CommitSHA: "0000000000000000000000000000000000000000"},
			{Author: "pending", State: "PENDING"},
		},
		checks: []tracker.CheckRunResult{{Name: "Hosted Review", Status: "completed", Conclusion: "success", App: "review-app"}},
	}
	r, err := s.Status(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Reviews) != 3 || !r.Reviews[0].Current || r.Reviews[1].Current || r.Reviews[2].Current {
		t.Fatalf("review freshness against head %s: %+v", base, r.Reviews)
	}
	if len(r.Checks) != 1 || r.Checks[0].App != "review-app" {
		t.Fatalf("check reporter lost: %+v", r.Checks)
	}
}
