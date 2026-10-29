package pr

import (
	"context"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

const reviewStart = "<!-- swarm:review-evidence:start -->"
const reviewEnd = "<!-- swarm:review-evidence:end -->"

// ProjectReviewEvidence updates only the coordinator-owned review attachment.
func ProjectReviewEvidence(ctx context.Context, tr tracker.Tracker, st *lease.IssueState, number int) error {
	if st == nil || len(st.Artifacts) == 0 {
		return nil
	}
	pr, err := tr.GetPullRequest(ctx, number)
	if err != nil {
		return err
	}
	issue := st.Lease.IssueNumber
	ref := lease.DescriptionStateReference(pr.Body, issue)
	body, err := lease.ComposePRDescription(pr.Body, "review-evidence", "## SwarmOS review evidence\n"+lease.DescriptionArtifacts(st)+lease.DescriptionRetrieval(issue, ref), ref, issue)
	if err != nil {
		return err
	}
	if body == pr.Body {
		return nil
	}
	_, err = tr.UpdateDraftPR(ctx, number, pr.Title, body)
	return err
}
