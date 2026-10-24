package pr

import (
	"context"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

func TestRebaseProjectionPreservesHumanTextAndRetries(t *testing.T) {
	ctx := context.Background()
	tr := tracker.NewLocalTracker(t.TempDir())
	pr, err := tr.CreateDraftPR(ctx, tracker.DraftPRRequest{Title: "title", Body: "human introduction\n\nhuman footer", Head: "feature", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	st := &lease.IssueState{CarryReason: "approved plan carried forward", LastRebase: &lease.RebaseEvidence{OldHead: "old", NewHead: "new", OldBase: "base", NewBase: "next", Status: "completed", PatchIdentity: "unchanged", Verified: true}}
	if err := ProjectRebase(ctx, tr, st, pr.Number); err != nil {
		t.Fatal(err)
	}
	first, err := tr.GetPullRequest(ctx, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Body, "human introduction\n\nhuman footer") || !strings.Contains(first.Body, "old -> new") {
		t.Fatal(first.Body)
	}
	if err := ProjectRebase(ctx, tr, st, pr.Number); err != nil {
		t.Fatal(err)
	}
	second, err := tr.GetPullRequest(ctx, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if first.Body != second.Body || strings.Count(second.Body, rebaseStart) != 1 {
		t.Fatal("retry changed projection")
	}
}
