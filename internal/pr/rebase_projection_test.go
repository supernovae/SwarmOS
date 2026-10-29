package pr

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

func TestRebaseProjectionPreservesHumanTextAndRetries(t *testing.T) {
	t.Parallel()
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
	if first.Body != second.Body || strings.Count(second.Body, rebaseStart) != 1 || strings.Count(second.Body, rebaseEnd) != 1 {
		t.Fatal("retry changed projection")
	}
}

func TestRebaseSummaryDroppedPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		state     string // JSON keeps legacy states honest about missing fields
		want      []string
		forbidden []string
	}{
		{"dropped paths warn with recovery", `{"last_rebase":{"old_head":"old","new_head":"new","old_base":"b","new_base":"n","status":"completed","patch_identity":"changed","verified":true,"dropped_paths":["a.txt","tab\tname"],"dropped_paths_verified":true}}`,
			[]string{"Warning: no longer changed: a.txt\n", `Warning: no longer changed: "tab\tname"` + "\n", "git diff old new -- <path>", "swarm pr work 7", "swarm pr rebase 7 --abort", "paused rebase only"}, []string{"unavailable\n"}},
		{"fork uses fork flags", `{"fork_repo":"bob/swarm","last_rebase":{"status":"completed","dropped_paths":["a.txt"],"dropped_paths_verified":true}}`,
			[]string{"swarm --fork pr work 7", "swarm --fork pr rebase 7 --abort"}, nil},
		{"verified empty prints nothing", `{"last_rebase":{"status":"completed","dropped_paths_verified":true}}`,
			nil, []string{"no longer changed", "Changed-path comparison", "--abort"}},
		{"legacy evidence is unavailable not clean", `{"last_rebase":{"status":"completed","patch_identity":"changed","verified":true}}`,
			[]string{"Changed-path comparison: unavailable\n"}, []string{"no longer changed"}},
		{"in-progress shows no finding", `{"last_rebase":{"status":"in-progress","dropped_paths":["stale.txt"],"dropped_paths_verified":true}}`,
			nil, []string{"no longer changed", "Changed-path comparison"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var st lease.IssueState
			if err := json.Unmarshal([]byte(tc.state), &st); err != nil {
				t.Fatal(err)
			}
			got := RebaseSummary(&st, 7)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("summary lacks %q:\n%s", want, got)
				}
			}
			for _, unwanted := range tc.forbidden {
				if strings.Contains(got, unwanted) {
					t.Errorf("summary contains %q:\n%s", unwanted, got)
				}
			}
		})
	}
}
