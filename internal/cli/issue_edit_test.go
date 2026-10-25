package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/coordination"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

type editRecordingTracker struct {
	tracker.Tracker
	reads, writes int
	onRead        func(int, *tracker.Issue) error
	fail          string
}

func (r *editRecordingTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	r.reads++
	i, err := r.Tracker.GetIssue(ctx, n)
	if err == nil && r.onRead != nil {
		err = r.onRead(r.reads, i)
	}
	return i, err
}
func (r *editRecordingTracker) UpdateIssueBody(ctx context.Context, n int, b string) (*tracker.Issue, error) {
	r.writes++
	if r.fail == "body" {
		return nil, errors.New("body failure")
	}
	return r.Tracker.UpdateIssueBody(ctx, n, b)
}
func (r *editRecordingTracker) UpdateIssueTitle(ctx context.Context, n int, b string) (*tracker.Issue, error) {
	r.writes++
	if r.fail == "title" {
		return nil, errors.New("title failure")
	}
	return r.Tracker.UpdateIssueTitle(ctx, n, b)
}
func (r *editRecordingTracker) RemoveLabel(ctx context.Context, n int, l string) error {
	r.writes++
	if r.fail == "remove" {
		return errors.New("remove failure")
	}
	return r.Tracker.RemoveLabel(ctx, n, l)
}
func (r *editRecordingTracker) AddLabels(ctx context.Context, n int, l []string) error {
	r.writes++
	if r.fail == "labels" {
		return errors.New("labels failure")
	}
	return r.Tracker.AddLabels(ctx, n, l)
}
func (r *editRecordingTracker) CreateComment(ctx context.Context, n int, b string) (*tracker.Comment, error) {
	r.writes++
	return r.Tracker.CreateComment(ctx, n, b)
}

func newEditFixture(t *testing.T) (*cliFixture, *tracker.Issue, *editRecordingTracker) {
	t.Helper()
	f := newCLIFixture(t)
	body := contract.FormatIssueMarkdown(&contract.WorkUnit{Objective: "old objective", Constraints: []string{"keep"}, Acceptance: []string{"works"}, Workflow: "feature"})
	i, err := f.tracker.CreateIssue(context.Background(), "title", body, []string{"custom"})
	if err != nil {
		t.Fatal(err)
	}
	return f, i, &editRecordingTracker{Tracker: f.tracker}
}
func runEdit(f *cliFixture, r tracker.Tracker, now time.Time, args ...string) (string, error) {
	opts := &RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: r, nowFn: func() time.Time { return now }}
	cmd := newIssueEditCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}
func TestIssueEditFlags(t *testing.T) {
	cmd := newIssueEditCmd(&RootOptions{})
	for _, name := range []string{"title", "objective", "constraint", "acceptance", "depends-on", "gate", "workflow", "dry-run", "json"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatal(name)
		}
	}
	if cmd.Flags().Lookup("ready") != nil {
		t.Fatal("edit may not approve")
	}
	for _, args := range [][]string{nil, {"1", "2"}, {"bad"}} {
		cmd := newIssueEditCmd(&RootOptions{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatal(args)
		}
	}
}
func TestIssueEditReplacement(t *testing.T) {
	f, i, r := newEditFixture(t)
	out, err := runEdit(f, r, time.Now(), fmt.Sprint(i.Number), "--constraint", "one", "--constraint", "two", "--depends-on=")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got, _ := f.tracker.GetIssue(context.Background(), i.Number)
	wu, err := contract.ParseWorkUnitFromIssue(i.Number, got.Title, got.Body)
	if err != nil || !reflect.DeepEqual(wu.Constraints, []string{"one", "two"}) || wu.Workflow != "feature" || len(wu.DependsOn) != 0 {
		t.Fatalf("%+v %v", wu, err)
	}
}
func TestIssueEditDryRunAndLint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		invalid bool
	}{
		{"changed", []string{"--objective", "new objective", "--dry-run"}, false},
		{"json", []string{"--title", "new title", "--dry-run", "--json"}, false},
		{"invalid", []string{"--objective", "", "--dry-run"}, true},
		{"invalid json", []string{"--gate", "bad", "--dry-run", "--json"}, true},
		{"invalid normal", []string{"--acceptance", ""}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, i, r := newEditFixture(t)
			out, err := runEdit(f, r, time.Now(), append([]string{fmt.Sprint(i.Number)}, tc.args...)...)
			if (err != nil) != tc.invalid {
				t.Fatalf("%v %s", err, out)
			}
			if r.writes != 0 {
				t.Fatalf("%d writes", r.writes)
			}
			got, _ := f.tracker.GetIssue(context.Background(), i.Number)
			if !reflect.DeepEqual(i, got) {
				t.Fatal("tracker changed")
			}
			if strings.Contains(tc.name, "json") {
				var v issueEditPreview
				if e := json.Unmarshal([]byte(out), &v); e != nil {
					t.Fatalf("%v: %s", e, out)
				}
			} else if !strings.Contains(out, "Lint:") {
				t.Fatal(out)
			}
		})
	}
}
func TestIssueEditApproval(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		changed bool
	}{
		{"changed", []string{"--objective", "new objective"}, true},
		{"title", []string{"--title", "new title"}, true},
		{"no-op", nil, false},
		{"same digest", []string{"--objective", "old objective"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, i, r := newEditFixture(t)
			ctx := context.Background()
			_, err := f.manager.ApproveTask(ctx, i.Number, "alice")
			if err != nil {
				t.Fatal(err)
			}
			approved, _ := f.manager.GetCurrentState(ctx, i.Number)
			out, err := runEdit(f, r, time.Now(), append([]string{fmt.Sprint(i.Number)}, tc.args...)...)
			if err != nil {
				t.Fatalf("%v %s", err, out)
			}
			state, _ := f.manager.GetCurrentState(ctx, i.Number)
			if !reflect.DeepEqual(approved, state) {
				t.Fatal("approval state mutated")
			}
			got, _ := f.tracker.GetIssue(ctx, i.Number)
			has := func(label string) bool {
				for _, l := range got.Labels {
					if l == label {
						return true
					}
				}
				return false
			}
			if !has("custom") || has("swarm:ready") == tc.changed || has("swarm:draft") != tc.changed {
				t.Fatal(got.Labels)
			}
			if tc.changed && !strings.Contains(out, fmt.Sprintf("swarm issue approve %d", i.Number)) {
				t.Fatal(out)
			}
		})
	}
}
func TestIssueEditClaim(t *testing.T) {
	for _, owner := range []string{"alice", "bob"} {
		t.Run(owner, func(t *testing.T) {
			f, i, r := newEditFixture(t)
			ctx := context.Background()
			if _, err := f.manager.ApproveTask(ctx, i.Number, "alice"); err != nil {
				t.Fatal(err)
			}
			if owner != "alice" {
				cfg, _ := harness.LoadConfig(f.root)
				f.manager = lease.NewManager(f.tracker.WithUser(owner, "OWNER"), cfg.Security)
				if err := f.manager.ConfigureStore(f.root); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			f.manager.SetNowFunc(func() time.Time { return now })
			st, err := f.manager.Acquire(ctx, i.Number, owner, "mock", "plan", time.Hour, 3)
			if err != nil {
				t.Fatal(err)
			}
			for _, dry := range []bool{false, true} {
				args := []string{fmt.Sprint(i.Number), "--objective", "new objective"}
				if dry {
					args = append(args, "--dry-run")
				}
				out, err := runEdit(f, r, now, args...)
				if err == nil || !strings.Contains(out+err.Error(), owner) || !strings.Contains(out+err.Error(), st.Lease.ExpiresAt.Format(time.RFC3339)) || !strings.Contains(out+err.Error(), "swarm issue release") {
					t.Fatalf("%v %s", err, out)
				}
			}
			if r.writes != 0 {
				t.Fatal("wrote under active claim")
			}
			if out, err := runEdit(f, r, st.Lease.ExpiresAt, fmt.Sprint(i.Number), "--objective", "new objective"); err != nil {
				t.Fatalf("expired: %v %s", err, out)
			}
		})
	}
}
func TestIssueEditConcurrent(t *testing.T) {
	for _, kind := range []string{"body", "title", "epic", "error", "claim"} {
		t.Run(kind, func(t *testing.T) {
			f, i, r := newEditFixture(t)
			ctx := context.Background()
			r.onRead = func(read int, got *tracker.Issue) error {
				if read == 2 {
					switch kind {
					case "body":
						got.Body += "web edit"
					case "title":
						got.Title = "web title"
					case "epic":
						got.Type = "Epic"
					case "error":
						return errors.New("read failed")

					}
				}
				return nil
			}
			if kind == "claim" {
				r.onRead = func(read int, got *tracker.Issue) error {
					if read == 1 {
						if _, err := f.manager.ApproveTask(ctx, i.Number, "alice"); err != nil {
							return err
						}
						_, err := f.manager.Acquire(ctx, i.Number, "alice", "mock", "plan", time.Hour, 3)
						return err
					}
					return nil
				}
			}
			_, err := runEdit(f, r, time.Now(), fmt.Sprint(i.Number), "--objective", "new objective")
			if err == nil || r.writes != 0 {
				t.Fatalf("err=%v writes=%d", err, r.writes)
			}
		})
	}
}
func TestIssueEditEpicAuthorizationFork(t *testing.T) {
	for _, kind := range []string{"epic", "unauthorized", "fork"} {
		t.Run(kind, func(t *testing.T) {
			f, i, r := newEditFixture(t)
			if kind == "epic" {
				r.onRead = func(_ int, i *tracker.Issue) error { i.Type = "Epic"; return nil }
			}
			if kind == "unauthorized" {
				r.Tracker = f.tracker.WithUser("outsider", "NONE")
			}
			opts := &RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: r, Fork: kind == "fork"}
			cmd := newIssueEditCmd(opts)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{fmt.Sprint(i.Number), "--objective", "new objective"})
			if err := cmd.Execute(); err == nil || r.writes != 0 {
				t.Fatalf("%v writes=%d", err, r.writes)
			}
		})
	}
}
func TestIssueEditApprovalPartialFailure(t *testing.T) {
	for _, step := range []string{"body", "title", "remove", "labels"} {
		t.Run(step, func(t *testing.T) {
			f, i, r := newEditFixture(t)
			ctx := context.Background()
			_, err := f.manager.ApproveTask(ctx, i.Number, "alice")
			if err != nil {
				t.Fatal(err)
			}
			approved, _ := f.manager.GetCurrentState(ctx, i.Number)
			r.fail = step
			_, err = runEdit(f, r, time.Now(), fmt.Sprint(i.Number), "--objective", "new objective", "--title", "new title")
			if err == nil || !strings.Contains(err.Error(), "failed to") || !strings.Contains(err.Error(), "inspect issue") {
				t.Fatal(err)
			}
			state, _ := f.manager.GetCurrentState(ctx, i.Number)
			if !reflect.DeepEqual(approved, state) {
				t.Fatal("approval changed")
			}
			got, _ := f.tracker.GetIssue(ctx, i.Number)
			if step != "body" && !strings.Contains(got.Body, "new") {
				t.Fatal("unsafe rollback")
			}
		})
	}
}
func TestIssueEditDiff(t *testing.T) {
	for _, tc := range []struct{ a, b string }{{"", "new\n"}, {"old\n", ""}, {"old", "new"}, {"same", "same\n"}, {strings.Repeat("old\n", 3000), strings.Repeat("new\n", 3000)}} {
		diff := issueEditDiff(tc.a, tc.b)
		if !strings.Contains(diff, "--- a/body\n+++ b/body\n@@ -") {
			t.Fatal(diff)
		}
		if (!strings.HasSuffix(tc.a, "\n") && tc.a != "" || !strings.HasSuffix(tc.b, "\n") && tc.b != "") && !strings.Contains(diff, "\\ No newline at end of file") {
			t.Fatal(diff)
		}
	}
	if issueEditDiff("same", "same") != "" {
		t.Fatal("unchanged diff")
	}
}

// A lease expires exactly at its recorded expiry, not one tick later.
func TestIssueEditClaimBoundary(t *testing.T) {
	now := time.Now()
	st := &lease.IssueState{Lease: contract.Lease{Status: "active", ExpiresAt: now}}
	if err := issueEditClaim(st, 1, now); err != nil {
		t.Fatal(err)
	}
}

func TestIssueEditBackendGitHub(t *testing.T) {
	f, i, _ := newEditFixture(t)
	ctx := context.Background()
	if _, err := f.manager.ApproveTask(ctx, i.Number, "alice"); err != nil {
		t.Fatal(err)
	}
	state, err := f.manager.GetCurrentState(ctx, i.Number)
	if err != nil {
		t.Fatal(err)
	}
	// Keep all Git coordination traffic inside a temporary bare origin.
	origin := filepath.Join(t.TempDir(), "origin.git")
	for _, args := range [][]string{{"init", "--bare", origin}, {"-C", f.root, "remote", "add", "origin", origin}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	store := coordination.NewGitStore(f.root)
	data, _ := json.Marshal(state)
	if _, err := store.CompareAndSwap(ctx, i.Number, "", data); err != nil {
		t.Fatal(err)
	}
	current, _ := f.tracker.GetIssue(ctx, i.Number)
	reads, patches, labelWrites := 0, 0, 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user":
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "alice", "type": "User"})
		case strings.HasSuffix(r.URL.Path, "/permission"):
			_ = json.NewEncoder(w).Encode(map[string]string{"permission": "write"})
		case strings.Contains(r.URL.Path, "/labels"):
			labelWrites++
			if r.Method == "DELETE" {
				w.WriteHeader(204)
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/"):
			if r.Method == "GET" {
				reads++
			} else if r.Method == "PATCH" {
				patches++
				var p map[string]string
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Error(err)
				}
				if b, ok := p["body"]; ok {
					current.Body = b
				}
				if title, ok := p["title"]; ok {
					current.Title = title
				}
			} else {
				t.Errorf("unexpected %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": i.Number, "title": current.Title, "body": current.Body, "user": map[string]string{"login": "alice"}, "labels": []map[string]string{{"name": "swarm:ready"}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "test-token", Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		resp := rec.Result()
		resp.Request = req
		return resp, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	tr := tracker.NewGitHubTrackerWithClient("octo", "swarm", client)
	out, err := runEdit(f, tr, time.Now(), fmt.Sprint(i.Number), "--objective", "new objective", "--title", "new title")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if reads != 2 || patches != 2 || labelWrites != 2 || current.Title != "new title" || !strings.Contains(current.Body, "new objective") {
		t.Fatalf("reads=%d patches=%d labels=%d issue=%+v", reads, patches, labelWrites, current)
	}
	after, _, err := store.Load(ctx, i.Number)
	if err != nil || string(after) != string(data) {
		t.Fatalf("state mutated: %v", err)
	}
}

func TestIssueEditState(t *testing.T) {
	for _, kind := range []string{"corrupt", "unreadable", "missing"} {
		t.Run(kind, func(t *testing.T) {
			f, i, r := newEditFixture(t)
			dir := filepath.Join(f.root, ".swarm", "coordination")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, fmt.Sprintf("issue-%d.json", i.Number))
			switch kind {
			case "corrupt":
				if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
			}
			_, err := runEdit(f, r, time.Now(), fmt.Sprint(i.Number), "--objective", "new objective")
			if kind == "missing" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || r.writes != 0 {
				t.Fatalf("err=%v writes=%d", err, r.writes)
			}
		})
	}
}

func TestIssueEditApprovalStaleAndTrustedLabel(t *testing.T) {
	for _, kind := range []string{"stale", "trusted label", "ready without state"} {
		t.Run(kind, func(t *testing.T) {
			f, i, r := newEditFixture(t)
			ctx := context.Background()
			if kind == "ready without state" {
				if err := f.tracker.AddLabels(ctx, i.Number, []string{"swarm:ready"}); err != nil {
					t.Fatal(err)
				}
			} else {
				if kind == "trusted label" {
					path := filepath.Join(f.root, ".swarm", "config.yaml")
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					data = bytes.ReplaceAll(data, []byte("swarm:ready"), []byte("custom:ready"))
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
					for _, args := range [][]string{{"add", ".swarm/config.yaml"}, {"commit", "-m", "custom ready label"}} {
						if out, err := exec.Command("git", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
							t.Fatalf("%v %s", err, out)
						}
					}
				}
				if _, err := f.manager.ApproveTask(ctx, i.Number, "alice"); err != nil {
					t.Fatal(err)
				}
				if kind == "trusted label" {
					path := filepath.Join(f.root, ".swarm", "config.yaml")
					data, _ := os.ReadFile(path)
					if err := os.WriteFile(path, bytes.ReplaceAll(data, []byte("custom:ready"), []byte("untrusted:ready")), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "stale" {
					if _, err := f.tracker.UpdateIssueBody(ctx, i.Number, strings.Replace(i.Body, "old objective", "stale objective", 1)); err != nil {
						t.Fatal(err)
					}
				}
			}
			out, err := runEdit(f, r, time.Now(), fmt.Sprint(i.Number), "--objective", "new objective")
			if err != nil {
				t.Fatalf("%v %s", err, out)
			}
			got, _ := f.tracker.GetIssue(ctx, i.Number)
			if kind == "ready without state" {
				if !strings.Contains(out, "no recorded approval") {
					t.Fatal(out)
				}
			} else {
				for _, label := range got.Labels {
					if strings.HasSuffix(label, ":ready") {
						t.Fatal(got.Labels)
					}
				}
			}
		})
	}
}

func TestIssueEditConcurrentStateRecheck(t *testing.T) {
	for _, kind := range []string{"claim", "corrupt state"} {
		t.Run(kind, func(t *testing.T) {
			f, i, r := newEditFixture(t)
			ctx := context.Background()
			if _, err := f.manager.ApproveTask(ctx, i.Number, "alice"); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			f.manager.SetNowFunc(func() time.Time { return now })
			calls := 0
			opts := &RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: r, nowFn: func() time.Time {
				calls++
				if calls == 1 { // Initial state has been inspected; the final read must catch this.
					if kind == "claim" {
						if _, err := f.manager.Acquire(ctx, i.Number, "alice", "mock", "plan", time.Hour, 3); err != nil {
							t.Fatal(err)
						}
					} else {
						path := filepath.Join(f.root, ".swarm", "coordination", fmt.Sprintf("issue-%d.json", i.Number))
						if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				return now
			}}
			cmd := newIssueEditCmd(opts)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{fmt.Sprint(i.Number), "--objective", "new objective"})
			if err := cmd.Execute(); err == nil || r.writes != 0 {
				t.Fatalf("err=%v writes=%d", err, r.writes)
			}
		})
	}
}

func TestIssueEditDryRunApprovedAndClaimJSON(t *testing.T) {
	for _, claim := range []bool{false, true} {
		t.Run(fmt.Sprint(claim), func(t *testing.T) {
			f, i, r := newEditFixture(t)
			ctx := context.Background()
			if _, err := f.manager.ApproveTask(ctx, i.Number, "alice"); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			f.manager.SetNowFunc(func() time.Time { return now })
			if claim {
				if _, err := f.manager.Acquire(ctx, i.Number, "alice", "mock", "plan", time.Hour, 3); err != nil {
					t.Fatal(err)
				}
			}
			before, _, err := f.manager.Store().Load(ctx, i.Number)
			if err != nil {
				t.Fatal(err)
			}
			oldIssue, _ := f.tracker.GetIssue(ctx, i.Number)
			out, err := runEdit(f, r, now, fmt.Sprint(i.Number), "--objective", "new objective", "--dry-run", "--json")
			if (err != nil) != claim {
				t.Fatalf("%v %s", err, out)
			}
			var preview issueEditPreview
			if err := json.Unmarshal([]byte(out), &preview); err != nil {
				t.Fatal(err, out)
			}
			if preview.Next != fmt.Sprintf("swarm issue approve %d", i.Number) || !preview.DigestChanged || !strings.Contains(preview.Diff, "-old objective\n+new objective\n") {
				t.Fatalf("%+v", preview)
			}
			after, _, err := f.manager.Store().Load(ctx, i.Number)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("state changed", err)
			}
			got, _ := f.tracker.GetIssue(ctx, i.Number)
			if r.writes != 0 || !reflect.DeepEqual(oldIssue, got) {
				t.Fatal("dry run wrote tracker")
			}
		})
	}
}

func TestIssueEditDiffContext(t *testing.T) {
	want := "--- a/body\n+++ b/body\n@@ -1 +1 @@\n-old\n+new\n"
	if got := issueEditDiff("old\n", "new\n"); got != want {
		t.Fatalf("%q", got)
	}
	var body strings.Builder
	for n := 0; n < 30; n++ {
		fmt.Fprintf(&body, "line %d\n", n)
	}
	before := body.String()
	after := strings.Replace(strings.Replace(before, "line 2\n", "first change\n", 1), "line 25\n", "second change\n", 1)
	diff := issueEditDiff(before, after)
	if strings.Count(diff, "@@ -") != 2 || !strings.Contains(diff, "-line 2\n+first change\n") || !strings.Contains(diff, "-line 25\n+second change\n") {
		t.Fatal(diff)
	}
	large := issueEditDiff(strings.Repeat("a\n", 3000), strings.Repeat("b\n", 3000))
	if strings.Count(large, "-a\n") != 3000 || strings.Count(large, "+b\n") != 3000 {
		t.Fatal("large diff omitted content")
	}
}
