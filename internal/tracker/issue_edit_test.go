package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUpdateIssueTitleLocal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tr := NewLocalTracker(root)
	issue, err := tr.CreateIssue(ctx, "old", "untouched body", []string{"custom"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.UpdateIssueTitle(ctx, issue.Number, "new title"); err != nil {
		t.Fatal(err)
	}
	got, err := NewLocalTracker(root).GetIssue(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "new title" || got.Body != issue.Body || !reflect.DeepEqual(got.Labels, issue.Labels) {
		t.Fatal(got)
	}
	if _, err := tr.UpdateIssueTitle(ctx, 999, "missing"); err == nil {
		t.Fatal("missing issue accepted")
	}
	statePath := filepath.Join(root, ".swarm", "local-tracker", "state.json")
	target := filepath.Join(root, "outside.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.UpdateIssueTitle(ctx, issue.Number, "symlink"); err == nil {
		t.Fatal("followed state symlink")
	}
	after, _ := os.ReadFile(target)
	if string(after) != string(before) {
		t.Fatal("symlink target modified")
	}
}
func TestUpdateIssueTitleGitHub(t *testing.T) {
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" || r.URL.Path != "/repos/octo/swarm/issues/51" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(payload, map[string]string{"title": "new title"}) {
			t.Error(payload)
		}
		writeJSON(t, w, map[string]any{"number": 51, "title": payload["title"], "body": "unchanged"})
	}))
	got, err := tr.UpdateIssueTitle(context.Background(), 51, "new title")
	if err != nil || got.Title != "new title" || got.Body != "unchanged" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestIssueEditAuthorization(t *testing.T) {
	for _, perm := range []string{"admin", "maintain", "write", "read"} {
		t.Run(perm, func(t *testing.T) {
			tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/user" {
					writeJSON(t, w, map[string]string{"login": "maintainer", "type": "User"})
					return
				}
				writeJSON(t, w, map[string]string{"permission": perm})
			}))
			if err := tr.Authorize(context.Background(), "edit-task"); (err != nil) != (perm == "read") {
				t.Fatal(err)
			}
		})
	}
	for _, assoc := range []string{"OWNER", "MEMBER", "COLLABORATOR", "NONE"} {
		tr := NewLocalTracker(t.TempDir()).WithUser("actor", assoc)
		if err := tr.Authorize(context.Background(), "edit-task"); (err != nil) != (assoc == "NONE") {
			t.Fatal(assoc, err)
		}
	}
}
