package validation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/progress"
)

func setupGitRepoForValidation(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", tmp}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@localhost")

	for _, rel := range []string{
		".github/workflows/swarm-validate.yml",
		".swarm/validation/required-checks.yaml",
		".swarm/config.yaml",
		"main.go",
	} {
		full := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		var contents string
		if rel == ".swarm/validation/required-checks.yaml" {
			contents = "required_checks:\n  - name: ok\n    command: \"true\"\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n"
		} else if rel == ".swarm/config.yaml" {
			// These fixtures exercise validation policy in explicit host compatibility mode.
			contents = "version: 1\ndefault_agent: mock\ndefault_gate: none\nexecution:\n  mode: trusted-local\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  mock:\n    binary: git\n    harness: mock\n"
		} else {
			contents = "initial\n"
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit("add", ".")
	runGit("commit", "-m", "initial")

	activeDir := filepath.Join(tmp, ".swarm", "active")
	_ = os.MkdirAll(activeDir, 0o755)
	_ = os.WriteFile(filepath.Join(activeDir, "PLAN.md"), []byte("# Plan\n"), 0o644)
	_ = os.WriteFile(filepath.Join(activeDir, "TEST_PLAN.md"), []byte("# Test Plan\n"), 0o644)
	return tmp
}

func TestProtectedPathTamperDetection(t *testing.T) {
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	// Git's nonfatal line-ending warnings must never become filename data.
	if out, err := exec.Command("git", "-C", repo, "config", "core.autocrlf", "true").CombinedOutput(); err != nil {
		t.Fatalf("configure CRLF conversion: %v (%s)", err, out)
	}

	// Tamper with a protected file (.github/workflows/swarm-validate.yml)
	if err := os.WriteFile(filepath.Join(repo, ".github/workflows/swarm-validate.yml"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wu := &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}
	_, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base})
	if !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("expected ErrProtectedPathModified, got: %v", err)
	}

	// A candidate cannot remove protected paths or checks by replacing its own
	// policy; the baseline remains authoritative.
	if err := os.WriteFile(filepath.Join(repo, ".swarm", "validation", "required-checks.yaml"), []byte("required_checks: []\nprotected_paths: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base})
	if !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("candidate policy tampering must not weaken baseline: %v", err)
	}
}

func TestCommittedPolicyTamperingUsesTrustedBase(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	policy := filepath.Join(repo, ".swarm", "validation", "required-checks.yaml")
	if err := os.WriteFile(policy, []byte("required_checks: []\nprotected_paths: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("add", ".swarm/validation/required-checks.yaml")
	run("commit", "-m", "tamper policy")
	_, err := Validate(context.Background(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base})
	if !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("committed tampering must fail: %v", err)
	}
}

func TestNULPathsAndInstructionFilesAreNotIgnored(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	path := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "space \n name.yml"
	if runtime.GOOS == "windows" {
		// Windows rejects control characters in filenames; retain whitespace coverage.
		name = "space name.yml"
	}
	weird := filepath.Join(repo, ".github", "workflows", name)
	if err := os.WriteFile(weird, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Validate(context.Background(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base})
	if !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("protected filename containing whitespace must fail: %v", err)
	}
}

func TestSymlinkEscapeFailsClosed(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	outside := filepath.Join(t.TempDir(), "outside.yml")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repo, ".github", "workflows", "escape.yml")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := Validate(context.Background(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink escape must fail closed: %v", err)
	}
}

func gitRevision(t *testing.T, repo, revision string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", revision).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out[:len(out)-1])
}

func TestValidationRejectsMutableBaseline(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	for _, base := range []string{"HEAD", "HEAD~0", gitRevision(t, repo, "HEAD")[:12]} {
		t.Run(base, func(t *testing.T) {
			_, err := Validate(context.Background(), repo, nil, ValidateOptions{BaseSHA: base, CIMode: true})
			if !errors.Is(err, ErrInvalidPolicy) {
				t.Fatalf("mutable or abbreviated baseline %q must fail closed: %v", base, err)
			}
		})
	}
}

func TestForkModeProtectedPathsRejectHarnessAndWorkflowsDespiteAllowHarnessChanges(t *testing.T) {
	ctx := context.Background()
	for _, tamperedFile := range []string{
		".swarm/validation/required-checks.yaml",
		".swarm/config.yaml",
		".github/workflows/swarm-fork-coordinator.yml",
	} {
		t.Run(tamperedFile, func(t *testing.T) {
			repo := setupGitRepoForValidation(t)
			base := gitRevision(t, repo, "HEAD")
			full := filepath.Join(repo, tamperedFile)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("tampered: true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			wu := &contract.WorkUnit{ID: "ISSUE-42", IssueNumber: 42, AllowHarnessChanges: true}
			_, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base, ForkMode: true})
			if !errors.Is(err, ErrProtectedPathModified) {
				t.Fatalf("ForkMode Validate(%s) with AllowHarnessChanges=true = %v, want ErrProtectedPathModified", tamperedFile, err)
			}
		})
	}
}

func TestTemporaryTaskPointerIsNotAProtectedChange(t *testing.T) {
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@localhost"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	policy := "required_checks:\n  - name: ok\n    command: \"true\"\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n  - AGENTS.md\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := "# Team guide\n\nKeep changes small.\n"
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(agents), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "protect AGENTS.md")
	base := gitRevision(t, repo, "HEAD")
	wu := &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}
	pointer := "<!-- swarm:active:start -->\n## Active SwarmOS Work Unit\n<!-- swarm:active:end -->\n\n"

	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(pointer + agents)
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base}); err != nil {
		t.Fatalf("temporary task pointer failed validation: %v", err)
	}
	write(pointer + agents + "Also skip tests.\n")
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base}); !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("edit beside the task pointer passed: %v", err)
	}
	write(pointer + agents)
	git("add", "AGENTS.md")
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base}); !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("staged task pointer passed: %v", err)
	}
}

func TestRunnerIsolatedChecksDoNotNeedAgentSandbox(t *testing.T) {
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	// Standard policy: Codex's native sandbox, which stock CI runners lack.
	config := "version: 1\ndefault_agent: codex\ndefault_gate: agent\nexecution:\n  mode: isolated\n  backend: native\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_isolation: true\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  codex:\n    binary: swarm-test-missing-codex\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm", "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "standard isolation policy"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@localhost"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	base := gitRevision(t, repo, "HEAD")
	if _, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, BaseSHA: base}); err == nil {
		t.Fatal("workstation validation ran without the configured sandbox")
	}
	report, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, RunnerIsolated: true, BaseSHA: base})
	if err != nil || !report.Passed {
		t.Fatalf("CI runner validation = %+v, %v", report, err)
	}
}

func TestValidateExecutionModeLookup(t *testing.T) {
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	mode := func(value string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			if name == "SWARM_EXECUTION_MODE" {
				return value, true
			}
			return "", false
		}
	}
	t.Setenv("SWARM_EXECUTION_MODE", "process-mode")
	if _, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, BaseSHA: base}); err == nil || !strings.Contains(err.Error(), `execution mode "process-mode"`) {
		t.Fatalf("default validation did not read the process environment: %v", err)
	}
	if _, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, LookupEnv: mode("injected-mode")}); err == nil || !strings.Contains(err.Error(), `execution mode "injected-mode"`) {
		t.Fatalf("validation did not resolve the injected mode: %v", err)
	}
	report, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, LookupEnv: mode(contract.ExecutionTrustedLocal)})
	if err != nil || !report.Passed {
		t.Fatalf("injected trusted-local validation = %+v, %v", report, err)
	}
}

func TestValidateLookupDoesNotReachCheckEnvironment(t *testing.T) {
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	out := filepath.Join(t.TempDir(), "env.txt")
	policy := "required_checks:\n  - name: env\n    command: 'env > \"" + filepath.ToSlash(out) + "\"'\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "environment probe"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@localhost"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	base := gitRevision(t, repo, "HEAD")
	t.Setenv("LANG", "C.process")
	injected := map[string]string{"SWARM_EXECUTION_MODE": contract.ExecutionTrustedLocal, "LANG": "C.injected", "GITHUB_TOKEN": "publish-secret", "HOME": "/injected/home"}
	lookup := func(name string) (string, bool) {
		value, ok := injected[name]
		return value, ok
	}
	report, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, LookupEnv: lookup})
	if err != nil || !report.Passed {
		t.Fatalf("validation = %+v, %v", report, err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	env := string(raw)
	if !strings.Contains(env, "LANG=C.process\n") {
		t.Fatalf("check lost the process LANG:\n%s", env)
	}
	for _, leaked := range []string{"C.injected", "publish-secret", "/injected/home", "SWARM_EXECUTION_MODE"} {
		if strings.Contains(env, leaked) {
			t.Fatalf("injected %q reached the check environment:\n%s", leaked, env)
		}
	}
}

func TestReportProtectedChangesListsPolicyEditsAndStillRunsBaseChecks(t *testing.T) {
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	for _, rel := range []string{".github/workflows/swarm-validate.yml", ".swarm/validation/required-checks.yaml"} {
		// The candidate also tries to drop every required check.
		if err := os.WriteFile(filepath.Join(repo, rel), []byte("required_checks: []\nprotected_paths: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	report, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, ReportProtectedChanges: true})
	if err != nil {
		t.Fatalf("report mode: %v", err)
	}
	want := []string{".github/workflows/swarm-validate.yml", ".swarm/validation/required-checks.yaml"}
	if len(report.ProtectedChanges) != len(want) || report.ProtectedChanges[0] != want[0] || report.ProtectedChanges[1] != want[1] {
		t.Fatalf("protected changes = %v, want %v", report.ProtectedChanges, want)
	}
	if len(report.CheckOutcomes) != 1 || report.CheckOutcomes[0].Name != "ok" || !report.Passed {
		t.Fatalf("base checks must still run in report mode: %+v", report.CheckOutcomes)
	}

	if _, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, BaseSHA: base}); !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("enforce mode must still reject: %v", err)
	}
	wu := &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base, ReportProtectedChanges: true}); err == nil || errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("agent validation must refuse report mode before checking files: %v", err)
	}
	if _, err := Validate(ctx, repo, nil, ValidateOptions{CIMode: true, ForkMode: true, BaseSHA: base, ReportProtectedChanges: true}); err == nil {
		t.Fatal("fork mode must refuse report mode")
	}
}

func TestDefaultProtectedPathsCoverAgentInstructions(t *testing.T) {
	matches := func(file string) bool {
		for _, pattern := range contract.DefaultProtectedPaths() {
			if matchGlobPattern(pattern, file) {
				return true
			}
		}
		return false
	}
	for _, file := range []string{
		"AGENTS.md", "CLAUDE.md", ".swarm/project.md", ".swarm/architecture.md", ".swarm/conventions.md",
		".claude/commands/swarm-plan.md", ".opencode/commands/swarm-review.md", ".swarm/prompts/planner.md",
	} {
		if !matches(file) {
			t.Errorf("%s is not protected by default", file)
		}
	}
	for _, file := range []string{"docs/AGENTS.md", "internal/harness/templates/swarm/prompts/planner.md", "README.md", ".claude/settings.json"} {
		if matches(file) {
			t.Errorf("%s should stay editable", file)
		}
	}
}

func TestValidationProgressUsesTrustedChecks(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	candidate := "required_checks:\n  - name: candidate-secret\n    command: false\nprotected_paths: []\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm/validation/required-checks.yaml"), []byte(candidate), 0600); err != nil {
		t.Fatal(err)
	}
	var events []progress.Event
	report, err := Validate(t.Context(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1, AllowHarnessChanges: true}, ValidateOptions{BaseSHA: base, OnProgress: func(e progress.Event) { events = append(events, e) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Check != "ok" || events[0].Status != "started" || events[1].Check != "ok" || events[1].Status != "PASS" || events[1].StageElapsed != report.CheckOutcomes[0].Duration {
		t.Fatalf("unexpected lifecycle %+v", events)
	}
}

func TestValidationProgressFailureTimeoutAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, command, timeout string
		cancel                 bool
	}{
		{"failure", "printf 'PRIVATE_OUTPUT'; exit 7", "1m", false},
		{"timeout", "sleep 5", "10ms", false},
		{"cancel", "true", "1m", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := setupGitRepoForValidation(t)
			policy := "required_checks:\n  - name: trusted\n    command: \"" + tc.command + "\"\n    timeout: " + tc.timeout + "\nprotected_paths:\n  - protected/**\n"
			if err := os.WriteFile(filepath.Join(repo, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "."}, {"commit", "-m", "check policy"}} {
				if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			base := gitRevision(t, repo, "HEAD")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var events []progress.Event
			report, err := Validate(ctx, repo, nil, ValidateOptions{BaseSHA: base, OnProgress: func(e progress.Event) {
				events = append(events, e)
				if tc.cancel && e.Status == "started" {
					cancel()
				}
			}})
			if err == nil || report == nil || report.Passed {
				t.Fatalf("missing failed result: %+v %v", report, err)
			}
			if len(events) != 2 || events[0].Status != "started" || events[1].Status != "FAIL" || events[1].Check != "trusted" {
				t.Fatalf("unexpected events %+v", events)
			}
		})
	}
}

func TestValidatorSessionHeartbeatAdapter(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	ticks := make(chan time.Time)
	events := make(chan progress.Event, 10)
	var seconds atomic.Int64
	now := func() time.Time { return time.Unix(seconds.Load(), 0) }
	session := progress.New(t.Context(), 1, 0, func(e progress.Event) { events <- e }, ticks, now, func() {})
	<-events // preflight
	session.Next("validation")
	<-events
	<-events
	_, err := Validate(t.Context(), repo, nil, ValidateOptions{BaseSHA: base, OnProgress: func(e progress.Event) {
		session.Observe(e)
		if e.Status == "started" {
			started := <-events
			if started.Check != "ok" || started.Status != "started" {
				t.Fatalf("bad start %+v", started)
			}
			seconds.Store(15)
			ticks <- now()
			select {
			case running := <-events:
				if running.Check != "ok" || running.Status != "running" || running.StageElapsed != 15*time.Second {
					t.Fatalf("bad heartbeat %+v", running)
				}
			case <-time.After(time.Second):
				t.Fatal("missing heartbeat")
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	finish := <-events
	if finish.Check != "ok" || finish.Status != "PASS" {
		t.Fatalf("bad finish %+v", finish)
	}
	session.Finish("completed")
}

func TestProtectedPathGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern, target string
		want            bool
	}{
		{"**/.gitattributes", ".gitattributes", true},
		{"**/.gitattributes", "docs/.gitattributes", true},
		{"**/.gitattributes", "a/b/.gitattributes", true},
		{"**/.gitattributes", ".gitattributes.bak", false},
		{"**/.gitattributes", "a/b/.gitattributes.bak", false},
		{"**/.gitattributes", "a/.gitattributes/x", false},
		{"**/.gitattributes", "main.go", false},
		{"a/**/b.yaml", "a/b.yaml", true},
		{"a/**/b.yaml", "a/x/b.yaml", true},
		{"a/**/b.yaml", "a/x/y/b.yaml", true},
		{"a/**/b.yaml", "ab/b.yaml", false},
		{"a/**/b.yaml", "a/b.yml", false},
		{"a/**/b.yaml", "a/x/b.yaml/extra", false},
		{"a/**/b.yaml", "x/a/b.yaml", false},
		{"a/**/**/b.yaml", "a/b.yaml", true},
		{"a/**/**/b.yaml", "a/x/y/z/b.yaml", true},
		{"**/x/**/*.yml", "x/ci.yml", true},
		{"**/x/**/*.yml", "p/q/x/r/ci.yml", true},
		{"**/x/**/*.yml", "p/q/y/r/ci.yml", false},
		{"**/x/**/*.yml", "x/ci.yaml", false},
		{"**", "any/depth/file", true},
		{"**/**/**/**/**/**/**/**/**/**/**/**/z", "a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t/y", false},
	} {
		if got := matchGlobPattern(tc.pattern, tc.target); got != tc.want {
			t.Errorf("matchGlobPattern(%q, %q) = %v, want %v", tc.pattern, tc.target, got, tc.want)
		}
	}
}

// legacyMatchGlobPattern is the matcher before recursive ** support. Every
// target it matched must still match.
func legacyMatchGlobPattern(pattern, target string) bool {
	pattern, target = filepath.ToSlash(strings.TrimSpace(pattern)), filepath.ToSlash(target)
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return target == prefix || strings.HasPrefix(target, prefix+"/")
	}
	ok, err := filepath.Match(pattern, target)
	return (err == nil && ok) || pattern == target
}

func TestProtectedPathGlobCompatibility(t *testing.T) {
	// Only rows marked widened may match more than the legacy matcher did.
	for _, tc := range []struct {
		pattern, target string
		want, widened   bool
	}{
		// Patterns from .swarm/validation/required-checks.yaml.
		{".github/workflows/**", ".github/workflows", true, false},
		{".github/workflows/**", ".github/workflows/ci.yml", true, false},
		{".github/workflows/**", ".github/workflows/nested/ci.yml", true, false},
		{".github/workflows/**", ".github/workflowsx/ci.yml", false, false},
		{".github/workflows/**", ".github/ci.yml", false, false},
		{".gitattributes", ".gitattributes", true, false},
		{".gitattributes", "docs/.gitattributes", false, false},
		{"**/.gitattributes", "docs/.gitattributes", true, false},
		{"**/.gitattributes", ".gitattributes", true, true},
		{"**/.gitattributes", "a/b/.gitattributes", true, true},
		{"**/.gitattributes", "docs/gitattributes", false, false},
		{".swarm/validation/**", ".swarm/validation/required-checks.yaml", true, false},
		{".swarm/validation/**", ".swarm/validation", true, false},
		{".swarm/validation/**", ".swarm/validationx", false, false},
		{".swarm/contracts/**", ".swarm/contracts/a/b.json", true, false},
		{".swarm/config.yaml", ".swarm/config.yaml", true, false},
		{".swarm/config.yaml", ".swarm/config.yml", false, false},
		{".swarm/prompts/**", ".swarm/prompts/planner.md", true, false},
		{".swarm/workflows/**", ".swarm/workflows/bugfix.yaml", true, false},
		{".swarm/goals.md", ".swarm/goals.md", true, false},
		{".swarm/SKILLS.md", ".swarm/SKILLS.md", true, false},
		{".swarm/SKILLS.md", ".swarm/skills.md", false, false},
		{".swarm/rules/**", ".swarm/rules/lessons.md", true, false},
		{".swarm/skills/**", ".swarm/skills/triage-and-plan.md", true, false},
		{".swarm/project.md", ".swarm/project.md", true, false},
		{".swarm/architecture.md", ".swarm/architecture.md", true, false},
		{".swarm/conventions.md", ".swarm/conventions.md", true, false},
		{".claude/commands/**", ".claude/commands/swarm-plan.md", true, false},
		{".claude/commands/**", ".claude/settings.json", false, false},
		{".opencode/commands/**", ".opencode/commands/swarm-review.md", true, false},
		{"AGENTS.md", "AGENTS.md", true, false},
		{"AGENTS.md", "docs/AGENTS.md", false, false},
		{"CLAUDE.md", "CLAUDE.md", true, false},
		// Ordinary filepath.Match patterns.
		{"docs/*.md", "docs/a.md", true, false},
		{"docs/*.md", "docs/a/b.md", false, false},
		{"docs/?.md", "docs/a.md", true, false},
		{"docs/?.md", "docs/ab.md", false, false},
		{"docs/[ab].md", "docs/b.md", true, false},
		{"docs/[^ab].md", "docs/b.md", false, false},
		{"a**b", "axxb", true, false},
		{"a**b", "ax/xb", false, false},
		{"  AGENTS.md  ", "AGENTS.md", true, false},
		// Legacy literal prefix shortcut for wildcard prefixes.
		{"*/**", "*/x", true, false},
		{"*/**", "dir/x", true, true},
		{"docs/*/**", "docs/a/b/c", true, true},
	} {
		got, legacy := matchGlobPattern(tc.pattern, tc.target), legacyMatchGlobPattern(tc.pattern, tc.target)
		if got != tc.want {
			t.Errorf("matchGlobPattern(%q, %q) = %v, want %v", tc.pattern, tc.target, got, tc.want)
		}
		if legacy && !got {
			t.Errorf("%q no longer matches %q", tc.pattern, tc.target)
		}
		if got != legacy && !tc.widened {
			t.Errorf("%q on %q changed from %v without being marked widened", tc.pattern, tc.target, legacy)
		}
	}
	if runtime.GOOS != "windows" {
		if !matchGlobPattern(`docs/\*.md`, "docs/*.md") || matchGlobPattern(`docs/\*.md`, "docs/a.md") {
			t.Error("escaped star must stay literal")
		}
	}

	// Every pattern in this repository's policy must appear in the table above.
	data, err := os.ReadFile(filepath.Join("..", "..", ".swarm", "validation", "required-checks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := harness.DecodeValidationSpec(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range spec.ProtectedPaths {
		if err := checkProtectedPattern(pattern); err != nil {
			t.Errorf("repository pattern rejected: %v", err)
		}
		if !matchGlobPattern(pattern, strings.TrimSuffix(strings.ReplaceAll(pattern, "**/", ""), "/**")) {
			t.Errorf("repository pattern %q does not match its own path", pattern)
		}
	}
}

func commitPolicy(t *testing.T, repo, policy string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "policy"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	return gitRevision(t, repo, "HEAD")
}

func TestTrustedProtectedPatternValidation(t *testing.T) {
	malformed := []string{"[", "docs/[a-", "a*b[", "a/**/[", "[/**", "x*/[a-/**", "[]", "[a-]"}
	if runtime.GOOS != "windows" {
		malformed = append(malformed, `docs\`, `*[\`)
	}
	for _, pattern := range malformed {
		t.Run(pattern, func(t *testing.T) {
			repo := setupGitRepoForValidation(t)
			policy := "required_checks:\n  - name: ran\n    command: \"touch ran\"\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - '" + pattern + "'\n"
			base := commitPolicy(t, repo, policy)
			// A valid candidate policy cannot repair the trusted one.
			valid := "required_checks:\n  - name: ok\n    command: \"true\"\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n"
			if err := os.WriteFile(filepath.Join(repo, ".swarm", "validation", "required-checks.yaml"), []byte(valid), 0o644); err != nil {
				t.Fatal(err)
			}
			check := func(name string, err error) {
				t.Helper()
				if !errors.Is(err, ErrInvalidPolicy) || !strings.Contains(err.Error(), fmt.Sprintf("%q", pattern)) {
					t.Errorf("%s: got %v, want ErrInvalidPolicy naming %q", name, err, pattern)
				}
			}
			_, err := loadTrustedValidationSpec(repo, base)
			check("load", err)
			_, err = Validate(t.Context(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1, AllowHarnessChanges: true}, ValidateOptions{BaseSHA: base})
			check("harness changes allowed", err)
			_, err = Validate(t.Context(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, ReportProtectedChanges: true})
			check("CI report mode", err)
			if _, err := os.Stat(filepath.Join(repo, "ran")); !os.IsNotExist(err) {
				t.Errorf("checks ran despite a malformed policy: %v", err)
			}
		})
	}
	for _, pattern := range []string{"[a-c]/**", "**/.gitattributes", "a/**/b.yaml", "docs/[^x]?.md", `\*`} {
		if err := checkProtectedPattern(pattern); err != nil {
			t.Errorf("valid pattern %q rejected: %v", pattern, err)
		}
	}
}

func TestNestedProtectedPathValidation(t *testing.T) {
	const policy = "required_checks:\n  - name: ok\n    command: \"true\"\n    timeout: 1m\nprotected_paths:\n  - '**/.gitattributes'\n"
	git := func(t *testing.T, repo string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	write := func(t *testing.T, repo, rel string) {
		t.Helper()
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("*.bin binary\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(t *testing.T) (string, string) {
		repo := setupGitRepoForValidation(t)
		if err := os.MkdirAll(filepath.Join(repo, "a", "b"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "a", "b", ".gitattributes"), []byte("*.txt text\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return repo, commitPolicy(t, repo, policy)
	}
	for _, state := range []string{"committed", "staged", "unstaged", "untracked"} {
		t.Run(state, func(t *testing.T) {
			repo, base := setup(t)
			file := "a/b/.gitattributes"
			switch state {
			case "committed":
				write(t, repo, file)
				git(t, repo, "add", file)
				git(t, repo, "commit", "-qm", "edit nested attributes")
			case "staged":
				write(t, repo, file)
				git(t, repo, "add", file)
			case "unstaged":
				write(t, repo, file)
			case "untracked":
				file = "x/y/.gitattributes"
				write(t, repo, file)
			}
			want := fmt.Sprintf("file %q matches protected pattern %q", file, "**/.gitattributes")
			for name, opts := range map[string]ValidateOptions{
				"issue": {BaseSHA: base},
				"CI":    {BaseSHA: base, CIMode: true},
				"fork":  {BaseSHA: base, ForkMode: true},
			} {
				var wu *contract.WorkUnit
				if !opts.CIMode {
					wu = &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}
				}
				_, err := Validate(t.Context(), repo, wu, opts)
				if !errors.Is(err, ErrProtectedPathModified) || !strings.Contains(err.Error(), want) {
					t.Errorf("%s: got %v, want protected-path violation %s", name, err, want)
				}
			}
			report, err := Validate(t.Context(), repo, nil, ValidateOptions{BaseSHA: base, CIMode: true, ReportProtectedChanges: true})
			if err != nil || len(report.ProtectedChanges) != 1 || report.ProtectedChanges[0] != file || len(report.CheckOutcomes) != 1 || !report.Passed {
				t.Errorf("report mode = %+v, %v", report, err)
			}
		})
	}
	t.Run("unrelated file", func(t *testing.T) {
		repo, base := setup(t)
		if err := os.WriteFile(filepath.Join(repo, "a", "b", "notes.txt"), []byte("ok\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Validate(t.Context(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base}); err != nil {
			t.Fatalf("unrelated nested edit rejected: %v", err)
		}
	})
}

func TestSkippedTestNovelty(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, cacheableTestCheck, nil)
	for _, tc := range []struct{ file, pkg, name, source, want string }{
		{"leaf/leaf_test.go", "example.com/m/leaf", "TestLeaf", goModuleFixture["leaf/leaf_test.go"], "unknown"},
		{"leaf/leaf_test.go", "example.com/m/leaf", "TestAdded/sub", goModuleFixture["leaf/leaf_test.go"] + "\nfunc TestAdded(t *testing.T) {}\n", "new"},
		{"extra/extra_test.go", "example.com/m/extra", "TestNew", "package extra\nimport \"testing\"\nfunc TestNew(t *testing.T) {}\n", "new"},
	} {
		writeRepoFile(t, repo, tc.file, tc.source)
		got := skipStatus(t.Context(), repo, base, []string{tc.file}, SkippedTest{Package: tc.pkg, Test: tc.name})
		if got != tc.want {
			t.Fatalf("%s: %s", tc.name, got)
		}
	}
	if got := skipStatus(t.Context(), repo, "missing", []string{"extra/extra_test.go"}, SkippedTest{Package: "example.com/m/extra", Test: "TestNew"}); got != "unknown" {
		t.Fatal(got)
	}
}

func TestJSONCompileFailureReadable(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, cacheableTestCheck, nil)
	commitRepoFile(t, repo, "leaf/leaf.go", "package leaf\nfunc Value() int { return missing }\n")
	report, err := Validate(t.Context(), repo, nil, ValidateOptions{BaseSHA: base, CIMode: true})
	if !errors.Is(err, ErrRequiredCheckFailed) || report.Passed {
		t.Fatalf("%+v %v", report, err)
	}
	outcome := report.CheckOutcomes[0]
	if !strings.Contains(outcome.Output, "undefined: missing") || strings.Contains(outcome.Output, `"Action"`) || !strings.Contains(report.Artifact.Body, "undefined: missing") || outcome.SkippedTotal != 0 {
		t.Fatalf("%+v", outcome)
	}
}
