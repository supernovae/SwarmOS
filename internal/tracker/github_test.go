package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.NewRESTClient(api.ClientOptions{
		Host:      "github.com",
		AuthToken: "test-token",
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "api.github.com" {
				return nil, fmt.Errorf("unexpected outbound host %q", req.URL.Host)
			}
			cloned := req.Clone(req.Context())
			cloned.URL.Scheme = serverURL.Scheme
			cloned.URL.Host = serverURL.Host
			cloned.Host = serverURL.Host
			return http.DefaultTransport.RoundTrip(cloned)
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
		name        string
		accountType string
		permission  string
		wantErr     bool
	}{
		{name: "write approves task", permission: "write"},
		{name: "maintain approves task", permission: "maintain"},
		{name: "admin approves task", permission: "admin"},
		{name: "read cannot approve task", permission: "read", wantErr: true},
		{name: "bot cannot review as human", accountType: "Bot", permission: "admin", wantErr: true},
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
			permission := "approve-task"
			if tc.accountType != "" {
				permission = "review-human"
			}
			err := tracker.Authorize(context.Background(), permission)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Authorize() error = %v, wantErr %t", err, tc.wantErr)
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
