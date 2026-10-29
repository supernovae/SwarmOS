package pr

import (
	"context"
	"fmt"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

const reviewStart = "<!-- swarm:review-evidence:start -->"
const reviewEnd = "<!-- swarm:review-evidence:end -->"

// ProjectReviewEvidence refreshes coordinator-owned issue and review evidence.
func ProjectReviewEvidence(ctx context.Context, tr tracker.Tracker, st *lease.IssueState, number int) error {
	if st == nil || len(st.Artifacts) == 0 {
		return nil
	}
	pr, err := tr.GetPullRequest(ctx, number)
	if err != nil {
		return err
	}
	issue := st.Lease.IssueNumber
	projection := lease.PublicProjectionContext(tr)
	ref := lease.DescriptionStateReference(pr.Body, issue)
	outside := pr.Body
	sections := map[string]string{"review-evidence": "## SwarmOS review evidence\n" + lease.DescriptionProductionSummary(st, projection) + lease.DescriptionArtifacts(st, projection) + lease.DescriptionRetrieval(issue, ref)}
	// Refresh previously generated issue evidence from structured state as well:
	// copying an old managed section could republish private local actors.
	if strings.Contains(outside, "<!-- swarm:issue-description:start -->") && lease.DescriptionIssueNeedsProjection(outside) {
		issueRecord, err := tr.GetIssue(ctx, issue)
		if err != nil {
			return err
		}
		unit, err := contract.ParseWorkUnitFromIssue(issueRecord.Number, issueRecord.Title, issueRecord.Body)
		if err != nil {
			return err
		}
		if st.TaskDigest != "" && contract.DigestWorkUnit(unit) != st.TaskDigest {
			return fmt.Errorf("cannot project evidence for a changed WorkUnit")
		}
		sections["issue-description"] = lease.DescriptionIssueSection(unit, st, ref, projection)
	}
	body, err := lease.ComposePRDescriptionSections(outside, sections, ref, issue)
	if err != nil {
		return err
	}
	if body == pr.Body {
		return nil
	}
	_, err = tr.UpdateDraftPR(ctx, number, pr.Title, body)
	return err
}
