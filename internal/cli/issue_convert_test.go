package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

type convertRecordingTracker struct {
	*editRecordingTracker
	links       []int
	otherWrites int
	projectFail bool
	deny        bool
}

func (r *convertRecordingTracker) Authorize(ctx context.Context, op string) error {
	if r.deny {
		return errors.New("permission denied")
	}
	return r.Tracker.Authorize(ctx, op)
}
func (r *convertRecordingTracker) LinkBlockedBy(_ context.Context, n, dep int) error {
	r.links = append(r.links, dep)
	if r.projectFail {
		return errors.New("link unavailable")
	}
	return nil
}
func (r *convertRecordingTracker) LinkSubIssue(context.Context, int, int) error {
	r.otherWrites++
	return nil
}
func (r *convertRecordingTracker) CreateIssue(ctx context.Context, title, body string, labels []string) (*tracker.Issue, error) {
	r.otherWrites++
	return r.Tracker.CreateIssue(ctx, title, body, labels)
}

func newConvertFixture(t *testing.T) (*cliFixture, *tracker.Issue, *convertRecordingTracker) {
	t.Helper()
	f := newCLIFixture(t)
	i, err := f.tracker.CreateIssue(context.Background(), "intake title", "Original report\r\n<!-- ```yaml -->\n", []string{"swarm:ready", "swarm:needs-scope", "swarm:community", "custom"})
	if err != nil {
		t.Fatal(err)
	}
	return f, i, &convertRecordingTracker{editRecordingTracker: &editRecordingTracker{Tracker: f.tracker}}
}
func runConvert(f *cliFixture, r tracker.Tracker, args ...string) (string, error) {
	cmd := newIssueConvertCmd(f.options(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: r}))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}
func convertArgs(n int) []string {
	return []string{fmt.Sprint(n), "--objective", "Implement the scoped outcome", "--acceptance", "Observable outcome works"}
}

func TestIssueConvertFieldsAndPreview(t *testing.T) {
	for _, kind := range []string{"dry-run", "json", "invalid", "missing", "parent"} {
		t.Run(kind, func(t *testing.T) {
			f, i, r := newConvertFixture(t)
			args := convertArgs(i.Number)
			switch kind {
			case "dry-run":
				args = append(args, "--dry-run")
			case "json":
				args = append(args, "--dry-run", "--json")
			case "invalid":
				args = append(args, "--gate", "invalid")
			case "missing":
				args = []string{fmt.Sprint(i.Number)}
			case "parent":
				args = append(args, "--parent", "2")
			}
			out, err := runConvert(f, r, args...)
			valid := kind == "dry-run" || kind == "json"
			if (err == nil) != valid || r.writes != 0 || len(r.links) != 0 || r.otherWrites != 0 {
				t.Fatalf("%v writes=%d links=%v out=%s", err, r.writes, r.links, out)
			}
			if kind == "json" {
				var p issueConvertPreview
				if err := json.Unmarshal([]byte(out), &p); err != nil || p.Gate != contract.GateNone || p.Lint != "valid" || p.Diff == "" {
					t.Fatalf("%+v %v", p, err)
				}
			}
		})
	}
}
func TestIssueConvertBackendLocal(t *testing.T) {
	f, i, r := newConvertFixture(t)
	args := append(convertArgs(i.Number), "--title", "Converted title", "--constraint", "Keep metadata", "--constraint", "Stay scoped", "--acceptance", "Second criterion", "--depends-on", "2,2,1", "--capability", "coding,testing", "--allow-harness-changes", "--size", "multi_area", "--uncertainty", "known", "--risk", "sensitive", "--json")
	out, err := runConvert(f, r, args...)
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	got, err := tracker.NewLocalTracker(f.root).GetIssue(context.Background(), i.Number)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(got.Number, got.Title, got.Body)
	if err != nil || wu.IssueNumber != i.Number || wu.Gate != contract.GateNone || len(wu.Constraints) != 2 || len(wu.Acceptance) != 2 || len(wu.RequiredCapabilities) != 2 || !wu.AllowHarnessChanges || wu.Size != "multi_area" || wu.Risk != "sensitive" || wu.Uncertainty != "known" {
		t.Fatalf("%+v %v", wu, err)
	}
	if got.HTMLURL != i.HTMLURL || got.Author != i.Author || got.State != i.State || !reflect.DeepEqual(got.Assignees, i.Assignees) || !got.CreatedAt.Equal(i.CreatedAt) {
		t.Fatalf("metadata changed: %+v", got)
	}
	if !reflect.DeepEqual(got.Labels, []string{"swarm:community", "custom", "swarm:draft"}) {
		t.Fatal(got.Labels)
	}
	if !reflect.DeepEqual(r.links, []int{2}) || r.otherWrites != 0 {
		t.Fatalf("links %v writes %d", r.links, r.otherWrites)
	}
	if !strings.HasPrefix(lease.DescriptionIssueSection(wu, nil, "", lease.ProjectionContext{}), fmt.Sprintf("Closes #%d\n", i.Number)) {
		t.Fatal("closing identity changed")
	}
	if !strings.Contains(contract.FormatIssueMarkdown(wu), "Implement the scoped outcome") {
		t.Fatal(wu)
	}
	out, err = runConvert(f, r, convertArgs(i.Number)...)
	if err == nil || !strings.Contains(err.Error(), "issue edit") {
		t.Fatalf("%v %s", err, out)
	}
	out, err = runEdit(f, r, time.Now(), fmt.Sprint(i.Number), "--objective", "Updated explicit objective")
	if err != nil {
		t.Fatalf("edit after convert: %v %s", err, out)
	}
}
func TestIssueConvertRefusals(t *testing.T) {
	for _, kind := range []string{"epic", "closed", "pr", "denied", "valid", "fork", "state"} {
		t.Run(kind, func(t *testing.T) {
			f, i, r := newConvertFixture(t)
			r.onRead = func(_ int, got *tracker.Issue) error {
				switch kind {
				case "epic":
					got.Type = "Epic"
				case "closed":
					got.State = "closed"
				case "pr":
					got.IsPullRequest = true
				}
				return nil
			}
			if kind == "denied" {
				r.deny = true
			}
			if kind == "valid" || kind == "state" {
				_, err := f.tracker.UpdateIssueBody(context.Background(), i.Number, contract.FormatIssueMarkdown(&contract.WorkUnit{Objective: "Existing scoped objective", Constraints: []string{}, Acceptance: []string{"works"}, Workflow: "feature"}))
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "state" {
				if _, err := f.manager.ApproveTask(context.Background(), i.Number, "alice"); err != nil {
					t.Fatal(err)
				}
				_, _ = f.tracker.UpdateIssueBody(context.Background(), i.Number, "raw report")
			}
			var err error
			if kind == "fork" {
				cmd := newIssueConvertCmd(f.options(&RootOptions{Fork: true}))
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(&bytes.Buffer{})
				cmd.SetArgs(convertArgs(i.Number))
				err = cmd.Execute()
			} else {
				_, err = runConvert(f, r, convertArgs(i.Number)...)
			}
			if err == nil || r.writes != 0 || len(r.links) != 0 || r.otherWrites != 0 {
				t.Fatalf("%v writes=%d", err, r.writes)
			}
		})
	}
}
func TestIssueConvertConcurrent(t *testing.T) {
	for _, kind := range []string{"title", "body", "type", "closed", "pr", "labels", "assignees", "timestamp", "error", "state"} {
		t.Run(kind, func(t *testing.T) {
			f, i, r := newConvertFixture(t)
			r.onRead = func(read int, got *tracker.Issue) error {
				if read != 2 {
					return nil
				}
				switch kind {
				case "title":
					got.Title += " edit"
				case "body":
					got.Body += " edit"
				case "type":
					got.Type = "Epic"
				case "closed":
					got.State = "closed"
				case "pr":
					got.IsPullRequest = true
				case "labels":
					got.Labels = append(got.Labels, "changed")
				case "assignees":
					got.Assignees = []string{"bob"}
				case "timestamp":
					got.UpdatedAt = time.Now()
				case "error":
					return errors.New("read failed")
				case "state":
					original := got.Body
					_, _ = f.tracker.UpdateIssueBody(context.Background(), i.Number, contract.FormatIssueMarkdown(&contract.WorkUnit{Objective: "Existing scoped objective", Constraints: []string{}, Acceptance: []string{"works"}, Workflow: "feature"}))
					if _, err := f.manager.ApproveTask(context.Background(), i.Number, "alice"); err != nil {
						return err
					}
					_, _ = f.tracker.UpdateIssueBody(context.Background(), i.Number, original)
				}
				return nil
			}
			out, err := runConvert(f, r, convertArgs(i.Number)...)
			if err == nil || r.writes != 0 || len(r.links) != 0 || r.otherWrites != 0 {
				t.Fatalf("%v writes=%d %s", err, r.writes, out)
			}
		})
	}
}
func TestIssueConvertPartialFailure(t *testing.T) {
	for _, kind := range []string{"remove", "labels", "body", "title", "projection"} {
		t.Run(kind, func(t *testing.T) {
			f, i, r := newConvertFixture(t)
			r.fail = kind
			r.projectFail = kind == "projection"
			args := append(convertArgs(i.Number), "--title", "New title", "--depends-on", "2", "--json")
			out, err := runConvert(f, r, args...)
			var p issueConvertPreview
			if e := json.Unmarshal([]byte(out), &p); e != nil {
				t.Fatalf("%v %s", e, out)
			}
			if kind == "projection" {
				if err != nil || p.Status != "converted-with-warnings" || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "retry") {
					t.Fatalf("%v %+v", err, p)
				}
			} else {
				if err == nil || p.Status != "failed" || !strings.Contains(p.Error, "inspect") || len(r.links) != 0 {
					t.Fatalf("%v %+v", err, p)
				}
				if kind == "title" && !strings.Contains(p.Error, "issue edit") {
					t.Fatal(p.Error)
				}
			}
		})
	}
}

func TestIssueConvertBackendGitHub(t *testing.T) {
	for _, pr := range []bool{false, true} {
		t.Run(fmt.Sprint(pr), func(t *testing.T) {
			f, i, _ := newConvertFixture(t)
			origin := filepath.Join(t.TempDir(), "origin.git")
			for _, args := range [][]string{{"init", "--bare", "--initial-branch=main", origin}, {"-C", f.root, "remote", "add", "origin", origin}, {"-C", f.root, "push", "origin", "HEAD:refs/heads/main"}} {
				if out, err := gitfixture.Command(t, "", args...).CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			reads, patches, labels, permissions := 0, 0, 0, 0
			current := *i
			client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "test-token", Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				rec.Header().Set("Content-Type", "application/json")
				switch {
				case req.URL.Path == "/user":
					fmt.Fprint(rec, `{"login":"alice","type":"User"}`)
				case strings.HasSuffix(req.URL.Path, "/permission"):
					permissions++
					fmt.Fprint(rec, `{"permission":"write"}`)
				case strings.Contains(req.URL.Path, "/labels"):
					labels++
					if req.Method == "DELETE" {
						rec.WriteHeader(204)
					} else {
						fmt.Fprint(rec, `[]`)
					}
				case strings.HasPrefix(req.URL.Path, "/repos/octo/swarm/issues/"):
					if req.Method == "GET" {
						reads++
					} else if req.Method == "PATCH" {
						patches++
						var p map[string]string
						if e := json.NewDecoder(req.Body).Decode(&p); e != nil {
							t.Error(e)
						}
						if len(p) != 1 {
							t.Errorf("unexpected PATCH fields %v", p)
						}
						for k, v := range p {
							switch k {
							case "body":
								current.Body = v
							case "title":
								current.Title = v
							default:
								t.Errorf("unexpected PATCH field %s", k)
							}
						}
					} else {
						t.Errorf("unexpected %s", req.Method)
					}
					payload := map[string]any{"number": i.Number, "title": current.Title, "body": current.Body, "state": "open", "html_url": i.HTMLURL, "user": map[string]string{"login": "alice"}, "labels": []map[string]string{{"name": "swarm:ready"}}}
					if pr {
						payload["pull_request"] = map[string]string{"url": "https://example.test/pulls/1"}
					}
					_ = json.NewEncoder(rec).Encode(payload)
				default:
					t.Errorf("unexpected %s %s", req.Method, req.URL.Path)
					rec.WriteHeader(404)
				}
				resp := rec.Result()
				resp.Request = req
				return resp, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			tr := tracker.NewGitHubTrackerWithClient("octo", "swarm", client)
			out, err := runConvert(f, tr, convertArgs(i.Number)...)
			if pr {
				if err == nil || patches != 0 || labels != 0 {
					t.Fatalf("%v %s", err, out)
				}
				return
			}
			if err != nil || reads != 2 || patches != 1 || labels != 2 || permissions != 1 {
				t.Fatalf("%v reads=%d patches=%d labels=%d auth=%d %s", err, reads, patches, labels, permissions, out)
			}
			if _, err := contract.ParseWorkUnitFromIssue(i.Number, current.Title, current.Body); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIssueConvertAuthoritativeState(t *testing.T) {
	for _, kind := range []string{"active", "expired", "released", "approved", "checkpoint", "pr", "revision", "corrupt", "read-failure"} {
		for _, late := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/late=%v", kind, late), func(t *testing.T) {
				f, i, r := newConvertFixture(t)
				install := func() {
					st := lease.IssueState{}
					switch kind {
					case "active":
						st.Lease = contract.Lease{Status: "active", Owner: "other", ExpiresAt: time.Now().Add(time.Hour)}
					case "expired":
						st.Lease = contract.Lease{Status: "active", ExpiresAt: time.Now().Add(-time.Hour)}
					case "released":
						st.Lease.Status = "released"
					case "approved":
						st.ApprovedBy = "maintainer"
					case "checkpoint":
						st.Branch = "swarm/task"
					case "pr":
						st.PRNumber = 42
					case "revision":
						st.PRRevision = &lease.PRRevision{}
					}
					data, err := json.Marshal(st)
					if err != nil {
						t.Fatal(err)
					}
					if kind == "corrupt" {
						data = []byte("invalid")
					}
					if _, err = f.manager.Store().CompareAndSwap(context.Background(), i.Number, "", data); err != nil {
						t.Fatal(err)
					}
					if kind == "read-failure" {
						path := filepath.Join(f.root, ".swarm", "coordination", fmt.Sprintf("issue-%d.json", i.Number))
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(path, 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
				if late {
					r.onRead = func(read int, _ *tracker.Issue) error {
						if read == 2 {
							install()
						}
						return nil
					}
				} else {
					install()
				}
				out, err := runConvert(f, r, convertArgs(i.Number)...)
				if err == nil || r.writes != 0 || len(r.links) != 0 {
					t.Fatalf("%v %s", err, out)
				}
			})
		}
	}
}

func TestIssueConvertTrustedPolicy(t *testing.T) {
	for _, kind := range []string{"candidate", "branch", "missing-branch", "bad-config", "changed-base", "changed-policy", "resolve-failure"} {
		t.Run(kind, func(t *testing.T) {
			f, i, r := newConvertFixture(t)
			git := func(args ...string) {
				t.Helper()
				out, err := gitfixture.Command(t, "", append([]string{"-C", f.root}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			path := filepath.Join(f.root, ".swarm", "config.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := harness.LoadConfig(f.root)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Security.ReadyLabel = "trusted:ready"
			writeCfg := func() {
				t.Helper()
				b, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "branch" {
				git("checkout", "-b", "policy")
				writeCfg()
				git("add", ".swarm/config.yaml")
				git("commit", "-m", "branch label policy")
				git("checkout", "main")
				cfg, err = harness.LoadConfig(f.root)
				if err != nil {
					t.Fatal(err)
				}
				cfg.BaseBranch = "policy"
				writeCfg()
				git("add", ".swarm/config.yaml")
				git("commit", "-m", "select policy branch")
			} else if kind == "missing-branch" {
				cfg.BaseBranch = "missing"
				writeCfg()
				git("add", ".swarm/config.yaml")
				git("commit", "-m", "missing branch")
			} else if kind == "bad-config" {
				if err := os.WriteFile(path, []byte("invalid: ["), 0600); err != nil {
					t.Fatal(err)
				}
				git("add", ".swarm/config.yaml")
				git("commit", "-m", "bad config")
			} else {
				writeCfg()
				git("add", ".swarm/config.yaml")
				git("commit", "-m", "trusted label")
			}
			if err := f.tracker.AddLabels(context.Background(), i.Number, []string{"trusted:ready", "candidate:ready"}); err != nil {
				t.Fatal(err)
			}
			// This uncommitted configuration must never choose the demotion label.
			if err := os.WriteFile(path, bytes.ReplaceAll(data, []byte("swarm:ready"), []byte("candidate:ready")), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "changed-base" || kind == "changed-policy" || kind == "resolve-failure" {
				r.onRead = func(read int, _ *tracker.Issue) error {
					if read == 2 {
						if kind == "resolve-failure" {
							git("update-ref", "-d", "refs/heads/main")
							return nil
						}
						if kind == "changed-policy" {
							cfg.Security.ReadyLabel = "changed:ready"
							writeCfg()
							git("add", ".swarm/config.yaml")
							git("commit", "-m", "changed policy")
						} else {
							git("commit", "--allow-empty", "-m", "new base")
						}
					}
					return nil
				}
			}
			out, err := runConvert(f, r, convertArgs(i.Number)...)
			success := kind == "candidate" || kind == "branch"
			if (err == nil) != success {
				t.Fatalf("%v %s", err, out)
			}
			if !success {
				if r.writes != 0 {
					t.Fatalf("writes %d", r.writes)
				}
				return
			}
			got, err := f.tracker.GetIssue(context.Background(), i.Number)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.Join(got.Labels, ","), "trusted:ready") || !strings.Contains(strings.Join(got.Labels, ","), "candidate:ready") {
				t.Fatal(got.Labels)
			}
		})
	}
}

func TestIssueConvertApprovalDefaults(t *testing.T) {
	for _, gate := range []contract.GateMode{contract.GateAgent, contract.GateHuman} {
		t.Run(string(gate), func(t *testing.T) {
			f, i, r := newConvertFixture(t)
			cfg, err := harness.LoadConfig(f.root)
			if err != nil {
				t.Fatal(err)
			}
			cfg.DefaultGate = gate
			data, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(f.root, ".swarm", "config.yaml"), data, 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", ".swarm/config.yaml"}, {"commit", "-m", "stricter approval gate"}} {
				if out, err := gitfixture.Command(t, "", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			out, err := runConvert(f, r, convertArgs(i.Number)...)
			if err != nil {
				t.Fatalf("%v %s", err, out)
			}
			// Conversion must not create any approval or other authoritative state.
			if st, err := readIssueEditState(context.Background(), f.manager, i.Number); err != nil || st != nil {
				t.Fatalf("conversion created state: %+v %v", st, err)
			}
			out, err = f.run("issue", "lint", fmt.Sprint(i.Number))
			if err != nil {
				t.Fatalf("lint: %v %s", err, out)
			}
			out, err = f.run("issue", "approve", fmt.Sprint(i.Number))
			if err != nil {
				t.Fatalf("approval: %v %s", err, out)
			}
			st, err := f.manager.GetCurrentState(context.Background(), i.Number)
			if err != nil || st.EffectiveGate != gate {
				t.Fatalf("%+v %v", st, err)
			}
		})
	}
}

func TestIssueConvertWarningText(t *testing.T) {
	f, i, r := newConvertFixture(t)
	r.projectFail = true
	out, err := runConvert(f, r, append(convertArgs(i.Number), "--depends-on", "2,2,1")...)
	if err != nil || !strings.Contains(out, "converted-with-warnings") || !strings.Contains(out, "retry") || !reflect.DeepEqual(r.links, []int{2}) {
		t.Fatalf("%v %s links=%v", err, out, r.links)
	}
}
