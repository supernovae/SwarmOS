package pr

import (
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"strings"
	"testing"
)

func TestReviewProjectionPreservesTextAndUpdatesWithoutRebase(t *testing.T) {
	tr := tracker.NewLocalTracker(t.TempDir())
	original := "human introduction\n\nhuman footer"
	pr, err := tr.CreateDraftPR(t.Context(), tracker.DraftPRRequest{Title: "title", Body: original, Head: "feature", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	st := &lease.IssueState{Artifacts: []contract.Artifact{{Type: contract.ArtifactImplementation, Verdict: "PASS", Body: "Report\n## What we learned\n- reviewer-lesson", Metadata: map[string]string{"operator_steered": "true", "guidance_count": "2", "guidance_sha256": "hash"}}}}
	if err := ProjectReviewEvidence(t.Context(), tr, st, pr.Number); err != nil {
		t.Fatal(err)
	}
	first, err := tr.GetPullRequest(t.Context(), pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Body, original) || !strings.Contains(first.Body, "Operator-steered: 2") || !strings.Contains(first.Body, "reviewer-lesson") {
		t.Fatal(first.Body)
	}
	if err := ProjectReviewEvidence(t.Context(), tr, st, pr.Number); err != nil {
		t.Fatal(err)
	}
	retry, _ := tr.GetPullRequest(t.Context(), pr.Number)
	if retry.Body != first.Body {
		t.Fatal("retry changed projection")
	}
	st.Artifacts[0].Body = "new evidence"
	if err := ProjectReviewEvidence(t.Context(), tr, st, pr.Number); err != nil {
		t.Fatal(err)
	}
	updated, _ := tr.GetPullRequest(t.Context(), pr.Number)
	if strings.Count(updated.Body, reviewStart) != 1 || !strings.Contains(updated.Body, "new evidence") || strings.Contains(updated.Body, "reviewer-lesson") || !strings.Contains(updated.Body, original) {
		t.Fatal(updated.Body)
	}
}
