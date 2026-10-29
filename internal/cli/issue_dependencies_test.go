package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/cli/go-gh/v2/pkg/api"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
)

type driftTracker struct {
	tracker.Tracker
	refs          []tracker.BlockedByReference
	err           error
	links         [][2]int
	failHierarchy bool
	issue         *tracker.Issue
}

func (d *driftTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	if d.issue != nil && d.issue.Number == n {
		return d.issue, nil
	}
	return d.Tracker.GetIssue(ctx, n)
}
func (d *driftTracker) BackendName() string { return "github" }
func (d *driftTracker) ListBlockedBy(context.Context, int) ([]tracker.BlockedByReference, error) {
	return d.refs, d.err
}
func (d *driftTracker) LinkBlockedBy(_ context.Context, n, dep int) error {
	d.links = append(d.links, [2]int{n, dep})
	return errors.New("projection denied")
}
func (d *driftTracker) OrderSubIssues(context.Context, int, []int) error {
	return errors.New("priority denied")
}
func (d *driftTracker) LinkSubIssue(ctx context.Context, parent, child int) error {
	if d.failHierarchy {
		return errors.New("hierarchy denied")
	}
	return d.Tracker.LinkSubIssue(ctx, parent, child)
}

func TestNativeProjectionLintQualified(t *testing.T) {
	issue := &tracker.Issue{Number: 11, HTMLURL: "https://github.com/current/repo/issues/11"}
	d := &driftTracker{refs: []tracker.BlockedByReference{{Owner: "other", Repo: "repo", Number: 7}}}
	want := []string{"missing native blocked-by link: current/repo#7", "native blocked-by link absent from depends_on: other/repo#7"}
	if got := dependencyDriftWarnings(t.Context(), d, issue, []int{7, 7, 11}); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	d.refs = append(d.refs, tracker.BlockedByReference{Owner: "current", Repo: "repo", Number: 7})
	if got := dependencyDriftWarnings(t.Context(), d, issue, []int{7}); !reflect.DeepEqual(got, want[1:]) {
		t.Fatal(got)
	}
	d.err = errors.New("denied")
	if got := dependencyDriftWarnings(t.Context(), d, issue, []int{7}); len(got) != 1 || !strings.Contains(got[0], "unavailable") {
		t.Fatal(got)
	}
	d.err = nil
	issue.HTMLURL = "bad"
	if got := dependencyDriftWarnings(t.Context(), d, issue, []int{7}); len(got) != 1 || !strings.Contains(got[0], "unavailable") {
		t.Fatal(got)
	}
}

func executeProjectionIssue(f *cliFixture, tr tracker.Tracker, args ...string) (string, string, error) {
	opts := f.options(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: tr, nowFn: time.Now})
	cmd := newIssueCmd(opts)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestNativeProjectionCreateWarningsAndHierarchy(t *testing.T) {
	f := newCLIFixture(t)
	local := tracker.NewLocalTracker(f.root)
	d := &driftTracker{Tracker: local}
	out, stderr, err := executeProjectionIssue(f, d, "create", "--title", "Dependent feature", "--objective", "Deliver dependent feature", "--acceptance", "Feature works", "--depends-on", "7,8", "--json")
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	var result struct {
		Issue tracker.Issue `json:"issue"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("JSON: %v: %s", err, out)
	}
	if len(d.links) != 2 || strings.Count(stderr, "warning:") != 2 || result.Issue.Number == 0 {
		t.Fatalf("links=%v stderr=%s out=%s", d.links, stderr, out)
	}
	wu, err := contract.ParseWorkUnitFromIssue(result.Issue.Number, result.Issue.Title, result.Issue.Body)
	if err != nil || !reflect.DeepEqual(wu.DependsOn, []int{7, 8}) {
		t.Fatalf("body: %v %v", wu, err)
	}
	parent, err := local.CreateIssue(t.Context(), "parent", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.failHierarchy = true
	_, _, err = executeProjectionIssue(f, d, "create", "--title", "Child feature", "--objective", "Deliver child feature", "--acceptance", "Feature works", "--parent", jsonNumber(parent.Number), "--ready")
	if err == nil || !strings.Contains(err.Error(), "but failed to link as sub-issue") {
		t.Fatalf("%v", err)
	}
	if len(d.links) != 2 {
		t.Fatal("projection after hierarchy failure")
	}
	issues, _ := local.ListIssues(t.Context(), tracker.IssueFilter{State: "all"})
	for _, i := range issues {
		for _, label := range i.Labels {
			if label == "swarm:ready" {
				t.Fatal("approved hierarchy failure")
			}
		}
	}
}

func jsonNumber(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestNativeProjectionLintOutput(t *testing.T) {
	f := newCLIFixture(t)
	wu := &contract.WorkUnit{ID: "test", Objective: "Deliver dependent feature", Acceptance: []string{"Feature works"}, Constraints: []string{}, Workflow: "feature", Gate: "none", DependsOn: []int{7, 8}}
	issue := &tracker.Issue{Number: 11, Title: "feature", Body: contract.FormatIssueMarkdown(wu), HTMLURL: "https://github.com/octo/swarm/issues/11"}
	fake, tr := newFakeGitHubTracker(t, issue)
	d := &driftTracker{Tracker: tr, issue: issue, refs: []tracker.BlockedByReference{{Owner: "octo", Repo: "swarm", Number: 8}, {Owner: "octo", Repo: "swarm", Number: 9}}}
	for _, asJSON := range []bool{false, true} {
		args := []string{"lint", "11"}
		if asJSON {
			args = append(args, "--json")
		}
		out, _, err := executeProjectionIssue(f, d, args...)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "octo/swarm#7") || !strings.Contains(out, "octo/swarm#9") {
			t.Fatal(out)
		}
		if asJSON {
			var result struct {
				Status   string   `json:"status"`
				Warnings []string `json:"warnings"`
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil || result.Status != "valid" || len(result.Warnings) != 2 {
				t.Fatalf("%s %v", out, err)
			}
		}
	}
	if fake.writes != 0 {
		t.Fatal("lint wrote state")
	}
}

func TestNativeProjectionCreateHTTPWarning(t *testing.T) {
	f := newCLIFixture(t)
	fake, _ := newFakeGitHubTracker(t)
	client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "test-token", Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		if strings.HasSuffix(r.URL.Path, "/issues/7") {
			rec.Header().Set("Content-Type", "application/json")
			_, _ = rec.Write([]byte(`{"id":7007,"number":7}`))
		} else if strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by") {
			rec.Header().Set("Content-Type", "application/json")
			if r.Method == "GET" {
				_, _ = rec.Write([]byte(`[]`))
			} else {
				rec.WriteHeader(403)
				_, _ = rec.Write([]byte(`{"message":"denied"}`))
			}
		} else {
			fake.ServeHTTP(rec, r)
		}
		resp := rec.Result()
		resp.Request = r
		return resp, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	tr := tracker.NewGitHubTrackerWithClient("octo", "swarm", client)
	out, stderr, err := executeProjectionIssue(f, tr, "create", "--title", "HTTP feature", "--objective", "Deliver HTTP feature", "--acceptance", "Feature works", "--depends-on", "7", "--json")
	if err != nil || !json.Valid([]byte(out)) || !strings.Contains(stderr, "blocked-by #7") {
		t.Fatalf("out=%s stderr=%s err=%v", out, stderr, err)
	}
	if len(fake.issues) != 1 {
		t.Fatal(fake.issues)
	}
}

func TestNativeProjectionDecomposeAndPlanJSONWarnings(t *testing.T) {
	for _, mode := range []string{"decompose", "plan"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			addTestOrigin(t, f)
			d := &driftTracker{Tracker: f.tracker}
			var out, stderr bytes.Buffer
			opts := f.options(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: d, nowFn: time.Now})
			if mode == "decompose" {
				wu := &contract.WorkUnit{ID: "parent", Objective: "Deliver parent feature", Acceptance: []string{"First works", "Second works"}, Constraints: []string{}, Workflow: "feature", Gate: "none"}
				parent, err := f.tracker.CreateIssue(t.Context(), "parent", contract.FormatIssueMarkdown(wu), nil)
				if err != nil {
					t.Fatal(err)
				}
				cmd := newDecomposeCmd(opts)
				cmd.SetOut(&out)
				cmd.SetErr(&stderr)
				cmd.SetArgs([]string{jsonNumber(parent.Number), "--json"})
				if err := cmd.Execute(); err != nil {
					t.Fatalf("%v %s", err, stderr.String())
				}
			} else {
				path := filepath.Join(f.root, "roadmap.md")
				body := "# Plan\n\n## First feature\n### Objective\nDeliver first feature\n### Acceptance Criteria\n- [ ] First works\n\n## Second feature\n### Objective\nDeliver second feature\n### Acceptance Criteria\n- [ ] Second works\ndepends on: 1\n"
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				cmd := newPlanCmd(opts)
				cmd.SetOut(&out)
				cmd.SetErr(&stderr)
				cmd.SetArgs([]string{"--from", path, "--apply", "--json"})
				if err := cmd.Execute(); err != nil {
					t.Fatalf("%v %s", err, stderr.String())
				}
			}
			if !json.Valid(out.Bytes()) || !strings.Contains(stderr.String(), "blocked-by") || !strings.Contains(stderr.String(), "priority") {
				t.Fatalf("out=%s stderr=%s", out.String(), stderr.String())
			}
			if len(d.links) != 1 {
				t.Fatal(d.links)
			}
		})
	}
}

func TestNativeProjectionLintHTTPPagination(t *testing.T) {
	for _, localFirst := range []bool{false, true} {
		for _, includeLocal := range []bool{false, true} {
			t.Run(jsonNumber(map[bool]int{false: 0, true: 1}[localFirst])+jsonNumber(map[bool]int{false: 0, true: 1}[includeLocal]), func(t *testing.T) {
				f := newCLIFixture(t)
				wu := &contract.WorkUnit{ID: "dependent", Objective: "Deliver feature", Acceptance: []string{"Feature works"}, Constraints: []string{}, Workflow: "feature", Gate: "none", DependsOn: []int{7}}
				issue := &tracker.Issue{Number: 11, Title: "feature", Body: contract.FormatIssueMarkdown(wu), HTMLURL: "https://github.com/octo/swarm/issues/11"}
				cross := map[string]any{"id": 9007, "number": 7, "html_url": "https://github.com/other/repo/issues/7"}
				local := map[string]any{"id": 7007, "number": 7, "html_url": "https://github.com/octo/swarm/issues/7"}
				// Repeated native rows are deliberately deduplicated by qualified identity.
				rows := make([]map[string]any, 100)
				for i := range rows {
					rows[i] = cross
				}
				if includeLocal {
					if localFirst {
						for i := range rows {
							rows[i] = local
						}
						rows = append(rows, cross)
					} else {
						rows = append(rows, local)
					}
				}
				pages := 0
				client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "test-token", Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					rec := httptest.NewRecorder()
					rec.Header().Set("Content-Type", "application/json")
					if r.Method != "GET" {
						t.Errorf("lint mutation: %s", r.Method)
					}
					switch r.URL.Path {
					case "/repos/octo/swarm/issues/11":
						_ = json.NewEncoder(rec).Encode(issue)
					case "/repos/octo/swarm/issues/11/dependencies/blocked_by":
						pages++
						if r.URL.Query().Get("page") == "1" {
							_ = json.NewEncoder(rec).Encode(rows[:100])
						} else {
							_ = json.NewEncoder(rec).Encode(rows[100:])
						}
					default:
						t.Errorf("unexpected %s", r.URL)
						rec.WriteHeader(404)
					}
					resp := rec.Result()
					resp.Request = r
					return resp, nil
				})})
				if err != nil {
					t.Fatal(err)
				}
				tr := tracker.NewGitHubTrackerWithClient("octo", "swarm", client)
				for _, asJSON := range []bool{false, true} {
					args := []string{"lint", "11"}
					if asJSON {
						args = append(args, "--json")
					}
					out, _, err := executeProjectionIssue(f, tr, args...)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(out, "other/repo#7") {
						t.Fatal(out)
					}
					missing := strings.Contains(out, "missing native blocked-by link: octo/swarm#7")
					if missing == includeLocal {
						t.Fatalf("missing=%t includeLocal=%t output=%s", missing, includeLocal, out)
					}
					if asJSON {
						var doc struct {
							Status   string   `json:"status"`
							Warnings []string `json:"warnings"`
						}
						if err := json.Unmarshal([]byte(out), &doc); err != nil {
							t.Fatal(err)
						}
						want := 2
						if includeLocal {
							want = 1
						}
						if doc.Status != "valid" || len(doc.Warnings) != want {
							t.Fatal(out)
						}
					}
				}
				if pages != 4 {
					t.Fatalf("pagination requests=%d", pages)
				}
			})
		}
	}
}
