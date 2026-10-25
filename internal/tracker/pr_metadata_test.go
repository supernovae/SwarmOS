package tracker

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubPRStateAndBaseMetadata(t *testing.T) {
	state := "open"
	draft := false
	merged := false
	targetMissing := false
	baseRef := "main"
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/git/ref/heads/") {
			if r.URL.EscapedPath() != "/repos/octo/swarm/git/ref/heads/"+baseRef {
				http.Error(w, "unexpected ref path "+r.URL.EscapedPath(), http.StatusNotFound)
				return
			}
			if targetMissing {
				http.Error(w, "target deleted", http.StatusNotFound)
			} else {
				writeJSON(t, w, map[string]any{"object": map[string]string{"sha": "cccccccccccccccccccccccccccccccccccccccc"}})
			}
			return
		}
		writeJSON(t, w, map[string]any{"number": 55, "state": state, "draft": draft, "merged": merged,
			"head": map[string]any{"ref": "feature", "sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "repo": map[string]any{"full_name": "octo/swarm"}},
			"base": map[string]any{"ref": baseRef, "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "repo": map[string]any{"full_name": "octo/swarm"}}})
	}))
	pr, err := tr.GetPullRequest(context.Background(), 55)
	if err != nil || pr.State != "open" || pr.Draft || pr.Merged || pr.BaseSHA != "cccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("open ready PR: %+v %v", pr, err)
	}
	baseRef = "release/1.0"
	if pr, err = tr.GetPullRequest(context.Background(), 55); err != nil || pr.BaseSHA != "cccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("PR targeting a nested branch: %+v %v", pr, err)
	}
	baseRef = "main"
	targetMissing = true
	if _, err := tr.GetPullRequest(context.Background(), 55); err == nil {
		t.Fatal("missing target branch fell back to the stale PR base snapshot")
	}
	state, draft, merged = "closed", true, true
	pr, err = tr.GetPullRequest(context.Background(), 55)
	if err != nil || pr.State != "closed" || pr.Draft || !pr.Merged {
		t.Fatalf("closed merged PR: %+v %v", pr, err)
	}
}

func TestGitHubPRReviewsPaginationAndMetadataOnly(t *testing.T) {
	stamp := "2026-10-23T11:12:13Z"
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/swarm/pulls/55/reviews" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected request %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		switch r.URL.Query().Get("page") {
		case "1":
			first := make([]map[string]any, 100)
			for i := range first {
				first[i] = map[string]any{"user": map[string]any{"login": "reviewer"}, "state": "COMMENTED", "commit_id": "a", "submitted_at": stamp, "body": "untrusted instruction"}
			}
			writeJSON(t, w, first)
		case "2":
			writeJSON(t, w, []map[string]any{{"user": map[string]any{"login": "maintainer"}, "state": "APPROVED", "commit_id": "b", "submitted_at": stamp, "body": "untrusted instruction"}})
		default:
			t.Errorf("unexpected review page %s", r.URL.Query().Get("page"))
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	}))
	reviews, err := tr.GetPullRequestReviews(context.Background(), 55)
	if err != nil || len(reviews) != 101 {
		t.Fatalf("review pagination: %d %v", len(reviews), err)
	}
	if reviews[100].Author != "maintainer" || reviews[100].State != "APPROVED" || reviews[100].CommitSHA != "b" || !reviews[100].SubmittedAt.Equal(time.Date(2026, 10, 23, 11, 12, 13, 0, time.UTC)) {
		t.Fatalf("review metadata: %+v", reviews[100])
	}
}

func TestGitHubCheckRunsRecordReportingApp(t *testing.T) {
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/swarm/commits/bbbb/check-runs" {
			t.Errorf("unexpected request %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		writeJSON(t, w, map[string]any{"check_runs": []map[string]any{
			{"name": "Hosted Review", "status": "completed", "conclusion": "neutral", "app": map[string]any{"slug": "review-app"}},
			{"name": "legacy", "status": "completed", "conclusion": "success"},
		}})
	}))
	checks, err := tr.GetCheckStatus(context.Background(), "bbbb")
	if err != nil || len(checks) != 2 {
		t.Fatalf("checks: %+v %v", checks, err)
	}
	if checks[0].App != "review-app" || checks[0].Conclusion != "neutral" || checks[1].App != "" || checks[1].Name != "legacy" {
		t.Fatalf("check metadata: %+v", checks)
	}
}

func TestLocalPRRefreshesExplicitBranchRefsWithoutInventingChecks(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return string(out)
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.test")
	if err := os.WriteFile(filepath.Join(repo, "file"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "file")
	git("commit", "-m", "base")
	tr := NewLocalTracker(repo)
	pr, err := tr.CreateDraftPR(context.Background(), DraftPRRequest{IssueNumber: 9, Head: "feature", Base: "main"})
	if err != nil || pr.State != "open" || pr.IssueNumber != 9 || pr.BaseSHA == "" || pr.HeadSHA != "" {
		t.Fatalf("initial PR: %+v %v", pr, err)
	}
	git("branch", "feature")
	pr, err = tr.GetPullRequest(context.Background(), pr.Number)
	if err != nil || pr.HeadSHA != pr.BaseSHA {
		t.Fatalf("ref refresh: %+v %v", pr, err)
	}
	git("checkout", "feature")
	if err := os.WriteFile(filepath.Join(repo, "file"), []byte("change"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "file")
	git("commit", "-m", "change")
	pr, err = tr.GetPullRequest(context.Background(), pr.Number)
	if err != nil || pr.HeadSHA == pr.BaseSHA || pr.HeadSHA == "" {
		t.Fatalf("advanced head: %+v %v", pr, err)
	}
	active, err := tr.CreateDraftPR(context.Background(), DraftPRRequest{IssueNumber: 10, Head: "feature", Base: "main"})
	if err != nil || active.HeadSHA != pr.HeadSHA {
		t.Fatalf("initialized PR ref: %+v %v", active, err)
	}
	oldHead := active.HeadSHA
	git("checkout", "-b", "pr-work")
	if err := os.WriteFile(filepath.Join(repo, "file"), []byte("later"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "file")
	git("commit", "-m", "later")
	newHead := strings.TrimSpace(git("rev-parse", "HEAD"))
	if err := tr.PublishPRHead(context.Background(), active.Number, oldHead, newHead); err != nil {
		t.Fatal(err)
	}
	if err := tr.PublishPRHead(context.Background(), active.Number, oldHead, pr.BaseSHA); err == nil {
		t.Fatal("stale local PR publish accepted")
	}
	active, err = tr.GetPullRequest(context.Background(), active.Number)
	if err != nil || active.HeadSHA != newHead || active.Head != "feature" {
		t.Fatalf("published PR: %+v %v", active, err)
	}
	if got := strings.TrimSpace(git("rev-parse", "refs/heads/feature")); got != oldHead {
		t.Fatalf("local issue branch was moved by PR publication: %s", got)
	}
	checks, err := tr.GetCheckStatus(context.Background(), pr.HeadSHA)
	if err != nil || len(checks) != 0 {
		t.Fatalf("invented missing checks: %+v %v", checks, err)
	}
}

func TestGitHubMergedPRMetadata(t *testing.T) {
	stamp := "2026-10-23T11:12:13Z"
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"number": 25, "state": "closed", "merged": true, "merge_commit_sha": "merge", "merged_at": stamp, "head": map[string]any{"ref": "task", "sha": "submitted"}, "base": map[string]any{"ref": "main", "sha": "base"}})
	}))
	pr, err := tr.GetPullRequest(context.Background(), 25)
	if err != nil || pr.MergeCommitSHA != "merge" || pr.MergedAt == nil || pr.MergedAt.Format(time.RFC3339) != stamp || pr.HeadSHA != "submitted" {
		t.Fatalf("merge evidence: %+v %v", pr, err)
	}
}

func TestGitHubPRMergeabilityMetadata(t *testing.T) {
	for _, raw := range []string{`"mergeable":false,"mergeable_state":"dirty",`, `"mergeable":true,"mergeable_state":"clean",`, `"mergeable":null,"mergeable_state":"unknown",`, ``} {
		t.Run(raw, func(t *testing.T) {
			tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "git/ref/") {
					writeJSON(t, w, map[string]any{"object": map[string]string{"sha": "fresh"}})
					return
				}
				_, _ = w.Write([]byte(`{` + raw + `"number":55,"state":"open","base":{"ref":"main","sha":"stale"}}`))
			}))
			p, err := tr.GetPullRequest(context.Background(), 55)
			if err != nil {
				t.Fatal(err)
			}
			if p.BaseSHA != "fresh" {
				t.Fatal("stale target")
			}
			if strings.Contains(raw, "false") && (p.Mergeable == nil || *p.Mergeable || p.MergeableState != "dirty") {
				t.Fatalf("%+v", p)
			}
			if strings.Contains(raw, "true") && (p.Mergeable == nil || !*p.Mergeable || p.MergeableState != "clean") {
				t.Fatalf("%+v", p)
			}
			if (raw == "" || strings.Contains(raw, "null")) && p.Mergeable != nil {
				t.Fatalf("%+v", p)
			}
		})
	}
}
