package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	prservice "github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workflow"
)

func encoded(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func digest(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }

var firstHead = strings.Repeat("a", 40)
var secondHead = strings.Repeat("b", 40)

// Fake service responses marshal the production emitting types. No test invokes
// a subprocess, including tests exercising the complete run entry point.
type fixture struct {
	t              *testing.T
	o              options
	id             string
	calls          [][]string
	revision       bool
	submitted      bool
	statusNumber   int
	approvalChange func(*lease.IssueState)
	intercept      func(int, string, []string, []byte) ([]byte, error)
}

func newFixture(t *testing.T, gate string) *fixture {
	t.Helper()
	return &fixture{t: t, o: options{Upstream: "maintainer/fixture", Fork: "actor/fixture", UpstreamRoot: t.TempDir(), ForkRoot: t.TempDir(), Gate: gate}}
}
func (f *fixture) args() []string {
	return []string{"--upstream", f.o.Upstream, "--fork", f.o.Fork, "--upstream-root", f.o.UpstreamRoot, "--fork-root", f.o.ForkRoot, "--gate", f.o.Gate}
}
func (f *fixture) issue() *tracker.Issue {
	return &tracker.Issue{Number: 7, Title: "Public fork regression " + f.id, Body: "### Objective\nCreate a unique fixture file.\n### Constraints\nChange only public-fork-regression/" + f.id + ".txt\n### Acceptance Criteria\n- [ ] Fixture exists\n### Workflow\nfeature\n### Plan Gate\n" + f.o.Gate, State: "open", HTMLURL: "https://github.com/" + f.o.Upstream + "/issues/7"}
}
func (f *fixture) state() *lease.IssueState {
	i := f.issue()
	wu, e := contract.ParseWorkUnitFromIssue(7, i.Title, i.Body)
	if e != nil {
		f.t.Fatal(e)
	}
	st := &lease.IssueState{TaskDigest: contract.DigestWorkUnit(wu), BaseSHA: firstHead, PolicyDigest: "policy", EffectiveGate: contract.GateMode(f.o.Gate), HeadSHA: firstHead, ForkRepo: f.o.Fork}
	if f.submitted {
		st.PRNumber = 9
	}
	if f.revision {
		st.HeadSHA = secondHead
		st.PRRevision = &lease.PRRevision{Number: 9, RemoteHeadSHA: firstHead, LocalHeadSHA: secondHead, Branch: "fixture"}
	}
	plan, test, review := "plan", "tests", "VERDICT: APPROVED\n"
	kind := f.o.Gate
	st.Artifacts = []contract.Artifact{{Type: contract.ArtifactDesign, Body: plan}, {Type: contract.ArtifactTestPlan, Body: test}, {Type: contract.ArtifactReview, Body: review, Verdict: "APPROVED", Metadata: map[string]string{"actor": "maintainer", "kind": kind, "review_source": "authenticated-" + kind, "task_digest": st.TaskDigest, "base_sha": st.BaseSHA, "policy_digest": st.PolicyDigest, "content_sha256": digest(review), "plan_digest": digest(plan), "test_plan_digest": digest(test)}}}
	if e := lease.VerifyApproval(st, wu); e != nil {
		f.t.Fatalf("invalid approval fixture: %v", e)
	}
	if f.approvalChange != nil {
		f.approvalChange(st)
	}
	return st
}
func (f *fixture) pull(head string) *tracker.PullRequest {
	return &tracker.PullRequest{Number: 9, Draft: true, State: "open", HTMLURL: "https://github.com/" + f.o.Upstream + "/pull/9", HeadRepo: f.o.Fork, BaseRepo: f.o.Upstream, HeadSHA: head, Head: "fixture", Base: "main"}
}
func (f *fixture) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.t.Helper()
	f.calls = append(f.calls, append([]string{name}, args...))
	b := f.response(name, args)
	if f.intercept != nil {
		return f.intercept(len(f.calls), name, args, b)
	}
	return b, nil
}
func (f *fixture) response(name string, args []string) []byte {
	f.t.Helper()
	if name == "gh" {
		h := slices.Index(args, "--hostname")
		if h < 0 || args[h+1] != "github.com" {
			f.t.Fatal("public GitHub API host must be explicit")
		}
		switch args[1] {
		case "user":
			return encoded(map[string]any{"login": "actor"})
		case "repos/" + f.o.Upstream:
			return encoded(repository{FullName: f.o.Upstream, Push: true, Owner: struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			}{strings.Split(f.o.Upstream, "/")[0], "User"}})
		case "repos/" + f.o.Fork:
			return encoded(repository{FullName: f.o.Fork, Fork: true, Parent: f.o.Upstream, Owner: struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			}{"actor", "User"}})
		case "repos/" + f.o.Upstream + "/pulls/9":
			return encoded(map[string]any{"number": 9, "html_url": f.pull(firstHead).HTMLURL, "draft": true, "head": firstHead, "head_repo": f.o.Fork, "base_repo": f.o.Upstream})
		}
	}
	if name == "git" {
		if args[2] == "rev-parse" {
			return []byte(args[1] + "\n")
		}
		slug := f.o.Upstream
		if args[1] == f.o.ForkRoot && args[len(args)-1] == "origin" {
			slug = f.o.Fork
		}
		return []byte("git@github.com:" + slug + ".git\n")
	}
	if name != "swarm" {
		f.t.Fatalf("unexpected executable %s", name)
	}
	// Assert real root flags, correct checkout and explicit contributor mode.
	pos := slices.Index(args, "issue")
	if pos < 0 {
		pos = slices.Index(args, "pr")
	}
	if pos < 0 {
		f.t.Fatal("missing resource")
	}
	a := args[pos:]
	contributor := a[0] == "pr" || a[1] == "claim" || a[1] == "work" || a[1] == "submit"
	root, remote := f.o.UpstreamRoot, "origin"
	if contributor {
		root, remote = f.o.ForkRoot, "upstream"
	}
	want := []string{"--backend", "github", "--repo", f.o.Upstream, "--root", root, "--upstream-remote", remote}
	if contributor {
		want = append(want, "--fork", "--fork-remote", "origin")
	}
	if !reflect.DeepEqual(args[:pos], want) {
		f.t.Fatalf("root argv: %v want %v", args[:pos], want)
	}
	if slices.Contains(a, "validate") || slices.Contains(a, "--approve") || slices.Contains(a, "--draft") || slices.Contains(a, "--expected-head") {
		f.t.Fatalf("unregistered/forbidden operation: %v", a)
	}
	if a[len(a)-1] != "--json" {
		f.t.Fatal("JSON required")
	}
	if a[1] != "create" && a[1] != "work" {
		n := "7"
		if a[1] == "status" && f.statusNumber > 0 {
			n = strconv.Itoa(f.statusNumber)
		}
		if a[0] == "pr" {
			n = "9"
		}
		expected := []string{a[0], a[1], n, "--json"}
		if !reflect.DeepEqual(a, expected) {
			f.t.Fatalf("operation argv %v want %v", a, expected)
		}
	}
	switch a[1] {
	case "create":
		wantFlags := []string{"issue", "create", "--title"}
		if !reflect.DeepEqual(a[:3], wantFlags) {
			f.t.Fatal(a)
		}
		f.id = strings.TrimPrefix(a[3], "Public fork regression ")
		if !idRE.MatchString(f.id) {
			f.t.Fatal("run ID")
		}
		path := "public-fork-regression/" + f.id + ".txt"
		expected := []string{"issue", "create", "--title", "Public fork regression " + f.id, "--objective", "Create only " + path + " containing run ID " + f.id + " and revision one.", "--constraint", "Change only " + path, "--acceptance", path + " contains run ID " + f.id + ".", "--workflow", "feature", "--gate", f.o.Gate, "--json"}
		if !reflect.DeepEqual(a, expected) {
			f.t.Fatalf("create argv %v", a)
		}
		return encoded(map[string]any{"kind": "issue", "status": "draft", "issue": f.issue(), "allow_harness_changes": false})
	case "approve":
		return encoded(map[string]any{"kind": "task-approval", "issue_number": 7, "state": f.state()})
	case "claim":
		return encoded(map[string]any{"kind": "claim", "issue_number": 7, "state": f.state(), "branch": "fixture", "worktree": "/secret/path"})
	case "status":
		if a[0] == "issue" {
			return encoded(map[string]any{"kind": "status", "issues": []statusEntry{{Repository: f.o.Upstream, Issue: f.issue(), State: f.state()}}})
		}
		return encoded(prEnvelope{Kind: "pr-status", Status: "pending", Result: &prservice.Result{IssueNumber: 7, Status: "pending", HeadSHA: secondHead, PR: f.pull(firstHead)}})
	case "review-plan":
		return encoded(workEnvelope{Kind: "work", Status: "completed", Result: &workflow.WorkResult{IssueNumber: 7, PhaseCompleted: "review"}})
	case "work":
		phase := a[slices.Index(a, "--phase")+1]
		head := firstHead
		if a[0] == "pr" {
			f.revision = true
			head = secondHead
			expected := []string{"pr", "work", "9", "--phase", phase, "--instructions", "Update only public-fork-regression/" + f.id + ".txt to contain run ID " + f.id + " and revision two.", "--json"}
			if !reflect.DeepEqual(a, expected) {
				f.t.Fatal(a)
			}
		} else {
			if !reflect.DeepEqual(a, []string{"issue", "work", "7", "--phase", phase, "--json"}) {
				f.t.Fatal(a)
			}
		}
		w := &workflow.WorkResult{IssueNumber: 7, PhaseCompleted: phase, ValidationPassed: phase == "implement", HeadSHA: head}
		if a[0] == "pr" {
			return encoded(prEnvelope{Kind: "pr-work", Status: "pending", Result: &prservice.Result{IssueNumber: 7, Status: "pending", HeadSHA: head, PR: f.pull(firstHead), Work: w}})
		}
		return encoded(workEnvelope{Kind: "work", Status: "completed", Result: w})
	case "submit":
		if a[0] == "pr" {
			return encoded(prEnvelope{Kind: "pr-submit", Status: "submitted", Result: &prservice.Result{IssueNumber: 7, Status: "submitted", HeadSHA: secondHead, PR: f.pull(secondHead)}})
		}
		f.submitted = true
		return encoded(map[string]any{"kind": "submission", "status": "submitted", "issue_number": 7, "pr_number": 9, "pr_url": f.pull(firstHead).HTMLURL, "head_sha": firstHead})
	}
	f.t.Fatalf("unexpected argv %v %v", name, args)
	return nil
}
func operations(calls [][]string) []string {
	var out []string
	for _, c := range calls {
		if c[0] != "swarm" {
			continue
		}
		i := slices.Index(c, "issue")
		if i < 0 {
			i = slices.Index(c, "pr")
		}
		s := c[i] + " " + c[i+1]
		if p := slices.Index(c, "--phase"); p >= 0 {
			s += " " + c[p+1]
		}
		out = append(out, s)
	}
	return out
}
func TestLifecycleSequence(t *testing.T) {
	f := newFixture(t, "agent")
	var out bytes.Buffer
	if e := run(context.Background(), f.args(), f.command, &out); e != nil {
		t.Fatal(e)
	}
	want := []string{"issue create", "issue approve", "issue claim", "issue work plan", "issue review-plan", "issue status", "issue work implement", "issue status", "issue submit", "pr work plan", "issue review-plan", "issue status", "pr work implement", "pr status", "pr submit"}
	if got := operations(f.calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(out.String(), "completed:") {
		t.Fatal(out.String())
	}
}
func TestSameAccountLifecycle(t *testing.T) {
	f := newFixture(t, "agent")
	f.o.Upstream = "actor/upstream"
	if e := run(context.Background(), f.args(), f.command, &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
}
func TestDryRunNoMutations(t *testing.T) {
	f := newFixture(t, "human")
	for _, resume := range []string{"", filepath.Join(f.o.ForkRoot, "missing")} {
		args := append(f.args(), "--dry-run")
		if resume != "" {
			args = append(args, "--resume", resume)
		}
		var out bytes.Buffer
		if e := run(context.Background(), args, func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("dry run subprocess")
			return nil, nil
		}, &out); e != nil {
			t.Fatal(e)
		}
		for _, s := range []string{"issue create", "issue claim", "issue work", "issue submit", "pr work", "pr status", "pr submit", "pending live preflight"} {
			if !strings.Contains(out.String(), s) {
				t.Fatal(s)
			}
		}
		entries, _ := os.ReadDir(f.o.ForkRoot)
		if len(entries) != 0 {
			t.Fatal("dry run wrote files")
		}
	}
}
func TestPreflightRefusals(t *testing.T) {
	for _, slug := range []string{"", "supernovae/swarmos", "SuPeRnOvAe/SwArMoS", "bad", "x/y/z", "a/b\n", "actor/fixture"} {
		t.Run("argument/"+slug, func(t *testing.T) {
			f := newFixture(t, "agent")
			a := f.args()
			a[1] = slug
			if e := run(context.Background(), a, f.command, &bytes.Buffer{}); e == nil {
				t.Fatal("accepted")
			}
			if len(f.calls) != 0 {
				t.Fatal("subprocess before argument refusal")
			}
		})
	}
	for _, field := range []string{"private upstream", "private fork", "org fork", "parent", "owner", "permissions", "canonical production", "missing private", "api failure", "remote", "missing root", "mismatch root", "malformed"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t, "agent")
			if field == "missing root" {
				f.o.ForkRoot = filepath.Join(f.o.ForkRoot, "missing")
			}
			f.intercept = func(_ int, name string, args []string, b []byte) ([]byte, error) {
				if name == "swarm" {
					t.Fatal("mutation after refusal")
				}
				if name == "gh" && strings.HasPrefix(args[1], "repos/") {
					if field == "api failure" {
						return nil, errors.New("token-secret")
					}
					if field == "malformed" {
						return []byte("{"), nil
					}
					var m map[string]any
					_ = json.Unmarshal(b, &m)
					isFork := args[1] == "repos/"+f.o.Fork
					switch {
					case field == "private upstream" && !isFork, field == "private fork" && isFork:
						m["private"] = true
					case field == "org fork" && isFork:
						m["owner"].(map[string]any)["type"] = "Organization"
					case field == "parent" && isFork:
						m["parent"] = "other/repo"
					case field == "owner" && isFork:
						m["owner"].(map[string]any)["login"] = "someone"
					case field == "permissions" && !isFork:
						m["push"] = false
					case field == "canonical production":
						m["full_name"] = "Supernovae/Swarmos"
					case field == "missing private":
						delete(m, "private")
					}
					return encoded(m), nil
				}
				if name == "git" && field == "remote" && args[2] == "remote" {
					return []byte("https://github.com/SUPERNOVAE/SWARMOS.git"), nil
				}
				if name == "git" && field == "mismatch root" && args[2] == "rev-parse" {
					return []byte("/different"), nil
				}
				return b, nil
			}
			if e := run(context.Background(), f.args(), f.command, &bytes.Buffer{}); e == nil {
				t.Fatal("refusal missing")
			}
		})
	}
}
func pauseRecord(t *testing.T, f *fixture, stage string) string {
	t.Helper()
	matches, e := filepath.Glob(filepath.Join(f.o.ForkRoot, ".swarm-public-fork-*-"+stage, "resume.json"))
	if e != nil || len(matches) != 1 {
		t.Fatalf("record: %v %v", matches, e)
	}
	return matches[0]
}
func TestHumanGatePauseResume(t *testing.T) {
	f := newFixture(t, "human")
	ctx := context.Background()
	if e := run(ctx, f.args(), f.command, &bytes.Buffer{}); !errors.Is(e, errPaused) {
		t.Fatal(e)
	}
	p := pauseRecord(t, f, "issue")
	f.calls = nil
	for _, change := range []string{"missing", "rejected", "superseded", "stale", "agent", "gate", "advisory"} {
		t.Run(change, func(t *testing.T) {
			f.approvalChange = approvalMutation(change)
			e := run(ctx, append(f.args(), "--resume", p), f.command, &bytes.Buffer{})
			if e == nil {
				t.Fatal("invalid approval accepted")
			}
			for _, op := range operations(f.calls) {
				if strings.Contains(op, "implement") || strings.Contains(op, "submit") {
					t.Fatal("advanced without approval")
				}
			}
			f.calls = nil
		})
	}
	f.approvalChange = nil
	if e := run(ctx, append(f.args(), "--resume", p), f.command, &bytes.Buffer{}); !errors.Is(e, errPaused) {
		t.Fatal(e)
	}
	if got := operations(f.calls); !reflect.DeepEqual(got, []string{"issue status", "issue work implement", "issue status", "issue submit", "pr work plan"}) {
		t.Fatal(got)
	}
	p = pauseRecord(t, f, "revision")
	f.calls = nil
	for _, change := range []string{"missing", "rejected", "superseded", "stale", "agent", "gate", "advisory"} {
		f.approvalChange = approvalMutation(change)
		if e := run(ctx, append(f.args(), "--resume", p), f.command, &bytes.Buffer{}); e == nil {
			t.Fatal("revision approval accepted", change)
		}
		if ops := operations(f.calls); !reflect.DeepEqual(ops, []string{"issue status"}) {
			t.Fatal("revision advanced", ops)
		}
		f.calls = nil
	}
	f.approvalChange = nil
	if e := run(ctx, append(f.args(), "--resume", p), f.command, &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
	if got := operations(f.calls); !reflect.DeepEqual(got, []string{"issue status", "pr work implement", "pr status", "pr submit"}) {
		t.Fatal(got)
	}
}
func approvalMutation(kind string) func(*lease.IssueState) {
	return func(st *lease.IssueState) {
		a := &st.Artifacts[2]
		switch kind {
		case "missing":
			st.Artifacts = st.Artifacts[:2]
		case "rejected":
			a.Verdict = "CHANGES_REQUESTED"
		case "superseded":
			a.Metadata["superseded_by"] = "later"
		case "stale":
			a.Metadata["plan_digest"] = "stale"
		case "agent":
			a.Metadata["kind"] = "agent"
			a.Metadata["review_source"] = "authenticated-agent"
		case "gate":
			st.EffectiveGate = contract.GateNone
		case "advisory":
			a.Metadata["review_source"] = "advisory"
		}
	}
}
func TestAgentGateUsesTrustedReview(t *testing.T) {
	for _, approved := range []bool{true, false} {
		t.Run(strconv.FormatBool(approved), func(t *testing.T) {
			f := newFixture(t, "agent")
			if !approved {
				f.approvalChange = approvalMutation("advisory")
			}
			e := run(context.Background(), f.args(), f.command, &bytes.Buffer{})
			if approved && e != nil {
				t.Fatal(e)
			}
			if !approved && !errors.Is(e, errPaused) {
				t.Fatal(e)
			}
			ops := operations(f.calls)
			if !slices.Contains(ops, "issue review-plan") {
				t.Fatal(ops)
			}
			if !approved && slices.Contains(ops, "issue work implement") {
				t.Fatal("advisory permitted implement")
			}
		})
	}
}
func TestFailureStopsSequence(t *testing.T) {
	baseline := newFixture(t, "agent")
	if e := run(context.Background(), baseline.args(), baseline.command, &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
	for step := 1; step <= len(baseline.calls); step++ {
		for _, mode := range []string{"exit", "malformed", "incomplete"} {
			t.Run(fmt.Sprintf("%d/%s", step, mode), func(t *testing.T) {
				f := newFixture(t, "agent")
				f.intercept = func(n int, name string, args []string, b []byte) ([]byte, error) {
					if n != step {
						return b, nil
					}
					switch mode {
					case "exit":
						return nil, errors.New("private secret error")
					case "malformed":
						return []byte("{bad"), nil
					default:
						return []byte("{}"), nil
					}
				}
				if e := run(context.Background(), f.args(), f.command, &bytes.Buffer{}); e == nil {
					t.Fatal("failure accepted")
				}
				if len(f.calls) != step {
					t.Fatalf("later call %d > %d", len(f.calls), step)
				}
			})
		}
	}
}
func TestImplementStops(t *testing.T) {
	for _, resource := range []string{"issue", "pr"} {
		for _, fault := range []string{"false validation", "missing validation", "empty head", "missing head", "wrong head", "wrong issue", "wrong phase", "paused", "handed-off", "budget", "split", "unknown status", "missing status", "missing result", "null work"} {
			t.Run(resource+"/"+fault, func(t *testing.T) {
				f := newFixture(t, "agent")
				hit := false
				f.intercept = func(_ int, name string, args []string, b []byte) ([]byte, error) {
					i := slices.Index(args, resource)
					if name != "swarm" || i < 0 || args[i+1] != "work" || !slices.Contains(args, "implement") {
						return b, nil
					}
					hit = true
					var m map[string]any
					_ = json.Unmarshal(b, &m)
					r := m["result"].(map[string]any)
					w := r
					if resource == "pr" {
						w = r["work"].(map[string]any)
					}
					switch fault {
					case "false validation":
						w["ValidationPassed"] = false
					case "missing validation":
						delete(w, "ValidationPassed")
					case "empty head":
						w["HeadSHA"] = ""
					case "missing head":
						delete(w, "HeadSHA")
					case "wrong head":
						w["HeadSHA"] = "invalid"
					case "wrong issue":
						w["IssueNumber"] = 8
					case "wrong phase":
						w["PhaseCompleted"] = "plan"
					case "paused":
						w["PausedForHuman"] = true
					case "handed-off":
						w["HandedOffToPhase"] = "implement"
					case "budget":
						w["claim_budget_pause"] = map[string]any{}
					case "split":
						w["split_recommended"] = true
					case "unknown status":
						m["status"] = "new-status"
					case "missing status":
						delete(m, "status")
					case "missing result":
						delete(m, "result")
					case "null work":
						if resource == "pr" {
							r["work"] = nil
						} else {
							m["result"] = nil
						}
					}
					return encoded(m), nil
				}
				if e := run(context.Background(), f.args(), f.command, &bytes.Buffer{}); e == nil {
					t.Fatal("incomplete work accepted")
				}
				if !hit {
					t.Fatal("not reached")
				}
				ops := operations(f.calls)
				last := ops[len(ops)-1]
				if last != resource+" work implement" {
					t.Fatal(ops)
				}
			})
		}
	}
}
func TestStructuredOutputContract(t *testing.T) {
	w := workEnvelope{Kind: "work", Status: "completed", Result: &workflow.WorkResult{IssueNumber: 7, PhaseCompleted: "implement", HeadSHA: firstHead, ValidationPassed: true}}
	if e := exact(encoded(w), &workEnvelope{}); e != nil {
		t.Fatal(e)
	}
	for _, fault := range []string{"trailing", "unknown", "case", "missing", "wrong type", "null boolean"} {
		t.Run(fault, func(t *testing.T) {
			var m map[string]any
			_ = json.Unmarshal(encoded(w), &m)
			r := m["result"].(map[string]any)
			switch fault {
			case "unknown":
				r["plan_approved"] = true
			case "case":
				r["validationpassed"] = r["ValidationPassed"]
				delete(r, "ValidationPassed")
			case "missing":
				delete(r, "PausedForHuman")
			case "wrong type":
				r["HeadSHA"] = 123
			case "null boolean":
				r["ValidationPassed"] = nil
			}
			b := encoded(m)
			if fault == "trailing" {
				b = append(b, []byte(" {}")...)
			}
			if exact(b, &workEnvelope{}) == nil {
				t.Fatal("accepted")
			}
		})
	}
	// Human review has a distinct wrapper and never authorizes resume.
	human := []byte(`{"kind":"review","issue_number":7,"reviewer_kind":"human","verdict":"APPROVED"}`)
	if exact(human, &workEnvelope{}) == nil {
		t.Fatal("human wrapper accepted as work")
	}
	if exact(encoded(w), &prEnvelope{}) == nil {
		t.Fatal("issue wrapper accepted as PR")
	}
}
func TestHeadAndApprovalChanges(t *testing.T) {
	for _, fault := range []string{"issue head", "revision head", "nested head", "submit head", "PR identity", "approval changed", "remote head"} {
		t.Run(fault, func(t *testing.T) {
			f := newFixture(t, "agent")
			statuses := 0
			hit := false
			f.intercept = func(_ int, name string, args []string, b []byte) ([]byte, error) {
				if name != "swarm" {
					return b, nil
				}
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				if slices.Contains(args, "status") && slices.Contains(args, "issue") {
					statuses++
					if statuses == 2 && (fault == "issue head" || fault == "approval changed") {
						hit = true
						e := m["issues"].([]any)[0].(map[string]any)
						st := e["state"].(map[string]any)
						if fault == "issue head" {
							st["head_sha"] = secondHead
						} else {
							st["task_digest"] = "stale"
						}
					}
				}
				if fault == "revision head" && slices.Contains(args, "pr") && slices.Contains(args, "status") {
					hit = true
					m["result"].(map[string]any)["head_sha"] = firstHead
				}
				if fault == "nested head" && slices.Contains(args, "pr") && slices.Contains(args, "implement") {
					hit = true
					m["result"].(map[string]any)["head_sha"] = firstHead
				}
				if fault == "submit head" && slices.Contains(args, "issue") && slices.Contains(args, "submit") {
					hit = true
					m["head_sha"] = secondHead
				}
				if fault == "remote head" && slices.Contains(args, "pr") && slices.Contains(args, "status") {
					hit = true
					m["result"].(map[string]any)["pr"].(map[string]any)["head_sha"] = secondHead
				}
				if fault == "PR identity" && slices.Contains(args, "pr") && slices.Contains(args, "submit") {
					hit = true
					m["result"].(map[string]any)["pr"].(map[string]any)["number"] = 10
				}
				return encoded(m), nil
			}
			if e := run(context.Background(), f.args(), f.command, &bytes.Buffer{}); e == nil {
				t.Fatal("accepted")
			}
			if !hit {
				t.Fatal("not reached")
			}
		})
	}
}
func TestResumeRecordRefusals(t *testing.T) {
	for _, fault := range []string{"symlink", "directory", "world-readable", "foreign actor", "repository", "issue", "PR", "approval field", "directory link"} {
		t.Run(fault, func(t *testing.T) {
			f := newFixture(t, "human")
			if e := run(context.Background(), f.args(), f.command, &bytes.Buffer{}); !errors.Is(e, errPaused) {
				t.Fatal(e)
			}
			p := pauseRecord(t, f, "issue")
			b, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			switch fault {
			case "symlink":
				if e = os.Rename(p, p+".real"); e != nil {
					t.Fatal(e)
				}
				e = os.Symlink(p+".real", p)
			case "directory":
				_ = os.Remove(p)
				e = os.Mkdir(p, 0700)
			case "directory link":
				dir := filepath.Dir(p)
				e = os.Rename(dir, dir+".real")
				if e == nil {
					e = os.Symlink(dir+".real", dir)
				}
			case "world-readable":
				e = os.Chmod(p, 0644)
			default:
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				switch fault {
				case "foreign actor":
					m["Actor"] = "foreign"
				case "repository":
					m["Options"].(map[string]any)["Fork"] = "other/repo"
				case "issue":
					m["Issue"] = 8
					f.statusNumber = 8
				case "PR":
					m["PR"] = 9
				case "approval field":
					m["approved"] = true
				}
				e = os.WriteFile(p, encoded(m), 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			f.calls = nil
			if e = run(context.Background(), append(f.args(), "--resume", p), f.command, &bytes.Buffer{}); e == nil {
				t.Fatal("record accepted")
			}
			for _, op := range operations(f.calls) {
				if op != "issue status" {
					t.Fatal("mutation from record", op)
				}
			}
		})
	}
}
func TestSafeSummary(t *testing.T) {
	f := newFixture(t, "agent")
	var out bytes.Buffer
	f.intercept = func(_ int, name string, args []string, b []byte) ([]byte, error) {
		if name == "swarm" && slices.Contains(args, "work") {
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if w, ok := m["result"].(map[string]any); ok && m["kind"] == "work" {
				w["WorktreePath"] = "/local/secret ghp_SECRET\x1b[31m"
				w["ReviewVerdict"] = "token-secret"
			}
			return encoded(m), nil
		}
		return b, nil
	}
	if e := run(context.Background(), f.args(), f.command, &out); e != nil {
		t.Fatal(e)
	}
	for _, secret := range []string{"SECRET", "token-secret", "/local/", f.o.UpstreamRoot, f.o.ForkRoot, "\x1b"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("summary leaked %q", secret)
		}
	}
}
func TestOptionsHelp(t *testing.T) {
	var out bytes.Buffer
	if e := run(context.Background(), []string{"--help"}, nil, &out); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "--resume") {
		t.Fatal("help")
	}
}

func TestDuplicateJSONKeys(t *testing.T) {
	for _, b := range []string{`{"kind":"work","kind":"work"}`, `{"kind":"work","status":"completed","result":{"ValidationPassed":false,"ValidationPassed":true}}`} {
		if exact([]byte(b), &workEnvelope{}) == nil {
			t.Fatal("duplicate keys accepted")
		}
	}
}
func TestKnownReviewPause(t *testing.T) {
	f := newFixture(t, "agent")
	f.intercept = func(_ int, name string, args []string, b []byte) ([]byte, error) {
		if name == "swarm" && slices.Contains(args, "review-plan") {
			return encoded(workEnvelope{Kind: "work", Status: "paused", Result: &workflow.WorkResult{IssueNumber: 7, PhaseCompleted: "plan", PausedForHuman: true}}), nil
		}
		return b, nil
	}
	if e := run(context.Background(), f.args(), f.command, &bytes.Buffer{}); !errors.Is(e, errPaused) {
		t.Fatal(e)
	}
	p := pauseRecord(t, f, "issue")
	f.calls = nil
	if e := run(context.Background(), append(f.args(), "--resume", p), f.command, &bytes.Buffer{}); !errors.Is(e, errPaused) {
		t.Fatal(e)
	} // revision review pauses again
	if got := operations(f.calls); !reflect.DeepEqual(got, []string{"issue status", "issue work implement", "issue status", "issue submit", "pr work plan", "issue review-plan"}) {
		t.Fatal(got)
	}
}
func TestRecordExclusive(t *testing.T) {
	f := newFixture(t, "human")
	d := driver{o: f.o, out: &bytes.Buffer{}}
	r := record{Options: f.o, Actor: "actor", ID: strings.Repeat("a", 32), Stage: "issue", Issue: 7}
	if e := d.saveRecord(r); e != nil {
		t.Fatal(e)
	}
	p := pauseRecord(t, f, "issue")
	before, _ := os.ReadFile(p)
	if e := d.saveRecord(r); e == nil {
		t.Fatal("record overwritten")
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("record changed")
	}
	info, e := os.Stat(p)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private record required")
	}
}
func TestGateStatusDecodeAndIdentity(t *testing.T) {
	for _, fault := range []string{"null state", "null issue", "wrong issue", "wrong repo", "missing gate", "unknown key", "parse error", "missing issues", "external contract"} {
		t.Run(fault, func(t *testing.T) {
			f := newFixture(t, "agent")
			hit := false
			f.intercept = func(_ int, name string, args []string, b []byte) ([]byte, error) {
				if name != "swarm" || !slices.Contains(args, "issue") || !slices.Contains(args, "status") {
					return b, nil
				}
				hit = true
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				entry := m["issues"].([]any)[0].(map[string]any)
				switch fault {
				case "null state":
					entry["state"] = nil
				case "null issue":
					entry["issue"] = nil
				case "wrong issue":
					entry["issue"].(map[string]any)["number"] = 8
				case "wrong repo":
					entry["repository"] = "other/repo"
				case "missing gate":
					delete(entry["state"].(map[string]any), "effective_gate")
				case "unknown key":
					entry["approved"] = true
				case "parse error":
					entry["issue"].(map[string]any)["body"] = "not a contract"
				case "missing issues":
					delete(m, "issues")
				case "external contract":
					entry["issue"].(map[string]any)["body"] = "swarm-workunit-path: secret.yaml"
				}
				return encoded(m), nil
			}
			if e := run(context.Background(), f.args(), f.command, &bytes.Buffer{}); e == nil {
				t.Fatal("status accepted")
			}
			if !hit {
				t.Fatal("status not reached")
			}
			if got := operations(f.calls); got[len(got)-1] != "issue status" {
				t.Fatal("advanced after invalid status", got)
			}
		})
	}
}
func TestStructuredEmitterFixtures(t *testing.T) {
	f := newFixture(t, "agent")
	f.id = strings.Repeat("c", 32)
	// Cover fresh submission's additional fields with their real emitting types.
	b := encoded(map[string]any{"kind": "submission", "status": "submitted", "issue_number": 7, "pr_number": 9, "pr_url": f.pull(firstHead).HTMLURL, "head_sha": firstHead, "base_freshness": submissionFreshness{Status: "current"}, "report": &validation.ValidationReport{Passed: true, HeadSHA: firstHead}})
	calls := 0
	d := driver{o: f.o, out: &bytes.Buffer{}, call: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls++
		if calls == 2 {
			return b, nil
		}
		return f.command(ctx, name, args...)
	}}
	r := record{Options: f.o, Actor: "actor", ID: f.id, Issue: 7, Stage: "issue"}
	if e := d.submit(context.Background(), &r, false, firstHead); e != nil {
		t.Fatal(e)
	}
}
