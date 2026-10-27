package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
)

// fakeGitHubIssues serves the issue REST calls create, lint, and edit make.
type fakeGitHubIssues struct {
	t      *testing.T
	mu     sync.Mutex
	next   int
	issues map[int]*tracker.Issue
	writes int
}

func newFakeGitHubTracker(t *testing.T, seed ...*tracker.Issue) (*fakeGitHubIssues, tracker.Tracker) {
	t.Helper()
	fake := &fakeGitHubIssues{t: t, next: 1, issues: map[int]*tracker.Issue{}}
	for _, i := range seed {
		copied := *i
		fake.issues[i.Number] = &copied
		if i.Number >= fake.next {
			fake.next = i.Number + 1
		}
	}
	client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "test-token", Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		fake.ServeHTTP(rec, req)
		resp := rec.Result()
		resp.Request = req
		return resp, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return fake, tracker.NewGitHubTrackerWithClient("octo", "swarm", client)
}

func (g *fakeGitHubIssues) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/issues"), "/")
	switch {
	case r.URL.Path == "/user":
		_ = json.NewEncoder(w).Encode(map[string]string{"login": "alice", "type": "User"})
	case strings.HasSuffix(r.URL.Path, "/permission"):
		_ = json.NewEncoder(w).Encode(map[string]string{"permission": "write"})
	case r.URL.Path == "/repos/octo/swarm/issues" && r.Method == "POST":
		g.writes++
		var p struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			g.t.Error(err)
		}
		issue := &tracker.Issue{Number: g.next, Title: p.Title, Body: p.Body, Labels: p.Labels}
		g.issues[issue.Number] = issue
		g.next++
		g.encode(w, issue)
	case len(parts) >= 2 && strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/"):
		n, _ := strconv.Atoi(parts[1])
		issue := g.issues[n]
		if issue == nil {
			http.NotFound(w, r)
			return
		}
		if len(parts) >= 3 && parts[2] == "labels" {
			g.writes++
			if r.Method == "DELETE" && len(parts) == 4 {
				name, _ := url.PathUnescape(parts[3])
				var kept []string
				for _, l := range issue.Labels {
					if l != name {
						kept = append(kept, l)
					}
				}
				issue.Labels = kept
				w.WriteHeader(204)
				return
			}
			var p struct {
				Labels []string `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				g.t.Error(err)
			}
			issue.Labels = append(issue.Labels, p.Labels...)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		switch r.Method {
		case "GET":
		case "PATCH":
			g.writes++
			var p map[string]string
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				g.t.Error(err)
			}
			if b, ok := p["body"]; ok {
				issue.Body = b
			}
			if title, ok := p["title"]; ok {
				issue.Title = title
			}
		default:
			g.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		g.encode(w, issue)
	default:
		g.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (g *fakeGitHubIssues) encode(w http.ResponseWriter, i *tracker.Issue) {
	labels := []map[string]string{}
	for _, l := range i.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"number": i.Number, "title": i.Title, "body": i.Body, "state": "open", "user": map[string]string{"login": "alice"}, "labels": labels})
}

func (g *fakeGitHubIssues) labels(n int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.issues[n].Labels...)
}

// addTestOrigin gives the fixture a bare origin so non-local trackers keep
// coordination state in a temporary repository.
func addTestOrigin(t *testing.T, f *cliFixture) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	for _, args := range [][]string{{"init", "--bare", "--initial-branch=main", origin}, {"-C", f.root, "remote", "add", "origin", origin}, {"-C", f.root, "push", "origin", "HEAD:refs/heads/main"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
}

func runIssueCmd(f *cliFixture, tr tracker.Tracker, args ...string) (string, error) {
	opts := f.options(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: tr, nowFn: time.Now})
	cmd := newIssueCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// checkAllowHarnessChanges runs create, lint, and edit against tr and checks
// that the stored body, lint output, and JSON results agree on the permission.
func checkAllowHarnessChanges(t *testing.T, f *cliFixture, tr tracker.Tracker) {
	t.Helper()
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out, err := runIssueCmd(f, tr, args...)
		if err != nil {
			t.Fatalf("issue %v: %v\n%s", args, err, out)
		}
		return out
	}
	stored := func(n int) bool {
		t.Helper()
		issue, err := tr.GetIssue(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
		if err != nil {
			t.Fatal(err)
		}
		return wu.AllowHarnessChanges
	}
	lint := func(n int, want bool) {
		t.Helper()
		var result struct {
			Allow    *bool `json:"allow_harness_changes"`
			WorkUnit struct {
				Allow bool `json:"allow_harness_changes"`
			} `json:"work_unit"`
		}
		raw := run("lint", strconv.Itoa(n), "--json")
		if err := json.Unmarshal([]byte(raw), &result); err != nil || result.Allow == nil || *result.Allow != want || result.WorkUnit.Allow != want {
			t.Fatalf("lint JSON want %v: %s (%v)", want, raw, err)
		}
		if text := run("lint", strconv.Itoa(n)); !strings.Contains(text, fmt.Sprintf("allow_harness_changes=%t", want)) {
			t.Fatalf("lint text want %v:\n%s", want, text)
		}
		if stored(n) != want {
			t.Fatalf("stored body of #%d does not parse as %v", n, want)
		}
	}
	create := func(want bool, flags ...string) int {
		t.Helper()
		args := append([]string{"create", "--json", "--title", "Harness task", "--objective", "Update the CI workflow", "--constraint", "Keep checks", "--acceptance", "CI passes"}, flags...)
		var created struct {
			Allow *bool          `json:"allow_harness_changes"`
			Issue *tracker.Issue `json:"issue"`
		}
		raw := run(args...)
		if err := json.Unmarshal([]byte(raw), &created); err != nil || created.Issue == nil || created.Allow == nil || *created.Allow != want {
			t.Fatalf("create %v JSON: %s (%v)", flags, raw, err)
		}
		if strings.Contains(created.Issue.Body, "### Allow Harness Changes") != want {
			t.Fatalf("create %v body:\n%s", flags, created.Issue.Body)
		}
		lint(created.Issue.Number, want)
		return created.Issue.Number
	}
	create(true, "--allow-harness-changes")
	create(false, "--allow-harness-changes=false")
	n := create(false)

	edit := func(want bool, args ...string) issueEditPreview {
		t.Helper()
		var p issueEditPreview
		raw := run(append([]string{"edit", strconv.Itoa(n), "--json"}, args...)...)
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.AllowHarnessChanges != want {
			t.Fatalf("edit %v JSON want %v: %s (%v)", args, want, raw, err)
		}
		return p
	}
	if p := edit(true, "--allow-harness-changes", "--dry-run"); p.Status != "preview" || !p.DigestChanged || stored(n) {
		t.Fatalf("dry run wrote or misreported: %+v", p)
	}
	if p := edit(true, "--allow-harness-changes=true"); p.Status != "edited" {
		t.Fatalf("enable: %+v", p)
	}
	lint(n, true)
	if p := edit(true, "--objective", "Update the CI workflow safely"); p.Status != "edited" {
		t.Fatalf("unrelated edit: %+v", p)
	}
	lint(n, true)
	if p := edit(false, "--allow-harness-changes=false"); p.Status != "edited" {
		t.Fatalf("disable: %+v", p)
	}
	lint(n, false)
	if out, err := runIssueCmd(f, tr, "edit", strconv.Itoa(n), "--allow-harness-changes=maybe"); err == nil {
		t.Fatalf("invalid boolean accepted: %s", out)
	}
}

func TestIssueAllowHarnessChangesLocal(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	checkAllowHarnessChanges(t, f, f.tracker)
	cmd := newIssueCmd(&RootOptions{})
	for _, sub := range []string{"create", "edit"} {
		c, _, err := cmd.Find([]string{sub})
		if err != nil || c.Flags().Lookup("allow-harness-changes") == nil {
			t.Fatalf("%s lacks --allow-harness-changes: %v", sub, err)
		}
	}
}

func TestIssueAllowHarnessChangesGitHub(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	addTestOrigin(t, f)
	_, tr := newFakeGitHubTracker(t)
	checkAllowHarnessChanges(t, f, tr)
}

func TestIssueEditAllowHarnessChangesApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	has := func(labels []string, want string) bool {
		for _, l := range labels {
			if l == want {
				return true
			}
		}
		return false
	}
	t.Run("local", func(t *testing.T) {
		t.Parallel()
		f, i, r := newEditFixture(t)
		n := strconv.Itoa(i.Number)
		for _, step := range []struct {
			flag    string
			changed bool
		}{
			{"--allow-harness-changes=true", true},
			{"--allow-harness-changes=true", false},
			{"--allow-harness-changes=false", true},
			{"--allow-harness-changes=false", false},
		} {
			if _, err := f.manager.ApproveTask(ctx, i.Number, "alice"); err != nil {
				t.Fatal(err)
			}
			approved, _ := f.manager.GetCurrentState(ctx, i.Number)
			out, err := runEdit(f, r, time.Now(), n, step.flag)
			if err != nil {
				t.Fatalf("%s: %v %s", step.flag, err, out)
			}
			state, _ := f.manager.GetCurrentState(ctx, i.Number)
			if !reflect.DeepEqual(approved, state) {
				t.Fatalf("%s: approval record mutated", step.flag)
			}
			got, _ := f.tracker.GetIssue(ctx, i.Number)
			wu, err := contract.ParseWorkUnitFromIssue(i.Number, got.Title, got.Body)
			if err != nil {
				t.Fatal(err)
			}
			if (contract.DigestWorkUnit(wu) != approved.TaskDigest) != step.changed {
				t.Fatalf("%s: digest changed=%v", step.flag, !step.changed)
			}
			if has(got.Labels, "swarm:ready") == step.changed || has(got.Labels, "swarm:draft") != step.changed {
				t.Fatalf("%s: labels %v", step.flag, got.Labels)
			}
			if step.changed != strings.Contains(out, "revision requires separate approval") {
				t.Fatalf("%s: approval preview:\n%s", step.flag, out)
			}
		}
	})
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("github from %v", allow), func(t *testing.T) {
			t.Parallel()
			f := newCLIFixture(t)
			body := contract.FormatIssueMarkdown(&contract.WorkUnit{Objective: "old objective", Constraints: []string{"keep"}, Acceptance: []string{"works"}, Workflow: "feature", AllowHarnessChanges: allow})
			i, err := f.tracker.CreateIssue(ctx, "title", body, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.manager.ApproveTask(ctx, i.Number, "alice"); err != nil {
				t.Fatal(err)
			}
			state, err := f.manager.GetCurrentState(ctx, i.Number)
			if err != nil {
				t.Fatal(err)
			}
			addTestOrigin(t, f)
			store := trustedLedgerStore(t, f.root, "")
			data, _ := json.Marshal(state)
			if _, err := store.CompareAndSwap(ctx, i.Number, "", data); err != nil {
				t.Fatal(err)
			}
			fake, tr := newFakeGitHubTracker(t, &tracker.Issue{Number: i.Number, Title: i.Title, Body: i.Body, Labels: []string{"swarm:ready", "custom"}})
			n := strconv.Itoa(i.Number)
			out, err := runEdit(f, tr, time.Now(), n, fmt.Sprintf("--allow-harness-changes=%t", allow))
			if err != nil || fake.writes != 0 || !has(fake.labels(i.Number), "swarm:ready") {
				t.Fatalf("setting the current value wrote: %v writes=%d %s", err, fake.writes, out)
			}
			out, err = runEdit(f, tr, time.Now(), n, fmt.Sprintf("--allow-harness-changes=%t", !allow))
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			labels := fake.labels(i.Number)
			if has(labels, "swarm:ready") || !has(labels, "swarm:draft") || !has(labels, "custom") {
				t.Fatalf("labels %v", labels)
			}
			edited, _ := tr.GetIssue(ctx, i.Number)
			wu, err := contract.ParseWorkUnitFromIssue(i.Number, edited.Title, edited.Body)
			if err != nil || wu.AllowHarnessChanges != !allow || contract.DigestWorkUnit(wu) == state.TaskDigest {
				t.Fatalf("edited contract %+v %v", wu, err)
			}
			after, _, err := store.Load(ctx, i.Number)
			if err != nil || string(after) != string(data) {
				t.Fatalf("approval record mutated: %v", err)
			}
		})
	}
}
