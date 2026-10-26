package tracker

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestGitHubListPullRequestsPagesAndMapsMerged(t *testing.T) {
	var pages []string
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != "closed" || q.Get("sort") != "updated" || q.Get("direction") != "desc" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		pages = append(pages, q.Get("page"))
		var rows []map[string]any
		start := 1
		count := 100
		if q.Get("page") == "2" {
			start, count = 101, 3
		}
		for i := 0; i < count; i++ {
			row := map[string]any{"number": start + i, "state": "closed", "title": fmt.Sprint("PR ", start+i), "draft": true,
				"updated_at": "2026-01-02T00:00:00Z", "user": map[string]any{"login": "octo"}}
			if (start+i)%2 == 0 {
				row["merged_at"] = "2026-01-02T00:00:00Z"
			}
			rows = append(rows, row)
		}
		writeJSON(t, w, rows)
	}))
	prs, err := tr.ListPullRequests(context.Background(), PullRequestFilter{State: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 103 || len(pages) != 2 {
		t.Fatalf("got %d PRs over pages %v", len(prs), pages)
	}
	if prs[0].Merged || !prs[1].Merged || prs[0].Draft || prs[0].Author != "octo" || prs[0].UpdatedAt.IsZero() {
		t.Fatalf("unexpected mapping: %+v %+v", prs[0], prs[1])
	}
	pages = nil
	if prs, err = tr.ListPullRequests(context.Background(), PullRequestFilter{State: "closed", Limit: 5}); err != nil || len(prs) != 5 || len(pages) != 1 {
		t.Fatalf("limit: %d PRs over %v pages (%v)", len(prs), pages, err)
	}
}

// Bulk cleanup previews read merge and repository identity from one "all"
// listing, so every page must keep those fields.
func TestGitHubListPullRequestsAllKeepsCleanupEvidence(t *testing.T) {
	requests := 0
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		q := r.URL.Query()
		if q.Get("state") != "all" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		start, count := 1, 100
		if q.Get("page") == "2" {
			start, count = 101, 1
		}
		var rows []map[string]any
		for i := start; i < start+count; i++ {
			row := map[string]any{"number": i, "state": "open", "html_url": fmt.Sprint("https://github.com/octo/swarm/pull/", i),
				"head": map[string]any{"ref": fmt.Sprint("swarm/issue-", i, "-x"), "sha": fmt.Sprintf("%040d", i), "repo": map[string]any{"full_name": "octo/swarm"}},
				"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "octo/swarm"}}}
			if i == 101 {
				row["state"] = "closed"
				row["merged_at"] = "2026-01-02T00:00:00Z"
				row["merge_commit_sha"] = "abc"
			}
			rows = append(rows, row)
		}
		writeJSON(t, w, rows)
	}))
	prs, err := tr.ListPullRequests(context.Background(), PullRequestFilter{State: "all"})
	if err != nil || len(prs) != 101 || requests != 2 {
		t.Fatalf("got %d PRs over %d requests (%v)", len(prs), requests, err)
	}
	last := prs[100]
	if last.Number != 101 || !last.Merged || last.MergedAt == nil || last.MergeCommitSHA != "abc" || last.HeadSHA != fmt.Sprintf("%040d", 101) ||
		last.HeadRepo != "octo/swarm" || last.BaseRepo != "octo/swarm" || last.Head != "swarm/issue-101-x" || last.HTMLURL == "" {
		t.Fatalf("lost cleanup evidence: %+v", last)
	}
	if prs[0].State != "open" || prs[0].Merged || prs[0].HeadRepo != "octo/swarm" {
		t.Fatalf("open PR identity: %+v", prs[0])
	}
}

func TestLocalListPullRequestsFiltersByState(t *testing.T) {
	tr := NewLocalTracker(t.TempDir())
	ctx := context.Background()
	for _, head := range []string{"a", "b"} {
		if _, err := tr.CreateDraftPR(ctx, DraftPRRequest{Head: head, Base: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	open, err := tr.ListPullRequests(ctx, PullRequestFilter{})
	if err != nil || len(open) != 2 || open[0].CreatedAt.IsZero() {
		t.Fatalf("open PRs: %+v %v", open, err)
	}
	if closed, err := tr.ListPullRequests(ctx, PullRequestFilter{State: "closed"}); err != nil || len(closed) != 0 {
		t.Fatalf("closed PRs: %+v %v", closed, err)
	}
	if limited, err := tr.ListPullRequests(ctx, PullRequestFilter{State: "all", Limit: 1}); err != nil || len(limited) != 1 {
		t.Fatalf("limited PRs: %+v %v", limited, err)
	}
}
