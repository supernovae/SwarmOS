package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/validation"
)

func TestWorkValidationReportPropagation(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		runnerErr     error
		wantReport    bool
		wantPassed    bool
	}{
		{name: "passing checks", command: "git status --porcelain", wantReport: true, wantPassed: true},
		{name: "failing check", command: "echo work-check-failed; exit 3", wantReport: true},
		{name: "failure before validation", command: "git status --porcelain", runnerErr: errors.New("runner unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, e, tr := setupProjectPlanTestRepo(t)
			configureValidation(t, root)
			policy := "required_checks:\n  - name: work-check\n    command: " + tc.command + "\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n  - .swarm/contracts/**\n"
			if err := os.WriteFile(filepath.Join(root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("git", "-C", root, "commit", "-am", "test validation").CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			wu := &contract.WorkUnit{Objective: "Report checks", Constraints: []string{"Keep fencing"}, Acceptance: []string{"Carry the report"}, Workflow: "feature", Gate: contract.GateNone}
			issue, err := tr.CreateIssue(t.Context(), "Report", contract.FormatIssueMarkdown(wu), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = e.LeaseManager.ApproveTask(t.Context(), issue.Number, "maintainer"); err != nil {
				t.Fatal(err)
			}
			if tc.runnerErr != nil {
				e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
					return &budgetTestRunner{run: func(context.Context, agent.RunRequest) (*agent.RunResult, error) {
						return nil, tc.runnerErr
					}}, nil
				}
			}
			res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
			if res == nil {
				t.Fatalf("no result: %v", err)
			}
			if (err == nil) != tc.wantPassed {
				t.Fatalf("err = %v", err)
			}
			if !tc.wantReport {
				if res.Validation != nil {
					t.Fatalf("report before validation: %+v", res.Validation)
				}
				return
			}
			if res.Validation == nil || res.Validation.Passed != tc.wantPassed || res.ValidationPassed != tc.wantPassed {
				t.Fatalf("report = %+v, passed = %t", res.Validation, res.ValidationPassed)
			}
			if !tc.wantPassed {
				outcome := res.Validation.CheckOutcomes[len(res.Validation.CheckOutcomes)-1]
				if outcome.Passed || outcome.ExitCode != 3 || outcome.OutputTail != "work-check-failed" {
					t.Fatalf("failed outcome = %+v", outcome)
				}
			}
		})
	}
}

// installRepairGo puts a fake go first on PATH whose TestBroken always fails
// on the candidate and, unless basePasses, also at the approved base checkout.
func installRepairGo(t *testing.T, basePasses bool) {
	t.Helper()
	dir := t.TempDir()
	pass := "false"
	if basePasses {
		pass = "true"
	}
	script := "#!/bin/sh\ncase \"$PWD\" in */swarm-repair-base-*) base=true;; *) base=false;; esac\n" +
		"if $base && " + pass + "; then echo '--- PASS: TestBroken (0.00s)'; echo PASS; exit 0; fi\n" +
		"echo '--- FAIL: TestBroken (0.00s)'; echo '    broken_test.go:1: broken'; echo FAIL; printf 'FAIL\\texample.com/m/broken\\t0.01s\\n'; exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWorkRepairBriefAndPreExistingStop(t *testing.T) {
	for _, tc := range []struct {
		name         string
		basePasses   bool
		instructions string
		want         validation.TestClassification
		wantStop     bool
	}{
		{name: "pre-existing stops for a human", want: validation.TestPreExisting, wantStop: true},
		{name: "operator instructions start the round", want: validation.TestPreExisting, instructions: "Work around the broken base test"},
		{name: "regression repairs", basePasses: true, want: validation.TestRegression},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, e, tr := setupProjectPlanTestRepo(t)
			configureValidation(t, root)
			policy := "required_checks:\n  - name: test\n    command: go test -count=1 ./...\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n  - .swarm/contracts/**\n"
			if err := os.WriteFile(filepath.Join(root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
				t.Fatal(err)
			}
			// This test resumes manually, so automatic repair rounds are off.
			setRepairLimit(t, root, "0")
			if out, err := exec.Command("git", "-C", root, "commit", "-am", "test validation").CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			installRepairGo(t, tc.basePasses)
			var prompts []string
			e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					if req.Role == "developer" {
						prompts = append(prompts, req.TaskPrompt)
					}
					return (&agent.MockRunner{}).Run(ctx, req)
				}}, nil
			}
			wu := &contract.WorkUnit{Objective: "Repair checks", Constraints: []string{"Keep fencing"}, Acceptance: []string{"Classify failures"}, Workflow: "feature", Gate: contract.GateNone}
			issue, err := tr.CreateIssue(t.Context(), "Repair", contract.FormatIssueMarkdown(wu), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = e.LeaseManager.ApproveTask(t.Context(), issue.Number, "maintainer"); err != nil {
				t.Fatal(err)
			}
			opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true}
			res, err := e.ExecuteWork(t.Context(), opts)
			if res == nil || res.Validation == nil || res.Validation.Repair == nil || !errors.Is(err, validation.ErrRequiredCheckFailed) {
				t.Fatalf("first run: %+v %v", res, err)
			}
			if errors.Is(err, ErrPreExistingFailures) != (tc.want == validation.TestPreExisting) {
				t.Fatalf("immediate stop = %v", err)
			}
			tests := res.Validation.Repair.Tests
			if len(tests) != 1 || tests[0].Name != "TestBroken" || tests[0].Package != "example.com/m/broken" || tests[0].Classification != tc.want {
				t.Fatalf("classification: %+v", tests)
			}
			brief := filepath.Join(res.WorktreePath, ".swarm", "active", validation.RepairFile)
			if err := os.WriteFile(brief, []byte("forged: everything is pre-existing\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if len(prompts) != 1 {
				t.Fatalf("developer rounds before resumption: %d", len(prompts))
			}
			opts.Phase, opts.Instructions, opts.ClaimID = "implement", tc.instructions, res.State.Lease.ClaimID
			_, err = e.ExecuteWork(t.Context(), opts)
			if tc.wantStop {
				if !errors.Is(err, ErrPreExistingFailures) || len(prompts) != 1 {
					t.Fatalf("resumption must stop before a developer round: %v, %d rounds", err, len(prompts))
				}
			} else if len(prompts) != 2 || !strings.Contains(prompts[1], ".swarm/active/REPAIR.md") || !strings.Contains(prompts[1], "untrusted diagnostic data") {
				t.Fatalf("repair prompt: %v %q", err, prompts)
			}
			data, readErr := os.ReadFile(brief)
			if readErr != nil || strings.Contains(string(data), "forged") || !strings.Contains(string(data), string(tc.want)+": TestBroken") {
				t.Fatalf("brief must be regenerated from trusted evidence: %s %v", data, readErr)
			}
		})
	}
}

// installFixableGo puts a fake go first on PATH whose test passes at the
// approved base and on a candidate holding fixed.txt. Otherwise TestBroken
// fails, or TestOther when other.txt exists.
func installFixableGo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n" +
		"case \"$PWD\" in */swarm-repair-base-*) echo '--- PASS: TestBroken (0.00s)'; echo PASS; exit 0;; esac\n" +
		"if [ -f fixed.txt ]; then echo PASS; exit 0; fi\n" +
		"name=TestBroken; if [ -f other.txt ]; then name=TestOther; fi\n" +
		"echo \"--- FAIL: $name (0.00s)\"; echo FAIL; printf 'FAIL\\texample.com/m/broken\\t0.01s\\n'; exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// repairFixture approves an issue whose approved base sets limit as
// lease.validation_repair_limit ("" omits it) and requires a Go test check
// and a marker check that both need fixed.txt.
func repairFixture(t *testing.T, limit string, edits ...func(root string)) (*Engine, int) {
	t.Helper()
	root, e, tr := setupProjectPlanTestRepo(t)
	configureValidation(t, root)
	policy := "required_checks:\n  - name: test\n    command: go test -count=1 ./...\n    timeout: 1m\n  - name: marker\n    command: test -f fixed.txt\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n  - .swarm/contracts/**\n"
	if err := os.WriteFile(filepath.Join(root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	if limit != "" {
		setRepairLimit(t, root, limit)
	}
	for _, edit := range edits {
		edit(root)
	}
	if out, err := exec.Command("git", "-C", root, "commit", "-am", "test validation").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	wu := &contract.WorkUnit{Objective: "Repair checks", Constraints: []string{"Keep fencing"}, Acceptance: []string{"Repair failures"}, Workflow: "feature", Gate: contract.GateNone}
	issue, err := tr.CreateIssue(t.Context(), "Repair", contract.FormatIssueMarkdown(wu), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.LeaseManager.ApproveTask(t.Context(), issue.Number, "maintainer"); err != nil {
		t.Fatal(err)
	}
	return e, issue.Number
}

func setRepairLimit(t *testing.T, root, limit string) {
	t.Helper()
	path := filepath.Join(root, ".swarm", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), "max_renewals: 3", "max_renewals: 3\n  validation_repair_limit: "+limit, 1)
	if updated == string(data) {
		t.Fatal("config has no max_renewals line")
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repairDeveloper runs rounds[i] on the i-th developer call and records each
// prompt; other roles use the mock runner.
func repairDeveloper(e *Engine, rounds ...func(dir string) error) *[]string {
	var prompts []string
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			if req.Role != "developer" {
				return (&agent.MockRunner{}).Run(ctx, req)
			}
			prompts = append(prompts, req.TaskPrompt)
			if n := len(prompts); n <= len(rounds) {
				if err := rounds[n-1](req.WorktreePath); err != nil {
					return nil, err
				}
			}
			return &agent.RunResult{}, nil
		}}, nil
	}
	return &prompts
}

func writeFile(name, content string) func(string) error {
	return func(dir string) error {
		return os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(content), 0o644)
	}
}

func TestWorkValidationRepairRounds(t *testing.T) {
	feature := writeFile("feature.txt", "feature\n")
	fix := writeFile("fixed.txt", "fixed\n")
	notesOnly := writeFile(".swarm/active/IMPLEMENTATION.md", "# Notes\n\nTried again.\n")
	for _, tc := range []struct {
		name, limit  string
		rounds       []func(string) error
		wantErr      error
		wantRounds   int
		wantCalls    int
		wantContains string
	}{
		{name: "default limit repairs on round 1", rounds: []func(string) error{feature, fix}, wantRounds: 1, wantCalls: 2},
		{name: "explicit zero disables repair", limit: "0", rounds: []func(string) error{feature, fix}, wantErr: validation.ErrRequiredCheckFailed, wantCalls: 1},
		{name: "limit 2 succeeds on the second repair", limit: "2", rounds: []func(string) error{feature, writeFile("try.txt", "x\n"), fix}, wantRounds: 2, wantCalls: 3},
		{name: "exhausted with the same failing tests", rounds: []func(string) error{feature, writeFile("try.txt", "x\n")}, wantErr: ErrValidationRepairExhausted, wantRounds: 1, wantCalls: 2, wantContains: "the same tests still fail: check marker, example.com/m/broken.TestBroken"},
		{name: "changed failing tests do not reset the budget", rounds: []func(string) error{feature, writeFile("other.txt", "x\n")}, wantErr: ErrValidationRepairExhausted, wantRounds: 1, wantCalls: 2, wantContains: "failing tests changed since the previous round: check marker, example.com/m/broken.TestOther"},
		{name: "a round with only notes changes stops", rounds: []func(string) error{feature, notesOnly}, wantErr: ErrValidationRepairNoChanges, wantRounds: 1, wantCalls: 2},
		{name: "blocked developer stops", limit: "3", rounds: []func(string) error{feature, writeFile(".swarm/active/IMPLEMENTATION.md", "Status: blocked - needs a maintainer\n")}, wantErr: validation.ErrRequiredCheckFailed, wantRounds: 1, wantCalls: 2, wantContains: "developer blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := installFixableGo(t)
			e, issue := repairFixture(t, tc.limit)
			prompts := repairDeveloper(e, tc.rounds...)
			res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
			if res == nil {
				t.Fatalf("no result: %v", err)
			}
			if len(*prompts) != tc.wantCalls || res.RepairRounds != tc.wantRounds {
				t.Fatalf("developer calls = %d, repair rounds = %d: %v", len(*prompts), res.RepairRounds, err)
			}
			if tc.wantErr == nil {
				if err != nil || !res.ValidationPassed || res.Validation == nil || !res.Validation.Passed || res.PhaseCompleted != "implement" {
					t.Fatalf("repair did not pass: %v %+v", err, res.Validation)
				}
				// Every required check ran on the repaired head.
				for _, name := range []string{"test", "marker"} {
					found := false
					for _, outcome := range res.Validation.CheckOutcomes {
						found = found || outcome.Name == name && outcome.Passed
					}
					if !found {
						t.Fatalf("check %s did not pass on the repaired head: %+v", name, res.Validation.CheckOutcomes)
					}
				}
				if res.Validation.HeadSHA != res.HeadSHA || res.State.HeadSHA != res.HeadSHA {
					t.Fatalf("report head %s, result head %s, state head %s", res.Validation.HeadSHA, res.HeadSHA, res.State.HeadSHA)
				}
				subject, gitErr := exec.Command("git", "-C", res.WorktreePath, "log", "-1", "--format=%s").Output()
				if gitErr != nil || !strings.HasPrefix(string(subject), "Repair validation: ") {
					t.Fatalf("repair checkpoint subject %q %v", subject, gitErr)
				}
				if _, statErr := os.Stat(filepath.Join(res.WorktreePath, ".swarm", "active", validation.RepairFile)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("passing validation kept a stale repair brief: %v", statErr)
				}
				if !strings.Contains((*prompts)[1], ".swarm/active/REPAIR.md") || !strings.Contains((*prompts)[1], "repair round 1 of") {
					t.Fatalf("repair prompt: %q", (*prompts)[1])
				}
				return
			}
			if !errors.Is(err, tc.wantErr) || !errors.Is(err, validation.ErrRequiredCheckFailed) || res.ValidationPassed || res.Validation == nil || res.Validation.Passed {
				t.Fatalf("err = %v, report = %+v", err, res.Validation)
			}
			if tc.wantContains != "" && !strings.Contains(err.Error(), tc.wantContains) {
				t.Fatalf("error %q lacks %q", err, tc.wantContains)
			}
			if tc.wantErr == ErrValidationRepairNoChanges || tc.wantErr == ErrValidationRepairExhausted {
				if !strings.Contains(err.Error(), fmt.Sprintf("swarm issue work %d --phase implement", issue)) {
					t.Fatalf("error lacks the resume command: %v", err)
				}
			}
			if tc.wantErr == ErrValidationRepairNoChanges {
				// No validation follows a round without changes.
				data, _ := os.ReadFile(log)
				if runs := strings.Count(string(data), "-count=1 ./..."); runs != 1 {
					t.Fatalf("full test check ran %d times: %s", runs, data)
				}
			}
		})
	}
}

func TestWorkRepairStopsForPreExistingFailures(t *testing.T) {
	for _, instructions := range []string{"", "Work around the broken base test"} {
		t.Run(fmt.Sprintf("instructions=%t", instructions != ""), func(t *testing.T) {
			// Only the Go test fails, so every failure is pre-existing.
			e, issue := repairFixture(t, "2", func(root string) {
				policy := "required_checks:\n  - name: test\n    command: go test -count=1 ./...\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n  - .swarm/contracts/**\n"
				if err := os.WriteFile(filepath.Join(root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
					t.Fatal(err)
				}
			})
			installRepairGo(t, false)
			prompts := repairDeveloper(e, writeFile("feature.txt", "feature\n"), writeFile("fixed.txt", "fixed\n"))
			res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true, Instructions: instructions})
			if !errors.Is(err, ErrPreExistingFailures) || errors.Is(err, ErrValidationRepairExhausted) || res == nil || res.Validation == nil || !res.Validation.Repair.AllPreExisting {
				t.Fatalf("pre-existing failures: %v", err)
			}
			if len(*prompts) != 1 || res.RepairRounds != 0 {
				t.Fatalf("automatic repair ran for pre-existing failures: %d calls, %d rounds", len(*prompts), res.RepairRounds)
			}
		})
	}
}

func TestWorkRepairLimitComesFromApprovedBase(t *testing.T) {
	log := installFixableGo(t)
	e, issue := repairFixture(t, "0")
	// The candidate raises the limit; only the approved base's policy counts.
	raise := func(dir string) error {
		if err := writeFile("feature.txt", "feature\n")(dir); err != nil {
			return err
		}
		path := filepath.Join(dir, ".swarm", "config.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, []byte(strings.Replace(string(data), "validation_repair_limit: 0", "validation_repair_limit: 3", 1)), 0o644)
	}
	prompts := repairDeveloper(e, raise, writeFile("fixed.txt", "fixed\n"))
	res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
	if err == nil || errors.Is(err, ErrValidationRepairExhausted) || res == nil || len(*prompts) != 1 || res.RepairRounds != 0 {
		data, _ := os.ReadFile(log)
		t.Fatalf("repair used an unapproved limit: %v, %d calls (%s)", err, len(*prompts), data)
	}
}

func TestWorkRepairStaleWorkerFailsWithClaimFence(t *testing.T) {
	log := installFixableGo(t)
	e, issue := repairFixture(t, "2")
	var before, stale string
	testerCalls := 0
	prompts := 0
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			switch req.Role {
			case "tester":
				testerCalls++
			case "developer":
				prompts++
				if prompts == 1 {
					return &agent.RunResult{}, writeFile("feature.txt", "feature\n")(req.WorktreePath)
				}
				st, err := e.LeaseManager.GetCurrentState(ctx, issue)
				if err != nil {
					return nil, err
				}
				before, stale = st.HeadSHA, st.Lease.ClaimID
				// A successor claim replaces this worker's during the repair
				// round; the worker keeps its now stale token.
				if _, err := e.LeaseManager.Release(ctx, issue, "maintainer", "operator reclaimed"); err != nil {
					return nil, err
				}
				if _, err := e.LeaseManager.Acquire(ctx, issue, "maintainer", "mock", "implement", time.Hour, 0); err != nil {
					return nil, err
				}
				e.LeaseManager.BindClaim(issue, stale)
				return &agent.RunResult{}, writeFile("fixed.txt", "fixed\n")(req.WorktreePath)
			}
			return (&agent.MockRunner{}).Run(ctx, req)
		}}, nil
	}
	res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
	if !errors.Is(err, lease.ErrClaimFence) || errors.Is(err, validation.ErrRequiredCheckFailed) {
		t.Fatalf("stale repair worker: %v", err)
	}
	if prompts != 2 || testerCalls != 1 || res == nil || res.ValidationPassed {
		t.Fatalf("stale worker continued: %d developer, %d tester calls", prompts, testerCalls)
	}
	data, _ := os.ReadFile(log)
	if runs := strings.Count(string(data), "-count=1 ./..."); runs != 1 {
		t.Fatalf("validation ran after the fence: %d full runs", runs)
	}
	st, stateErr := e.LeaseManager.GetCurrentState(t.Context(), issue)
	if stateErr != nil || st.Lease.ClaimID == stale || st.HeadSHA != before {
		t.Fatalf("successor state changed: %+v %v", st, stateErr)
	}
}

func TestWorkRepairPausesNearClaimBudget(t *testing.T) {
	installFixableGo(t)
	e, issue := repairFixture(t, "", func(root string) {
		path := filepath.Join(root, ".swarm", "config.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(data), "max_renewals: 3", "max_renewals: 0", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	var offset atomic.Int64
	e.LeaseManager.SetNowFunc(func() time.Time { return time.Now().Add(time.Duration(offset.Load())) })
	prompts := 0
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			switch req.Role {
			case "developer":
				prompts++
				return &agent.RunResult{}, writeFile(fmt.Sprintf("round%d.txt", prompts), "x\n")(req.WorktreePath)
			case "tester":
				// The claim nears the end of its budget during validation.
				st, err := e.LeaseManager.GetCurrentState(ctx, issue)
				if err != nil {
					return nil, err
				}
				offset.Store(int64(time.Until(st.Lease.ExpiresAt.Add(-time.Hour))))
			}
			return (&agent.MockRunner{}).Run(ctx, req)
		}}, nil
	}
	res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
	if err != nil || res == nil || res.ClaimBudgetPause == nil {
		t.Fatalf("budget stop: %+v %v", res, err)
	}
	if prompts != 1 || res.RepairRounds != 0 || res.ValidationPassed || res.Validation == nil || res.Validation.Passed {
		t.Fatalf("repair started past the budget or reported success: %d calls, %d rounds, passed %t", prompts, res.RepairRounds, res.ValidationPassed)
	}
	if !strings.Contains(res.ClaimBudgetPause.ContinueCommand, fmt.Sprintf("issue work %d --phase implement", issue)) {
		t.Fatal(res.ClaimBudgetPause.ContinueCommand)
	}
}

func TestWorkRepairRejectsForgedActiveFiles(t *testing.T) {
	for _, forged := range []string{validation.RepairFile, "PLAN.md", "VALIDATION.md"} {
		t.Run(forged, func(t *testing.T) {
			log := installFixableGo(t)
			e, issue := repairFixture(t, "2")
			var before string
			prompts := 0
			e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					if req.Role != "developer" {
						return (&agent.MockRunner{}).Run(ctx, req)
					}
					prompts++
					if prompts == 1 {
						return &agent.RunResult{}, writeFile("feature.txt", "feature\n")(req.WorktreePath)
					}
					st, err := e.LeaseManager.GetCurrentState(ctx, issue)
					if err != nil {
						return nil, err
					}
					before = st.HeadSHA
					// The sandbox asks before importing; one forged file rejects all.
					return nil, req.CheckImport(ctx, []string{"fixed.txt", ".swarm/active/" + forged})
				}}, nil
			}
			res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
			var rejection *importRejection
			if !errors.As(err, &rejection) || res == nil || res.ValidationPassed || prompts != 2 {
				t.Fatalf("forged %s: %v, %d calls", forged, err, prompts)
			}
			if _, statErr := os.Stat(filepath.Join(res.WorktreePath, "fixed.txt")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("source change partially imported: %v", statErr)
			}
			data, _ := os.ReadFile(log)
			if runs := strings.Count(string(data), "-count=1 ./..."); runs != 1 {
				t.Fatalf("validation ran after rejection: %d full runs", runs)
			}
			if st, stateErr := e.LeaseManager.GetCurrentState(t.Context(), issue); stateErr != nil || st.HeadSHA != before {
				t.Fatalf("rejected round moved the head: %v", stateErr)
			}
		})
	}
}
