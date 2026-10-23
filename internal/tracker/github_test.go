package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newHTTPGitHubTracker(t *testing.T, handler http.Handler) *GitHubTracker {
	t.Helper()
	client, err := api.NewRESTClient(api.ClientOptions{
		Host:      "github.com",
		AuthToken: "test-token",
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "api.github.com" {
				return nil, fmt.Errorf("unexpected outbound host %q", req.URL.Host)
			}
			cloned := req.Clone(req.Context())
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, cloned)
			resp := rec.Result()
			resp.Request = cloned
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &GitHubTracker{owner: "octo", repo: "swarm", client: client}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubTrackerPaginatesIssuesAndComments(t *testing.T) {
	issues := make([]map[string]any, 100)
	for i := range issues {
		issues[i] = map[string]any{"number": i + 1, "title": "newer", "user": map[string]string{"login": "author"}}
	}
	issues = append(issues, map[string]any{"number": 101, "title": "ready work", "labels": []map[string]string{{"name": "swarm:ready"}}, "user": map[string]string{"login": "author"}})
	comments := make([]map[string]any, 100)
	for i := range comments {
		comments[i] = map[string]any{"id": i + 1, "body": "newer", "user": map[string]string{"login": "author"}}
	}
	comments = append(comments, map[string]any{"id": 101, "body": "VERDICT: APPROVED", "user": map[string]string{"login": "reviewer"}})

	tracker := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/swarm/issues":
			if r.URL.Query().Get("page") == "1" {
				writeJSON(t, w, issues[:100])
				return
			}
			if r.URL.Query().Get("page") == "2" {
				writeJSON(t, w, issues[100:])
				return
			}
		case "/repos/octo/swarm/issues/101/comments":
			if r.URL.Query().Get("page") == "1" {
				writeJSON(t, w, comments[:100])
				return
			}
			if r.URL.Query().Get("page") == "2" {
				writeJSON(t, w, comments[100:])
				return
			}
		}
		http.Error(w, "unexpected request", http.StatusNotFound)
	}))

	listedIssues, err := tracker.ListIssues(context.Background(), IssueFilter{Labels: []string{"swarm:ready"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(listedIssues) != 101 || listedIssues[100].Number != 101 || listedIssues[100].Labels[0] != "swarm:ready" {
		t.Fatalf("older readiness issue was not returned: %#v", listedIssues[len(listedIssues)-1])
	}
	listedComments, err := tracker.ListComments(context.Background(), 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(listedComments) != 101 || listedComments[100].Body != "VERDICT: APPROVED" {
		t.Fatalf("older review comment was not returned: %#v", listedComments[len(listedComments)-1])
	}
}

func TestGitHubTrackerCurrentUserUsesGitHubIdentity(t *testing.T) {
	t.Setenv("SWARM_USER", "attacker")
	tracker := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/user" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		writeJSON(t, w, map[string]string{"login": "trusted-user"})
	}))

	user, err := tracker.CurrentUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if user != "trusted-user" {
		t.Fatalf("CurrentUser() = %q, want GitHub identity", user)
	}
	if got := os.Getenv("SWARM_USER"); got != "attacker" {
		t.Fatalf("test setup lost untrusted environment value %q", got)
	}
}

func TestGitHubTrackerAuthorizePermissionsAndHumanReview(t *testing.T) {
	for _, tc := range []struct {
		name          string
		accountType   string
		permission    string
		reqPermission string
		wantErr       bool
	}{
		{name: "write approves task", permission: "write", reqPermission: "approve-task"},
		{name: "maintain approves task", permission: "maintain", reqPermission: "approve-task"},
		{name: "admin approves task", permission: "admin", reqPermission: "approve-task"},
		{name: "read cannot approve task", permission: "read", reqPermission: "approve-task", wantErr: true},
		{name: "bot cannot review as human", accountType: "Bot", permission: "admin", reqPermission: "review-human", wantErr: true},
		{name: "write cannot create epic", permission: "write", reqPermission: "create-epic", wantErr: true},
		{name: "maintain can create epic", permission: "maintain", reqPermission: "create-epic"},
		{name: "admin can create epic", permission: "admin", reqPermission: "create-epic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/user":
					writeJSON(t, w, map[string]string{"login": "maintainer", "type": tc.accountType})
				case "/repos/octo/swarm/collaborators/maintainer/permission":
					writeJSON(t, w, map[string]string{"permission": tc.permission})
				default:
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			err := tracker.Authorize(context.Background(), tc.reqPermission)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Authorize(%q) error = %v, wantErr %t", tc.reqPermission, err, tc.wantErr)
			}
		})
	}
}

func TestGitHubTrackerContextCancellationAndResponseErrors(t *testing.T) {
	requests := 0
	tracker := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "upstream failure", http.StatusInternalServerError)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tracker.ListIssues(ctx, IssueFilter{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ListIssues() error = %v, want context cancellation", err)
	}
	if requests != 0 {
		t.Fatalf("canceled request reached GitHub: %d requests", requests)
	}
	_, err = tracker.GetIssue(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("GetIssue() error = %v, want propagated HTTP failure", err)
	}
}

func TestGitHubTrackerFindDraftPRUsesRequestedHeadAndBase(t *testing.T) {
	tracker := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/octo/swarm/pulls" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		if q.Get("head") != "octo:feature/ref" || q.Get("base") != "release" || q.Get("state") != "open" {
			http.Error(w, "wrong draft PR lookup", http.StatusBadRequest)
			return
		}
		writeJSON(t, w, []map[string]any{{"number": 12, "title": "feature", "draft": true, "html_url": "https://example.test/pr/12"}})
	}))

	pr, err := tracker.FindDraftPR(context.Background(), "feature/ref", "release")
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.Number != 12 || pr.Head != "feature/ref" || pr.Base != "release" || !pr.Draft {
		t.Fatalf("FindDraftPR() = %#v", pr)
	}
}

func TestGitHubTrackerEpicIssueTypeAndNativeSubIssues(t *testing.T) {
	ctx := context.Background()
	enabled := true
	disabled := false

	t.Run("create typed epic and link sub-issue using child database id", func(t *testing.T) {
		var createdPayload map[string]any
		var linkedPayload map[string]any
		parentLinked := 0

		tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/user":
				writeJSON(t, w, map[string]string{"login": "maintainer", "type": "User"})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/collaborators/maintainer/permission":
				writeJSON(t, w, map[string]string{"permission": "maintain"})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/issue-types":
				writeJSON(t, w, []map[string]any{
					{"id": 10, "name": "Bug", "is_enabled": enabled},
					{"id": 20, "name": "Epic", "is_enabled": enabled},
				})
			case r.Method == http.MethodPost && r.URL.Path == "/repos/octo/swarm/issues":
				if err := json.NewDecoder(r.Body).Decode(&createdPayload); err != nil {
					t.Fatal(err)
				}
				writeJSON(t, w, map[string]any{
					"id":       5001,
					"number":   1,
					"title":    createdPayload["title"],
					"body":     createdPayload["body"],
					"state":    "open",
					"html_url": "https://example.test/issues/1",
					"type":     map[string]any{"name": "Epic"},
				})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/issues/1":
				writeJSON(t, w, map[string]any{
					"id":     5001,
					"number": 1,
					"title":  "Roadmap Epic",
					"state":  "open",
					"type":   map[string]any{"name": "Epic"},
				})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/issues/2":
				writeJSON(t, w, map[string]any{
					"id":     987654321,
					"number": 2,
					"title":  "Child task",
					"state":  "open",
				})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/issues/2/parent":
				if parentLinked == 0 {
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
					return
				}
				writeJSON(t, w, map[string]any{"id": 5001, "number": parentLinked, "title": "Parent"})
			case r.Method == http.MethodPost && r.URL.Path == "/repos/octo/swarm/issues/1/sub_issues":
				if err := json.NewDecoder(r.Body).Decode(&linkedPayload); err != nil {
					t.Fatal(err)
				}
				parentLinked = 1
				writeJSON(t, w, map[string]any{"id": 987654321, "number": 2})
			default:
				http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			}
		}))

		epic, err := tr.CreateTypedIssue(ctx, "Roadmap Epic", "# Plan", nil, "Epic")
		if err != nil {
			t.Fatalf("CreateTypedIssue() error = %v", err)
		}
		if !epic.IsEpic() || epic.Type != "Epic" {
			t.Fatalf("epic.Type = %q, want Epic", epic.Type)
		}
		if got := createdPayload["type"]; got != "Epic" {
			t.Fatalf("POST /issues payload type = %v, want Epic", got)
		}

		parent, err := tr.ParentIssueNumber(ctx, 2)
		if err != nil || parent != 0 {
			t.Fatalf("ParentIssueNumber(unlinked) = (%d, %v), want (0, nil)", parent, err)
		}

		if err := tr.LinkSubIssue(ctx, 1, 2); err != nil {
			t.Fatalf("LinkSubIssue() error = %v", err)
		}
		if got := linkedPayload["sub_issue_id"]; got != float64(987654321) {
			t.Fatalf("POST /sub_issues sub_issue_id = %v, want child database id 987654321", got)
		}
		// Idempotent repeat link to same parent
		if err := tr.LinkSubIssue(ctx, 1, 2); err != nil {
			t.Fatalf("idempotent LinkSubIssue() error = %v", err)
		}
		parent, err = tr.ParentIssueNumber(ctx, 2)
		if err != nil || parent != 1 {
			t.Fatalf("ParentIssueNumber(linked) = (%d, %v), want (1, nil)", parent, err)
		}
	})

	t.Run("rejects disabled epic type, non-maintainer create, self link, and foreign parent", func(t *testing.T) {
		posts := 0
		tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/user":
				writeJSON(t, w, map[string]string{"login": "contributor", "type": "User"})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/collaborators/contributor/permission":
				writeJSON(t, w, map[string]string{"permission": "write"})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/issue-types":
				writeJSON(t, w, []map[string]any{
					{"id": 20, "name": "Epic", "is_enabled": disabled},
				})
			case r.Method == http.MethodPost:
				posts++
				http.Error(w, "should not write", http.StatusInternalServerError)
			case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/issues/10":
				writeJSON(t, w, map[string]any{"id": 1000, "number": 10, "state": "open"})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/issues/20":
				writeJSON(t, w, map[string]any{"id": 2000, "number": 20, "state": "open"})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/issues/20/parent":
				writeJSON(t, w, map[string]any{"id": 999, "number": 99})
			default:
				http.Error(w, "unexpected", http.StatusNotFound)
			}
		}))

		if _, err := tr.CreateTypedIssue(ctx, "Unauthorized Epic", "body", nil, "Epic"); err == nil {
			t.Fatal("expected write collaborator to be denied create-epic")
		}
		if err := tr.CheckIssueType(ctx, "Epic"); err == nil {
			t.Fatal("expected disabled Epic type to fail CheckIssueType")
		}
		if err := tr.CheckIssueType(ctx, "Task"); err == nil {
			t.Fatal("expected unsupported type to fail CheckIssueType")
		}
		if err := tr.LinkSubIssue(ctx, 10, 10); err == nil {
			t.Fatal("expected self-link to fail")
		}
		if err := tr.LinkSubIssue(ctx, 10, 20); err == nil || !strings.Contains(err.Error(), "#99") {
			t.Fatalf("expected foreign parent #99 conflict, got %v", err)
		}
		if posts != 0 {
			t.Fatalf("expected 0 writes on preflight failures, got %d", posts)
		}
	})
}

func TestLocalTrackerEpicAndHierarchyPersistence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tr := NewLocalTracker(root)

	collab := tr.WithUser("contrib", "COLLABORATOR")
	if err := collab.Authorize(ctx, "approve-task"); err != nil {
		t.Fatalf("COLLABORATOR should authorize approve-task: %v", err)
	}
	if err := collab.Authorize(ctx, "create-epic"); err == nil {
		t.Fatal("COLLABORATOR must not authorize create-epic")
	}
	if _, err := collab.CreateTypedIssue(ctx, "Blocked Epic", "body", nil, "Epic"); err == nil {
		t.Fatal("COLLABORATOR CreateTypedIssue(Epic) must fail before writing")
	}
	if issues, err := tr.ListIssues(ctx, IssueFilter{State: "all"}); err != nil || len(issues) != 0 {
		t.Fatalf("expected 0 issues after unauthorized CreateTypedIssue, got %d (%v)", len(issues), err)
	}

	if err := tr.CheckIssueType(ctx, "Unknown"); err == nil {
		t.Fatal("expected unknown issue type to fail")
	}

	epic, err := tr.CreateTypedIssue(ctx, "Roadmap Epic", "# Plan", nil, "epic")
	if err != nil {
		t.Fatalf("OWNER CreateTypedIssue(Epic) failed: %v", err)
	}
	if epic.Type != "Epic" || !epic.IsEpic() {
		t.Fatalf("epic.Type = %q, want Epic", epic.Type)
	}

	child, err := tr.CreateIssue(ctx, "Child 1", "body", []string{"swarm:draft"})
	if err != nil {
		t.Fatal(err)
	}
	otherEpic, err := tr.CreateTypedIssue(ctx, "Other Epic", "# Other", nil, "Epic")
	if err != nil {
		t.Fatal(err)
	}

	if p, err := tr.ParentIssueNumber(ctx, child.Number); err != nil || p != 0 {
		t.Fatalf("unlinked child parent = (%d, %v), want (0, nil)", p, err)
	}
	if err := tr.LinkSubIssue(ctx, epic.Number, child.Number); err != nil {
		t.Fatalf("LinkSubIssue failed: %v", err)
	}
	// Idempotent link
	if err := tr.LinkSubIssue(ctx, epic.Number, child.Number); err != nil {
		t.Fatalf("idempotent LinkSubIssue failed: %v", err)
	}
	// Reject foreign parent
	if err := tr.LinkSubIssue(ctx, otherEpic.Number, child.Number); err == nil {
		t.Fatal("expected linking child to second parent to fail")
	}
	// Reject self link and missing issue
	if err := tr.LinkSubIssue(ctx, child.Number, child.Number); err == nil {
		t.Fatal("expected self link to fail")
	}
	if err := tr.LinkSubIssue(ctx, epic.Number, 999); err == nil {
		t.Fatal("expected missing child link to fail")
	}

	reloaded := NewLocalTracker(root)
	gotEpic, err := reloaded.GetIssue(ctx, epic.Number)
	if err != nil || !gotEpic.IsEpic() {
		t.Fatalf("reloaded epic = %#v, err = %v", gotEpic, err)
	}
	gotParent, err := reloaded.ParentIssueNumber(ctx, child.Number)
	if err != nil || gotParent != epic.Number {
		t.Fatalf("reloaded ParentIssueNumber = (%d, %v), want (%d, nil)", gotParent, err, epic.Number)
	}
}

func TestForkGitHubTrackerVerifyForkAndGetPullRequest(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name          string
		forkSlug      string
		expectedOwner string
		requirePush   bool
		repoResp      map[string]any
		wantErr       bool
	}{
		{
			name:          "valid writable personal fork",
			forkSlug:      "alice/swarm",
			expectedOwner: "alice",
			requirePush:   true,
			repoResp: map[string]any{
				"full_name":   "alice/swarm",
				"fork":        true,
				"owner":       map[string]any{"login": "alice"},
				"source":      map[string]any{"full_name": "octo/swarm"},
				"permissions": map[string]any{"push": true},
			},
		},
		{
			name:          "valid fork verified by coordinator without push permission",
			forkSlug:      "alice/swarm",
			expectedOwner: "alice",
			requirePush:   false,
			repoResp: map[string]any{
				"full_name":   "alice/swarm",
				"fork":        true,
				"owner":       map[string]any{"login": "alice"},
				"source":      map[string]any{"full_name": "octo/swarm"},
				"permissions": map[string]any{"push": false},
			},
		},
		{
			name:          "rejects non-fork repository",
			forkSlug:      "alice/swarm",
			expectedOwner: "alice",
			requirePush:   true,
			repoResp: map[string]any{
				"full_name":   "alice/swarm",
				"fork":        false,
				"owner":       map[string]any{"login": "alice"},
				"permissions": map[string]any{"push": true},
			},
			wantErr: true,
		},
		{
			name:          "rejects fork of unrelated upstream",
			forkSlug:      "alice/swarm",
			expectedOwner: "alice",
			requirePush:   true,
			repoResp: map[string]any{
				"full_name":   "alice/swarm",
				"fork":        true,
				"owner":       map[string]any{"login": "alice"},
				"source":      map[string]any{"full_name": "other/repo"},
				"permissions": map[string]any{"push": true},
			},
			wantErr: true,
		},
		{
			name:          "rejects fork owned by different user",
			forkSlug:      "bob/swarm",
			expectedOwner: "alice",
			requirePush:   true,
			repoResp: map[string]any{
				"full_name":   "bob/swarm",
				"fork":        true,
				"owner":       map[string]any{"login": "bob"},
				"source":      map[string]any{"full_name": "octo/swarm"},
				"permissions": map[string]any{"push": true},
			},
			wantErr: true,
		},
		{
			name:          "rejects non-writable fork on client",
			forkSlug:      "alice/swarm",
			expectedOwner: "alice",
			requirePush:   true,
			repoResp: map[string]any{
				"full_name":   "alice/swarm",
				"fork":        true,
				"owner":       map[string]any{"login": "alice"},
				"source":      map[string]any{"full_name": "octo/swarm"},
				"permissions": map[string]any{"push": false},
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/") {
					writeJSON(t, w, tc.repoResp)
					return
				}
				http.Error(w, "unexpected", http.StatusNotFound)
			}))
			got, err := tr.VerifyForkRepository(ctx, tc.forkSlug, tc.expectedOwner, tc.requirePush)
			if (err != nil) != tc.wantErr {
				t.Fatalf("VerifyForkRepository() = (%q, %v), wantErr %t", got, err, tc.wantErr)
			}
		})
	}

	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/pulls":
			if r.URL.Query().Get("head") != "alice:swarm/issue-42-claim1" {
				http.Error(w, "expected qualified head without double prefix, got "+r.URL.Query().Get("head"), http.StatusBadRequest)
				return
			}
			writeJSON(t, w, []map[string]any{
				{"number": 77, "title": "Fix #42", "draft": true, "html_url": "https://github.com/octo/swarm/pull/77"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/swarm/pulls/77":
			writeJSON(t, w, map[string]any{
				"number":   77,
				"title":    "Fix #42",
				"state":    "open",
				"draft":    true,
				"html_url": "https://github.com/octo/swarm/pull/77",
				"head": map[string]any{
					"ref":  "swarm/issue-42-claim1",
					"sha":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					"repo": map[string]any{"full_name": "alice/swarm"},
				},
				"base": map[string]any{
					"ref":  "main",
					"repo": map[string]any{"full_name": "octo/swarm"},
				},
			})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))

	found, err := tr.FindDraftPR(ctx, "alice:swarm/issue-42-claim1", "main")
	if err != nil || found == nil || found.Number != 77 {
		t.Fatalf("FindDraftPR(qualified) = (%+v, %v)", found, err)
	}
	pr, err := tr.GetPullRequest(ctx, 77)
	if err != nil || pr == nil {
		t.Fatalf("GetPullRequest(77) = (%+v, %v)", pr, err)
	}
	if pr.HeadRepo != "alice/swarm" || pr.BaseRepo != "octo/swarm" || pr.Head != "swarm/issue-42-claim1" || pr.HeadSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || !pr.Draft {
		t.Fatalf("unexpected GetPullRequest fields: %+v", pr)
	}
}
