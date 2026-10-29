package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
	"gopkg.in/yaml.v3"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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
