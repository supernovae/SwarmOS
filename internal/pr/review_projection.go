package pr

import (
	"context"
	"fmt"
	"strings"

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
	var b strings.Builder
	b.WriteString(reviewStart + "\n## SwarmOS review evidence\n\n")
	for _, a := range st.Artifacts {
		fmt.Fprintf(&b, "### %s: %s\n\n%s\n\n", a.Type, a.Verdict, a.Summary)
		if a.Metadata["operator_steered"] == "true" {
			fmt.Fprintf(&b, "Operator-steered: %s messages; SHA-256 `%s`.\n\n", a.Metadata["guidance_count"], a.Metadata["guidance_sha256"])
		}
		b.WriteString(a.Body + "\n\n")
	}
	b.WriteString(reviewEnd)
	body := pr.Body
	if start := strings.Index(body, reviewStart); start >= 0 {
		end := strings.Index(body[start:], reviewEnd)
		if end < 0 {
			return fmt.Errorf("managed review evidence section is incomplete")
		}
		body = body[:start] + b.String() + body[start+end+len(reviewEnd):]
	} else {
		body = strings.TrimRight(body, "\n") + "\n\n" + b.String() + "\n"
	}
	if body == pr.Body {
		return nil
	}
	_, err = tr.UpdateDraftPR(ctx, number, pr.Title, body)
	return err
}
