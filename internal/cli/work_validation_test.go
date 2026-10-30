package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"gopkg.in/yaml.v3"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// workValidationPolicy fails one of two trusted checks when the developer
// leaves its marker: a real Go test followed by noise past the tail limit, or
// a shell command that prints terminal controls.
const workValidationPolicy = `required_checks:
  - name: go-check
    command: |
      if test -f fail-go; then (cd gofail && go test -count=1 ./...); status=$?; seq 70; exit $status; fi
    timeout: 2m
  - name: shell-check
    command: |
      if test -f fail-shell; then printf 'shell failure \033[31mred\033[0m\r\b\177\000end\n'; exit 7; fi
    timeout: 1m
protected_paths: ['.swarm/**', '.github/**']
`

func setupWorkValidationPolicy(t *testing.T, f *cliFixture) {
	t.Helper()
	for name, contents := range map[string]string{
		".swarm/validation/required-checks.yaml": workValidationPolicy,
		"gofail/go.mod":                          "module example.com/gofail\n\ngo 1.21\n",
		"gofail/fail_test.go":                    "package gofail\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestWorkFailure(t *testing.T) {\n\tif _, err := os.Stat(\"../fail-go\"); err == nil {\n\t\tt.Fatal(\"work failure message\")\n\t}\n}\n",
	} {
		path := filepath.Join(f.root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prGit(t, f.root, "add", ".")
	prGit(t, f.root, "commit", "-m", "conditional failing checks")
}

func TestWorkValidationDiagnostics(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"issue", "pr"} {
		for _, check := range []string{"go", "shell"} {
			for _, asJSON := range []bool{false, true} {
				name := family + "/" + check + "/text"
				if asJSON {
					name = family + "/" + check + "/json"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					var f *cliFixture
					args := []string{family, "work"}
					if family == "issue" {
						f = newCLIFixture(t)
						setupWorkValidationPolicy(t, f)
						n := strconv.Itoa(f.create(t, "none"))
						f.must(t, "issue", "approve", n)
						args = append(args, n)
					} else {
						var prNumber int
						f, _, prNumber = submittedPRFixture(t, "none", func(f *cliFixture) { setupWorkValidationPolicy(t, f) })
						args = append(args, strconv.Itoa(prNumber))
					}
					f.setenv("SWARM_MOCK_DEVELOPER_CMD", "echo fail > fail-"+check)
					args = append(args, "--agent", "mock")
					if asJSON {
						// Progress stays enabled: it must not reach stdout.
						args = append(args, "--json")
					} else {
						args = append(args, "--progress=false")
					}
					out, _, err := runProgressCommand(t, f, args...)
					if err == nil || ExitCode(err) == ExitOK || !strings.Contains(err.Error(), "required validation check failed") {
						t.Fatalf("work error = %v\n%s", err, out)
					}
					// JSON keeps DEL literal inside strings, and the legacy
					// output field and artifact body stay unescaped, as in
					// standalone validation; output_tail is checked decoded.
					controls := "\x1b\r\b\x7f\x00"
					if asJSON {
						controls = "\x1b\r\b\x00"
					}
					if strings.ContainsAny(out, controls) {
						t.Fatalf("raw control characters on stdout: %q", out)
					}
					if asJSON {
						assertWorkValidationJSON(t, family, check, out)
					} else {
						assertWorkValidationText(t, check, out)
					}
				})
			}
		}
	}
}

const (
	goFailBlock    = "--- FAIL: TestWorkFailure ("
	goFailMessage  = "fail_test.go:10: work failure message"
	shellFailTail  = `shell failure \x1b[31mred\x1b[0m\r\b\x7f\x00end`
	truncatedNoise = "[swarm: output truncated]"
)

func assertWorkValidationText(t *testing.T, check, out string) {
	t.Helper()
	out = regexp.MustCompile(`(\(exit [0-9]+); [^)]+\)`).ReplaceAllString(out, "$1)")
	if strings.Contains(out, "PASS ") {
		t.Fatalf("work diagnostics repeated passing checks: %s", out)
	}
	switch check {
	case "go":
		assertProgressOrder(t, out, "FAIL go-check (exit 1)\nfailing tests: TestWorkFailure\n", goFailBlock, goFailMessage, truncatedNoise, "\n70\n")
	case "shell":
		assertProgressOrder(t, out, "FAIL shell-check (exit 7)\n"+shellFailTail+"\n")
		if strings.Contains(out, "failing tests:") {
			t.Fatalf("shell check reported failing tests: %s", out)
		}
	}
}

func assertWorkValidationJSON(t *testing.T, family, check, out string) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(out))
	var doc struct {
		Kind   string          `json:"kind"`
		Status string          `json:"status"`
		Error  string          `json:"error"`
		Report json.RawMessage `json:"report"`
		Result struct {
			Work struct {
				Validation json.RawMessage `json:"validation"`
			} `json:"work"`
		} `json:"result"`
	}
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("extra JSON after document: %v\n%s", err, out)
	}
	raw, kind := doc.Report, "work"
	if family == "pr" {
		raw, kind = doc.Result.Work.Validation, "pr-work"
	}
	if doc.Kind != kind || doc.Status != "failed" || !strings.Contains(doc.Error, "required validation check failed") {
		t.Fatalf("failure envelope = %+v", doc)
	}
	var report struct {
		Passed        bool                         `json:"passed"`
		CheckOutcomes []map[string]json.RawMessage `json:"check_outcomes"`
	}
	if err := json.Unmarshal(raw, &report); err != nil || report.Passed {
		t.Fatalf("report %s: %v", raw, err)
	}
	failed := map[string]json.RawMessage{}
	for _, outcome := range report.CheckOutcomes {
		_, hasTail := outcome["output_tail"]
		_, hasTests := outcome["failing_tests"]
		if string(outcome["passed"]) == "true" {
			if hasTail || hasTests {
				t.Fatalf("passing outcome carries diagnostics: %v", outcome)
			}
			continue
		}
		if !hasTail || !hasTests {
			t.Fatalf("failed outcome lacks diagnostics: %v", outcome)
		}
		failed = outcome
	}
	var name, tail string
	var tests []string
	for key, target := range map[string]any{"name": &name, "output_tail": &tail, "failing_tests": &tests} {
		if err := json.Unmarshal(failed[key], target); err != nil {
			t.Fatalf("outcome %s: %v", key, err)
		}
	}
	if strings.ContainsAny(tail, "\x1b\r\b\x7f\x00") {
		t.Fatalf("decoded output_tail revives controls: %q", tail)
	}
	switch check {
	case "go":
		if name != "go-check" || len(tests) != 1 || tests[0] != "TestWorkFailure" {
			t.Fatalf("go outcome = %s %v", name, tests)
		}
		assertProgressOrder(t, tail, goFailBlock, goFailMessage, truncatedNoise, "\n70")
	case "shell":
		if name != "shell-check" || tests == nil || len(tests) != 0 || tail != shellFailTail {
			t.Fatalf("shell outcome = %s %q %v", name, tail, tests)
		}
	}
}

func TestWorkValidationRepairNextAction(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"issue", "pr"} {
		for _, tc := range []struct {
			name, developer, reason string
		}{
			{name: "exhausted", developer: "date +%s%N >> fail-shell", reason: ReasonValidationRepairExhausted},
			{name: "no changes", developer: "echo fail > fail-shell", reason: ReasonValidationRepairNoChanges},
			{name: "repaired", developer: "if test -f fail-shell; then rm fail-shell; else echo fail > fail-shell; fi"},
		} {
			t.Run(family+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				var f *cliFixture
				var number string
				if family == "issue" {
					f = newCLIFixture(t)
					setupWorkValidationPolicy(t, f)
					number = strconv.Itoa(f.create(t, "none"))
					f.must(t, "issue", "approve", number)
				} else {
					var prNumber int
					f, _, prNumber = submittedPRFixture(t, "none", func(f *cliFixture) { setupWorkValidationPolicy(t, f) })
					number = strconv.Itoa(prNumber)
				}
				f.setenv("SWARM_MOCK_DEVELOPER_CMD", tc.developer)
				doc, err := f.runJSON(t, family, "work", number, "--agent", "mock", "--progress=false", "--json")
				if tc.reason == "" {
					assertNoNextAction(t, doc, err)
					raw, _ := json.Marshal(doc)
					if !strings.Contains(string(raw), `"repair_rounds":1`) || !strings.Contains(string(raw), `"ValidationPassed":true`) {
						t.Fatalf("repaired run: %s", raw)
					}
					return
				}
				assertNextAction(t, doc, err, tc.reason, false, ExitChangesRequested, localArgv(f, family, "work", number, "--phase", "implement")...)
				if !strings.Contains(err.Error(), "required validation check failed") {
					t.Fatalf("stop lost the validation failure: %v", err)
				}
			})
		}
	}
}

func TestTestPhaseRequiresExplicitIssueBeforeSelection(t *testing.T) {
	for _, flags := range [][]string{{"--next"}, {"--next", "--all"}} {
		// No backend, repository, tracker, manager, or runner is available: any
		// selection or setup call would fail instead of producing this usage error.
		opts := &RootOptions{RepoRoot: t.TempDir(), Backend: "invalid"}
		cmd := newWorkCmd(opts)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"--phase", "test"}, flags...))
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "requires an explicit issue number") {
			t.Fatalf("flags %v: %v", flags, err)
		}
		entries, err := os.ReadDir(opts.RepoRoot)
		if err != nil || len(entries) != 0 {
			t.Fatalf("selection mutated root: %v %v", entries, err)
		}
	}
}

func TestExplicitTestWorkCommandRunsTesterOnly(t *testing.T) {
	for _, resource := range []string{"issue", "pr"} {
		t.Run(resource, func(t *testing.T) {
			var f *cliFixture
			var number int
			if resource == "issue" {
				f = newCLIFixture(t)
				number = f.create(t, "none")
				f.must(t, "issue", "approve", strconv.Itoa(number))
			} else {
				f, _, number = submittedPRFixture(t, "none")
			}
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "echo implemented > implementation.txt")
			f.must(t, resource, "work", strconv.Itoa(number), "--agent", "mock")
			// A developer invocation in the retry would fail the command.
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "exit 77")
			f.setenv("SWARM_MOCK_TESTER_CMD", "echo tester-ran > \"$TMPDIR/tester-retry\"")
			out, err := f.run(resource, "work", strconv.Itoa(number), "--phase", "test", "--agent", "mock", "--json")
			if err != nil {
				t.Fatalf("retry: %v %s", err, out)
			}
			var payload struct {
				Result struct {
					PhaseCompleted   string
					ValidationPassed bool
					RoleAgentsUsed   map[string]string
				}
			}
			if resource == "issue" {
				if err := json.Unmarshal([]byte(out), &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Result.PhaseCompleted != "test" || !payload.Result.ValidationPassed || payload.Result.RoleAgentsUsed["developer"] != "" {
					t.Fatalf("result: %s", out)
				}
			} else if !strings.Contains(out, "\"PhaseCompleted\":\"test\"") {
				t.Fatalf("PR result: %s", out)
			}
		})
	}
}

func TestExplicitTestWorkFakeGitHub(t *testing.T) {
	for _, resource := range []string{"issue", "pr"} {
		t.Run(resource, func(t *testing.T) {
			setup := func(f *cliFixture) {
				path := filepath.Join(f.root, ".swarm/config.yaml")
				cfg, err := harness.LoadConfig(f.root)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Agents["mock"] = contract.AgentConfig{Binary: "mock"}
				data, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0644); err != nil {
					t.Fatal(err)
				}
				prGit(t, f.root, "add", ".")
				prGit(t, f.root, "commit", "-m", "allow fixture mock")
			}
			var f *cliFixture
			var issue, number int
			if resource == "issue" {
				f = newCLIFixture(t)
				setup(f)
				issue = f.create(t, "none")
				number = issue
				f.must(t, "issue", "approve", strconv.Itoa(issue))
				f.must(t, "issue", "work", strconv.Itoa(issue), "--agent", "mock")
			} else {
				f, issue, number = submittedPRFixture(t, "none", setup)
				f.setenv("SWARM_MOCK_DEVELOPER_CMD", "echo revision > revision.txt")
				f.must(t, "pr", "work", strconv.Itoa(number), "--agent", "mock")
			}
			remote := filepath.Join(t.TempDir(), "remote.git")
			prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
			prGit(t, f.root, "remote", "add", "origin", remote)
			prGit(t, f.root, "push", "origin", "main")
			prGit(t, f.root, "push", "origin", f.state(t, issue).Branch)
			raw, _, err := f.manager.Store().Load(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := trustedLedgerStore(t, f.root, "").CompareAndSwap(t.Context(), issue, "", raw); err != nil {
				t.Fatal(err)
			}
			pull := tracker.PullRequest{}
			if resource == "pr" {
				original, err := f.tracker.GetPullRequest(t.Context(), number)
				if err != nil {
					t.Fatal(err)
				}
				pull = *original
				mergeable := true
				pull.Mergeable = &mergeable
				pull.MergeableState = "clean"
			}
			opts := &RootOptions{RepoRoot: f.root, Backend: "github", trackerOverride: workCLITracker{Tracker: f.tracker, pr: pull}}
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "exit 77")
			doc, err := runJSON(t, f.options(opts), resource, "work", strconv.Itoa(number), "--phase", "test", "--agent", "mock", "--json", "--progress=false")
			if err != nil {
				t.Fatalf("fake github retry: %v %+v", err, doc)
			}
			result := doc["result"].(map[string]any)
			if resource == "pr" {
				result = result["work"].(map[string]any)
			}
			if result["PhaseCompleted"] != "test" || result["ValidationPassed"] != true {
				t.Fatalf("retry result: %+v", result)
			}
		})
	}
}

// untouchedSelectionTracker counts tracker activity and refuses it; any other
// method panics through the nil interface.
type untouchedSelectionTracker struct {
	tracker.Tracker
	calls int
}

func (t *untouchedSelectionTracker) refuse() error {
	t.calls++
	return errors.New("unexpected tracker call")
}
func (t *untouchedSelectionTracker) BackendName() string {
	t.calls++
	return "github"
}
func (t *untouchedSelectionTracker) CurrentUser(context.Context) (string, error) {
	return "", t.refuse()
}
func (t *untouchedSelectionTracker) Authorize(context.Context, string) error { return t.refuse() }
func (t *untouchedSelectionTracker) GetIssue(context.Context, int) (*tracker.Issue, error) {
	return nil, t.refuse()
}
func (t *untouchedSelectionTracker) ListIssues(context.Context, tracker.IssueFilter) ([]*tracker.Issue, error) {
	return nil, t.refuse()
}

// countingRunner records every agent launch and refuses it.
type countingRunner struct {
	agent.MockRunner
	runs *int
}

func (r *countingRunner) Run(context.Context, agent.RunRequest) (*agent.RunResult, error) {
	*r.runs++
	return nil, errors.New("unexpected agent run")
}

// rootSnapshot digests every file under root, including Git refs, coordination
// state, claim tokens and worktrees.
func rootSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() {
				files[path] = "dir"
			}
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[path] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestTestPhaseNextRefusesBeforeAnyCoordinatorActivity(t *testing.T) {
	for _, flags := range [][]string{{"--next"}, {"--next", "--all"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			f := newCLIFixture(t)
			// A ready approved task that --next would otherwise select and claim.
			f.must(t, "issue", "approve", strconv.Itoa(f.create(t, "none")))
			runs := 0
			f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &countingRunner{runs: &runs}, nil
			}
			run := func(phase string) (*untouchedSelectionTracker, error) {
				tr := &untouchedSelectionTracker{}
				cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "github", Repo: "owner/repo", trackerOverride: tr})
				cmd.SetOut(io.Discard)
				cmd.SetErr(io.Discard)
				cmd.SetArgs(append([]string{"issue", "work", "--phase", phase, "--agent", "mock", "--progress=false"}, flags...))
				return tr, cmd.ExecuteContext(t.Context())
			}
			before := rootSnapshot(t, f.root)
			tr, err := run("test")
			if err == nil || !strings.Contains(err.Error(), "requires an explicit issue number") {
				t.Fatalf("refusal: %v", err)
			}
			if tr.calls != 0 || runs != 0 {
				t.Fatalf("activity before refusal: tracker=%d agents=%d", tr.calls, runs)
			}
			if after := rootSnapshot(t, f.root); !reflect.DeepEqual(before, after) {
				t.Fatal("refusal mutated state, tokens or worktrees")
			}
			if len(flags) == 1 {
				// Control: the same seams observe selection for a permitted phase.
				if tr, _ = run("implement"); tr.calls == 0 {
					t.Fatal("tracker seam did not observe selection")
				}
			}
		})
	}
}

func TestStagedImplementationIsNotTestRetryEligible(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	wt := filepath.Join(f.root, ".swarm", "worktrees", "issue-"+n)
	for name, body := range map[string]string{".swarm/active/PLAN.md": "# Plan\n\n1. Add feature.txt\n", ".swarm/active/TEST_PLAN.md": "# Test Plan\n\n- Run git --version\n"} {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.must(t, "issue", "stage", n, "--phase", "plan", "--harness", "claude", "--model", "test")
	f.must(t, "issue", "review-plan", n, "--agent", "mock")
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("shipped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.must(t, "issue", "stage", n, "--phase", "implement", "--harness", "codex", "--model", "test")
	before := f.state(t, number)
	staged := false
	for _, a := range before.Artifacts {
		staged = staged || (a.Type == contract.ArtifactImplementation && a.Verdict == "PASS" && a.Metadata["implementation_trusted_origin"] == "")
	}
	if !staged {
		t.Fatalf("no manual stage PASS: %+v", before.Artifacts)
	}
	runs := 0
	f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &countingRunner{runs: &runs}, nil
	}
	out, err := f.run("issue", "work", n, "--phase", "test", "--agent", "mock", "--progress=false")
	if err == nil || !strings.Contains(err.Error(), "tester retry unavailable") || runs != 0 {
		t.Fatalf("staged retry: %v runs=%d\n%s", err, runs, out)
	}
	after := f.state(t, number)
	for _, typ := range []contract.ArtifactType{contract.ArtifactImplementation, contract.ArtifactValidation} {
		var was, now []contract.Artifact
		for _, a := range before.Artifacts {
			if a.Type == typ {
				was = append(was, a)
			}
		}
		for _, a := range after.Artifacts {
			if a.Type == typ {
				now = append(now, a)
			}
		}
		if !reflect.DeepEqual(was, now) {
			t.Fatalf("%s evidence changed", typ)
		}
	}
}

// continuationRunner exercises the command's actual engine and coordinator;
// only role execution is scripted, including one actionable independent review.
type continuationRunner struct {
	agent.MockRunner
	counts   map[string]int
	feedback []string
	reject   bool
	bare     bool
}

func (r *continuationRunner) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	r.counts[req.Role]++
	if req.Role == "reviewer" {
		verdict := "APPROVED"
		if r.reject {
			verdict = "CHANGES_REQUESTED"
			r.reject = false
		}
		result, err := (&agent.MockRunner{ReviewVerdict: verdict}).Run(ctx, req)
		if err == nil && verdict == "CHANGES_REQUESTED" {
			name := ".swarm/active/REVIEW.md"
			if req.ReadOnly {
				name = ".swarm/active-output/REVIEW.md"
			}
			body := "VERDICT: CHANGES_REQUESTED\n\nInclude the missing recovery case."
			if r.bare {
				body = "VERDICT: CHANGES_REQUESTED"
			}
			err = os.WriteFile(filepath.Join(req.WorktreePath, name), []byte(body), 0644)
		}
		return result, err
	}
	result, err := r.MockRunner.Run(ctx, req)
	if req.Role == "planner" {
		r.feedback = append(r.feedback, req.TaskPrompt)
		if (strings.Contains(req.TaskPrompt, "Include the missing recovery case.") || req.OperatorInstructions != "") && err == nil {
			name := filepath.Join(req.WorktreePath, ".swarm/active/PLAN.md")
			body, readErr := os.ReadFile(name)
			if readErr != nil {
				return nil, readErr
			}
			err = os.WriteFile(name, append(body, []byte("\nCover the missing recovery case.\n")...), 0644)
		}
	}
	return result, err
}
func TestIssueWorkContinuationCommand(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		for _, resume := range []bool{false, true} {
			t.Run(backend+"/resume="+strconv.FormatBool(resume), func(t *testing.T) {
				f := newCLIFixture(t)
				cfg, err := harness.LoadConfig(f.root)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Agents["mock"] = contract.AgentConfig{Binary: "mock"}
				raw, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(f.root, ".swarm/config.yaml"), raw, 0644); err != nil {
					t.Fatal(err)
				}
				prGit(t, f.root, "add", ".")
				prGit(t, f.root, "commit", "-m", "allow mock")
				n := f.create(t, "agent")
				number := strconv.Itoa(n)
				var tr tracker.Tracker = f.tracker
				if backend == "github" {
					issue, err := f.tracker.GetIssue(t.Context(), n)
					if err != nil {
						t.Fatal(err)
					}
					_, tr = newFakeGitHubTracker(t, issue)
					addTestOrigin(t, f)
					f.manager = lease.NewManager(tr, cfg.Security)
					if err = f.manager.ConfigureStore(f.root); err != nil {
						t.Fatal(err)
					}
				}
				scripted := &continuationRunner{counts: map[string]int{}, reject: true}
				f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) { return scripted, nil }
				run := func(args ...string) (string, error) {
					cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: backend, Repo: "octo/swarm", trackerOverride: tr})
					var out bytes.Buffer
					cmd.SetOut(&out)
					cmd.SetErr(&out)
					cmd.SetArgs(args)
					err := cmd.ExecuteContext(t.Context())
					return out.String(), err
				}
				if out, err := run("issue", "approve", number); err != nil {
					t.Fatalf("approve: %v %s", err, out)
				}
				if resume {
					if out, err := run("issue", "work", number, "--phase", "plan", "--agent", "mock", "--progress=false"); err != nil {
						t.Fatalf("plan: %v %s", err, out)
					}
					if _, err := run("issue", "work", number, "--phase", "review", "--agent", "mock", "--progress=false"); err == nil {
						t.Fatal("explicit review continued rejection")
					}
					if scripted.counts["planner"] != 1 || scripted.counts["reviewer"] != 1 || scripted.counts["developer"] != 0 {
						t.Fatal(scripted.counts)
					}
					scripted.counts = map[string]int{}
				}
				out, err := run("issue", "work", number, "--agent", "mock", "--progress=false")
				if err != nil {
					t.Fatalf("work: %v %s", err, out)
				}
				want := map[string]int{"planner": 2, "reviewer": 2, "developer": 1, "tester": 1}
				if resume {
					want["planner"], want["reviewer"] = 1, 1
				}
				if !reflect.DeepEqual(scripted.counts, want) {
					t.Fatalf("counts=%v want %v", scripted.counts, want)
				}
				if !strings.Contains(scripted.feedback[len(scripted.feedback)-1], "Include the missing recovery case.") {
					t.Fatal("findings not delivered")
				}
				st, err := f.manager.GetCurrentState(t.Context(), n)
				if err != nil {
					t.Fatal(err)
				}
				if st.PlanReplanCount != 1 || !lease.CompletePlanPair(st) || st.PRNumber != 0 || st.HeadSHA == "" {
					t.Fatalf("state: %+v", st)
				}
				var passed bool
				for _, a := range st.Artifacts {
					if a.Type == contract.ArtifactValidation {
						passed = a.Verdict == "PASS" && a.Metadata["validation_trusted_origin"] == "coordinator"
					}
					if a.Type == contract.ArtifactReview {
						for k, v := range lease.ReviewBindings(st) {
							if k != "review_claim_id" && a.Metadata[k] != v {
								t.Fatalf("stale review %s", k)
							}
						}
					}
				}
				if !passed {
					t.Fatal("no trusted validated checkpoint")
				}
			})
		}
	}
}

type continuationActor struct {
	tracker.Tracker
	actor *string
}

func (t continuationActor) CurrentUser(context.Context) (string, error) { return *t.actor, nil }

func TestIssueWorkRecoveryMissingReports(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		for _, gate := range []string{"agent", "human"} {
			for _, limit := range []int{0, 1, 2} {
				t.Run(backend+"/"+gate+"/limit="+strconv.Itoa(limit), func(t *testing.T) {
					f := newCLIFixture(t)
					cfg, err := harness.LoadConfig(f.root)
					if err != nil {
						t.Fatal(err)
					}
					cfg.PlanMode = "agent"
					cfg.Agents["mock"] = contract.AgentConfig{Binary: "mock"}
					data, err := yaml.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					// Use the real optional policy field, including an explicit zero.
					var policy map[string]any
					if err = yaml.Unmarshal(data, &policy); err != nil {
						t.Fatal(err)
					}
					policy["lease"].(map[string]any)["plan_replan_limit"] = limit
					data, err = yaml.Marshal(policy)
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(filepath.Join(f.root, ".swarm/config.yaml"), data, 0644); err != nil {
						t.Fatal(err)
					}
					prGit(t, f.root, "add", ".")
					prGit(t, f.root, "commit", "-m", "agent planning budget")
					n := f.create(t, gate)
					number := strconv.Itoa(n)
					var tr tracker.Tracker = f.tracker
					if backend == "github" {
						issue, err := tr.GetIssue(t.Context(), n)
						if err != nil {
							t.Fatal(err)
						}
						_, tr = newFakeGitHubTracker(t, issue)
						addTestOrigin(t, f)
					}
					actor := "alice"
					tr = continuationActor{Tracker: tr, actor: &actor}
					f.manager = lease.NewManager(tr, cfg.Security)
					if err = f.manager.ConfigureStore(f.root); err != nil {
						t.Fatal(err)
					}
					scripted := &continuationRunner{counts: map[string]int{}}
					f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) { return scripted, nil }
					run := func(args ...string) (string, error) {
						cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: backend, Repo: "octo/swarm", trackerOverride: tr})
						var out bytes.Buffer
						cmd.SetOut(&out)
						cmd.SetErr(&out)
						cmd.SetArgs(args)
						err := cmd.ExecuteContext(t.Context())
						return out.String(), err
					}
					must := func(args ...string) string {
						t.Helper()
						out, err := run(args...)
						if err != nil {
							t.Fatalf("%v: %v %s", args, err, out)
						}
						return out
					}
					state := func() *lease.IssueState {
						t.Helper()
						st, err := f.manager.GetCurrentState(t.Context(), n)
						if err != nil {
							t.Fatal(err)
						}
						return st
					}
					must("issue", "approve", number)
					must("issue", "work", number, "--phase", "plan", "--agent", "mock", "--progress=false")
					actor = "independent-maintainer"
					must("issue", "review-plan", number, "--reject", "--notes", "Include the missing recovery case.")
					actor = "alice"
					for i := 0; i < limit; i++ {
						_, _ = run("issue", "work", number, "--phase", "plan", "--agent", "mock", "--progress=false")
					}
					rejected := state()
					if rejected.HumanPlanReview == nil || rejected.HumanDraftCount != limit {
						t.Fatalf("draft evidence: %+v", rejected)
					}
					identity := *rejected.HumanPlanReview
					must("issue", "release", number)
					prGit(t, f.root, "commit", "--allow-empty", "-m", "moved approved base")
					if backend == "github" {
						prGit(t, f.root, "push", "origin", "main")
					}
					must("issue", "approve", number)
					moved := state()
					if len(moved.Artifacts) != 0 || moved.HumanPlanReview == nil || *moved.HumanPlanReview != identity || moved.HumanDraftCount != limit {
						t.Fatalf("carry removed obligation: %+v", moved)
					}
					scripted.counts = map[string]int{}
					for attempt := 0; attempt < 2; attempt++ {
						out, err := run("issue", "work", number, "--agent", "mock", "--progress=false")
						if !errors.Is(err, lease.ErrPlanReplanLimit) || !strings.Contains(err.Error(), "identical WorkUnit cannot recover here") {
							t.Fatalf("stop: %v %s", err, out)
						}
						action := nextActionOf(err)
						if action == nil || !strings.Contains(strings.Join(action.Argv, " "), "issue edit "+number) {
							t.Fatalf("recovery: %+v", action)
						}
						if len(scripted.counts) != 0 {
							t.Fatal("roles after exhaustion", scripted.counts)
						}
					}
					actor = "independent-maintainer"
					before := state()
					for _, verdict := range []string{"--approve", "--reject"} {
						if out, err := run("issue", "review-plan", number, verdict, "--notes", "Revise the plan"); !errors.Is(err, lease.ErrApprovalRequired) {
							t.Fatalf("reportless verdict: %v %s", err, out)
						}
					}
					if !reflect.DeepEqual(before, state()) {
						t.Fatal("reportless review changed authority")
					}
					actor = "alice"
					if _, err := run("issue", "claim", number, "--agent", "mock"); !errors.Is(err, lease.ErrPlanReplanLimit) {
						t.Fatalf("capped claim: %v", err)
					}
					if _, err := run("issue", "stage", number, "--phase", "plan"); err == nil {
						t.Fatal("agent mode staging admitted")
					}
					if _, err := run("issue", "work", number, "--agent", "mock", "--progress=false"); !errors.Is(err, lease.ErrPlanReplanLimit) {
						t.Fatal("reclaim replenished budget", err)
					}
					if state().Lease.Status == "active" {
						must("issue", "release", number)
					}
					oldTask := state().TaskDigest
					actor = "independent-maintainer"
					must("issue", "edit", number, "--objective", "Implement the revised bounded recovery objective")
					must("issue", "approve", number)
					changed := state()
					if changed.TaskDigest == oldTask || changed.HumanPlanReview != nil || changed.PlanGeneration != 0 || changed.HumanDraftCount != 0 {
						t.Fatalf("changed task retained obsolete evidence: %+v", changed)
					}
					actor = "alice"
					scripted.counts = map[string]int{}
					must("issue", "work", number, "--agent", "mock", "--progress=false")
					want := map[string]int{"planner": 1, "reviewer": 1, "developer": 1, "tester": 1}
					if gate == "human" {
						want = map[string]int{"planner": 1}
					}
					if !reflect.DeepEqual(scripted.counts, want) {
						t.Fatalf("new task counts %v want %v", scripted.counts, want)
					}
					if gate == "human" {
						actor = "independent-maintainer"
						must("issue", "review-plan", number, "--approve")
						actor = "alice"
						scripted.counts = map[string]int{}
						must("issue", "work", number, "--agent", "mock", "--progress=false")
						if !reflect.DeepEqual(scripted.counts, map[string]int{"developer": 1, "tester": 1}) {
							t.Fatal(scripted.counts)
						}
					}
					if state().PRNumber != 0 {
						t.Fatal("work published")
					}
				})
			}
		}
	}
}

func TestIssueWorkRecoveryFeedbackSelection(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		for _, next := range []bool{false, true} {
			for _, flag := range []string{"--planner-instructions", "--instructions", "--developer-instructions"} {
				t.Run(backend+"/next="+strconv.FormatBool(next)+"/"+flag, func(t *testing.T) {
					f := newCLIFixture(t)
					cfg, err := harness.LoadConfig(f.root)
					if err != nil {
						t.Fatal(err)
					}
					cfg.Agents["mock"] = contract.AgentConfig{Binary: "mock"}
					data, err := yaml.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(filepath.Join(f.root, ".swarm/config.yaml"), data, 0644); err != nil {
						t.Fatal(err)
					}
					prGit(t, f.root, "add", ".")
					prGit(t, f.root, "commit", "-m", "allow mock")
					n := f.create(t, "agent")
					ready := f.create(t, "agent")
					number := strconv.Itoa(n)
					var tr tracker.Tracker = f.tracker
					if backend == "github" {
						a, _ := tr.GetIssue(t.Context(), n)
						b, _ := tr.GetIssue(t.Context(), ready)
						_, tr = newFakeGitHubTracker(t, a, b)
						addTestOrigin(t, f)
					}
					f.manager = lease.NewManager(tr, cfg.Security)
					if err = f.manager.ConfigureStore(f.root); err != nil {
						t.Fatal(err)
					}
					script := &continuationRunner{counts: map[string]int{}, reject: true, bare: true}
					f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) { return script, nil }
					run := func(args ...string) (string, error) {
						cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: backend, Repo: "octo/swarm", trackerOverride: tr})
						var out bytes.Buffer
						cmd.SetOut(&out)
						cmd.SetErr(&out)
						cmd.SetArgs(args)
						err := cmd.ExecuteContext(t.Context())
						return out.String(), err
					}
					must := func(args ...string) {
						t.Helper()
						if out, err := run(args...); err != nil {
							t.Fatalf("%v %v %s", args, err, out)
						}
					}
					must("issue", "approve", number)
					must("issue", "approve", strconv.Itoa(ready))
					must("issue", "work", number, "--phase", "plan", "--agent", "mock", "--progress=false")
					if _, err := run("issue", "work", number, "--phase", "review", "--agent", "mock", "--progress=false"); err == nil {
						t.Fatal("bare explicit rejection continued")
					}
					must("issue", "release", number)
					before, err := f.manager.GetCurrentState(t.Context(), n)
					if err != nil {
						t.Fatal(err)
					}
					script.counts = map[string]int{}
					args := []string{"issue", "work", number, "--agent", "mock", "--progress=false", flag, "operator fix"}
					if next {
						args[2] = "--next"
					}
					out, err := run(args...)
					eligible := flag != "--developer-instructions"
					if !eligible && !next {
						if !errors.Is(err, workflow.ErrPlanReviewRejected) || len(script.counts) != 0 {
							t.Fatalf("blocked: %v %s %v", err, out, script.counts)
						}
					} else {
						if err != nil {
							t.Fatalf("work: %v %s", err, out)
						}
						if !reflect.DeepEqual(script.counts, map[string]int{"planner": 1, "reviewer": 1, "developer": 1, "tester": 1}) {
							t.Fatal(script.counts)
						}
					}
					after, err := f.manager.GetCurrentState(t.Context(), n)
					if err != nil {
						t.Fatal(err)
					}
					if eligible {
						if after.PlanReplanCount != 1 || !lease.CompletePlanPair(after) {
							t.Fatal("instructed replan did not reserve/complete")
						}
						if !strings.Contains(script.feedback[len(script.feedback)-1], "Operator instructions: operator fix") {
							t.Fatal("planner lost instructions")
						}
					} else {
						if after.Lease.ClaimID != before.Lease.ClaimID || after.PlanReplanCount != 0 {
							t.Fatal("blocked issue claimed or reserved")
						}
						if next && !strings.Contains(out, "planner feedback") {
							t.Fatal("skip reason missing", out)
						}
					}
				})
			}
		}
	}
}
