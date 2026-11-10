package validation

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
)

func setupGitRepoForValidation(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := gitfixture.Command(t, "", append([]string{"-C", tmp}, args...)...)
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
	t.Parallel()
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	// Git's nonfatal line-ending warnings must never become filename data.
	if out, err := gitfixture.Command(t, "", "-C", repo, "config", "core.autocrlf", "true").CombinedOutput(); err != nil {
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
	t.Parallel()
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	policy := filepath.Join(repo, ".swarm", "validation", "required-checks.yaml")
	if err := os.WriteFile(policy, []byte("required_checks: []\nprotected_paths: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := gitfixture.Command(t, "", append([]string{"-C", repo}, args...)...)
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
	t.Parallel()
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
	t.Parallel()
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
	out, err := gitfixture.Command(t, "", "-C", repo, "rev-parse", revision).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out[:len(out)-1])
}

func TestValidationRejectsMutableBaseline(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	git := func(args ...string) {
		t.Helper()
		if out, err := gitfixture.Command(t, "", append([]string{"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@localhost"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	policy := "required_checks:\n  - name: ok\n    command: \"true\"\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n  - AGENTS.md\n  - CLAUDE.md\n"
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
	writeRepoFile(t, repo, "CLAUDE.md", pointer)
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base}); err != nil {
		t.Fatal(err)
	}
	git("add", "CLAUDE.md")
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base}); !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("staged CLAUDE pointer accepted: %v", err)
	}
	git("reset", "--", "CLAUDE.md")
	write(pointer + agents)
	git("add", "AGENTS.md")
	if _, err := Validate(ctx, repo, wu, ValidateOptions{BaseSHA: base}); !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("staged task pointer passed: %v", err)
	}
}

func TestRunnerIsolatedChecksDoNotNeedAgentSandbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := setupGitRepoForValidation(t)
	// Standard policy: Codex's native sandbox, which stock CI runners lack.
	config := "version: 1\ndefault_agent: codex\ndefault_gate: agent\nexecution:\n  mode: isolated\n  backend: native\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_isolation: true\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  codex:\n    binary: swarm-test-missing-codex\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm", "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "standard isolation policy"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@localhost"}, args...)...).CombinedOutput(); err != nil {
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
		if out, err := gitfixture.Command(t, "", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@localhost"}, args...)...).CombinedOutput(); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if len(events) != 4 || events[0].Check != "preparation: preflight" || events[1].Status != "PASS" {
		t.Fatalf("preflight events: %+v", events)
	}
	events = events[2:]
	if len(events) != 2 || events[0].Check != "ok" || events[0].Status != "started" || events[1].Check != "ok" || events[1].Status != "PASS" || events[1].StageElapsed != report.CheckOutcomes[0].Duration {
		t.Fatalf("unexpected lifecycle %+v", events)
	}
}

func TestValidationProgressFailureTimeoutAndCancellation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, command, timeout string
		cancel                 bool
	}{
		{"failure", "printf 'PRIVATE_OUTPUT'; exit 7", "1m", false},
		{"timeout", "sleep 5", "10ms", false},
		{"cancel", "true", "1m", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := setupGitRepoForValidation(t)
			policy := "required_checks:\n  - name: trusted\n    command: \"" + tc.command + "\"\n    timeout: " + tc.timeout + "\nprotected_paths:\n  - protected/**\n"
			if err := os.WriteFile(filepath.Join(repo, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "."}, {"commit", "-m", "check policy"}} {
				if out, err := gitfixture.Command(t, "", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			base := gitRevision(t, repo, "HEAD")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var events []progress.Event
			report, err := Validate(ctx, repo, nil, ValidateOptions{BaseSHA: base, OnProgress: func(e progress.Event) {
				if strings.HasPrefix(e.Check, "preparation:") {
					return
				}
				events = append(events, e)
				if tc.cancel && e.Status == "started" {
					cancel()
				}
			}})
			if err == nil || report == nil || report.Passed {
				t.Fatalf("missing failed result: %+v %v", report, err)
			}
			wantStatus := "FAIL"
			if tc.name == "timeout" {
				wantStatus = OutcomeTimeLimit
			}
			if len(events) != 2 || events[0].Status != "started" || events[1].Status != wantStatus || events[1].Check != "trusted" {
				t.Fatalf("unexpected events %+v", events)
			}
		})
	}
}

func TestValidatorSessionHeartbeatAdapter(t *testing.T) {
	t.Parallel()
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
		if strings.HasPrefix(e.Check, "preparation:") {
			return
		}
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
	t.Parallel()
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
	t.Parallel()
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
		if out, err := gitfixture.Command(t, "", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	return gitRevision(t, repo, "HEAD")
}

func TestTrustedProtectedPatternValidation(t *testing.T) {
	t.Parallel()
	malformed := []string{"[", "docs/[a-", "a*b[", "a/**/[", "[/**", "x*/[a-/**", "[]", "[a-]"}
	if runtime.GOOS != "windows" {
		malformed = append(malformed, `docs\`, `*[\`)
	}
	for _, pattern := range malformed {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
	const policy = "required_checks:\n  - name: ok\n    command: \"true\"\n    timeout: 1m\nprotected_paths:\n  - '**/.gitattributes'\n"
	git := func(t *testing.T, repo string, args ...string) {
		t.Helper()
		if out, err := gitfixture.Command(t, "", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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

func TestCheckpointBoundaryUsesTrustedPolicyWithoutExecutingPreview(t *testing.T) {
	// Selection reads the operator mode, so this test is serial.
	t.Setenv("SWARM_EXECUTION_MODE", "isolated")
	root := setupGitRepoForValidation(t)
	cfg := "version: 1\ndefault_agent: codex\ndefault_gate: none\nexecution:\n  mode: isolated\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_isolation: true\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  codex:\n    binary: swarm-checkpoint-missing-codex-309\n    harness: codex\n"
	configPath := filepath.Join(root, ".swarm/config.yaml")
	if err := os.WriteFile(configPath, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".swarm/config.yaml"}, {"commit", "-m", "approved isolated policy"}} {
		if out, err := gitfixture.Command(t, root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	raw, err := gitfixture.Command(t, root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(string(raw))
	// A candidate policy cannot select host checks or substitute the boundary.
	if err := os.WriteFile(configPath, []byte("candidate policy is invalid"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, probe := range []bool{false, true} {
		if _, err = CheckCheckpointBoundary(context.Background(), root, base, probe); err == nil || !strings.Contains(err.Error(), "swarm-checkpoint-missing-codex-309") {
			t.Fatalf("unavailable approved boundary accepted: %v", err)
		}
	}
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	if _, err = CheckCheckpointBoundary(context.Background(), root, base, false); err == nil || !strings.Contains(err.Error(), "requires isolated") {
		t.Fatalf("trusted isolation floor ignored: %v", err)
	}
	t.Setenv("SWARM_EXECUTION_MODE", "isolated")
	marker := filepath.Join(t.TempDir(), "boundary-ran")
	binary := filepath.Join(t.TempDir(), "codex")
	if err = testutil.WriteExecutable(binary, []byte("#!/bin/sh\nprintf invoked > '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg = strings.Replace(cfg, "swarm-checkpoint-missing-codex-309", binary, 1)
	if err = os.WriteFile(configPath, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".swarm/config.yaml"}, {"commit", "-m", "available boundary fixture"}} {
		if out, err := gitfixture.Command(t, root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	raw, err = gitfixture.Command(t, root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	base = strings.TrimSpace(string(raw))
	pending, err := CheckCheckpointBoundary(context.Background(), root, base, false)
	if err != nil || !pending {
		t.Fatalf("preview: pending=%t err=%v", pending, err)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("preview executed boundary canary: %v", err)
	}
}

func TestValidationInstructionRestoration(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			command := "test ! -e CLAUDE.md && ! grep -q swarm:active AGENTS.md"
			if fail {
				command += "; exit 7"
			}
			repo, base := setupGoFastPassRepo(t, "  - name: instructions\n    command: '"+command+"'\n    timeout: 1m\n", map[string]string{"AGENTS.md": "Original\n"})
			pointer := "<!-- swarm:active:start -->\nTask\n<!-- swarm:active:end -->\n\n"
			writeRepoFile(t, repo, "AGENTS.md", pointer+"Original\n")
			writeRepoFile(t, repo, "CLAUDE.md", pointer)
			index, err := os.ReadFile(filepath.Join(repo, ".git/index"))
			if err != nil {
				t.Fatal(err)
			}
			report, err := Validate(t.Context(), repo, nil, ValidateOptions{BaseSHA: base})
			for name, want := range map[string]string{"AGENTS.md": pointer + "Original\n", "CLAUDE.md": pointer} {
				got, readErr := os.ReadFile(filepath.Join(repo, name))
				if readErr != nil || string(got) != want {
					t.Fatalf("restoration %s: %q %v", name, got, readErr)
				}
			}
			after, _ := os.ReadFile(filepath.Join(repo, ".git/index"))
			if string(after) != string(index) {
				t.Fatal("index changed")
			}
			if report == nil || report.Passed == fail || (err != nil) != fail {
				t.Fatalf("report %+v: %v", report, err)
			}
		})
	}
}

func TestValidationCIKeepsInstructionMarkers(t *testing.T) {
	pointer := "<!-- swarm:active:start -->\nCommitted\n<!-- swarm:active:end -->\n\n"
	repo, base := setupGoFastPassRepo(t, "  - name: committed\n    command: grep -q swarm:active AGENTS.md\n    timeout: 1m\n", map[string]string{"AGENTS.md": pointer})
	report, err := Validate(t.Context(), repo, nil, ValidateOptions{BaseSHA: base, CIMode: true})
	if err != nil || !report.Passed {
		t.Fatalf("%+v %v", report, err)
	}
}

func TestValidationInstructionRestoreFailure(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, "  - name: unsafe\n    command: ln -s AGENTS.md CLAUDE.md\n    timeout: 1m\n", map[string]string{"AGENTS.md": "guide\n"})
	pointer := "<!-- swarm:active:start -->\nTask\n<!-- swarm:active:end -->\n\n"
	writeRepoFile(t, repo, "CLAUDE.md", pointer)
	report, err := Validate(t.Context(), repo, nil, ValidateOptions{BaseSHA: base})
	if err == nil || errors.Is(err, ErrRequiredCheckFailed) || report != nil {
		t.Fatalf("restoration became repairable/reusable: %+v %v", report, err)
	}
}

func TestValidationCancellationRestoresInstructions(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, "  - name: cancelled\n    command: sleep 1\n    timeout: 1m\n", map[string]string{"AGENTS.md": "guide\n"})
	pointer := "<!-- swarm:active:start -->\nTask\n<!-- swarm:active:end -->\n\n"
	writeRepoFile(t, repo, "AGENTS.md", pointer+"guide\n")
	writeRepoFile(t, repo, "CLAUDE.md", pointer)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	report, err := Validate(ctx, repo, nil, ValidateOptions{BaseSHA: base, OnProgress: func(e progress.Event) {
		if e.Check == "cancelled" && e.Status == "started" {
			cancel()
		}
	}})
	if err == nil || report == nil || report.Repair != nil {
		t.Fatalf("cancelled report %+v %v", report, err)
	}
	for name, want := range map[string]string{"AGENTS.md": pointer + "guide\n", "CLAUDE.md": pointer} {
		data, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil || string(data) != want {
			t.Fatalf("cancel restoration %s %q %v", name, data, err)
		}
	}
}

func TestCandidateTrackerConfigValidation(t *testing.T) {
	t.Parallel()
	root := setupGitRepoForValidation(t)
	base := gitRevision(t, root, "HEAD")
	path := filepath.Join(root, ".swarm", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\ntracker: unknown\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = Validate(context.Background(), root, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base})
	if err == nil || !strings.Contains(err.Error(), "tracker") || !strings.Contains(err.Error(), ".swarm/config.yaml") {
		t.Fatalf("invalid candidate tracker: %v", err)
	}
	if cfg, err := harness.LoadConfigAt(root, base); err != nil {
		t.Fatal(err)
	} else if value, _ := contract.ResolveTracker(cfg); value != "github" {
		t.Fatal(value)
	}
}

func TestCandidateTrackerDoesNotLoadExecutionPolicy(t *testing.T) {
	t.Parallel()
	root := setupGitRepoForValidation(t)
	base := gitRevision(t, root, "HEAD")
	path := filepath.Join(root, ".swarm", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate := strings.Replace(string(data), "mode: trusted-local", "mode: invalid-candidate-mode", 1) + "\ntracker: local\n"
	if err := os.WriteFile(path, []byte(candidate), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.LoadConfig(root); err == nil {
		t.Fatal("fixture must have invalid candidate execution policy")
	}
	report, err := Validate(context.Background(), root, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base})
	if err != nil || report == nil || !report.Passed {
		t.Fatalf("approved execution policy was replaced by candidate: %+v %v", report, err)
	}
}

// Load the repository policy rather than a copy that could drift from PR checks.
func repositoryCLIRegressionsCheck(t *testing.T) contract.CheckSpec {
	t.Helper()
	spec, err := harness.LoadValidationSpec(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var found []contract.CheckSpec
	for _, check := range spec.RequiredChecks {
		if check.Name == "cli-regressions" {
			found = append(found, check)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one cli-regressions check, got %d", len(found))
	}
	check := found[0]
	if check.Command != "go test -timeout=30m -parallel=8 ./internal/cli" {
		t.Fatalf("want unfiltered full CLI command, got %q", check.Command)
	}
	if check.Timeout != "30m" {
		t.Fatalf("want policy timeout 30m, got %q", check.Timeout)
	}
	return check
}

func TestRepositoryCLIRegressionsPolicy(t *testing.T) {
	repositoryCLIRegressionsCheck(t)
}

func TestRepositoryCLIRegressionsRejectsNewFailure(t *testing.T) {
	check := repositoryCLIRegressionsCheck(t)
	root := t.TempDir()
	cli := filepath.Join(root, "internal", "cli")
	if err := os.MkdirAll(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "go.mod"), "module example.com/cli-policy-fixture\n\ngo 1.22\n")
	testFile := filepath.Join(cli, "regression_test.go")
	const testName = "TestArbitraryFutureCLIRegression"
	source := func(body string) string {
		return "package cli\nimport \"testing\"\nfunc TestOrdinaryPassing(t *testing.T) {}\nfunc " + testName + "(t *testing.T) {" + body + "}\n"
	}
	run := func() ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", check.Command)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off")
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("fixture command timed out: %v (%s)", ctx.Err(), out)
		}
		return out, err
	}
	write(testFile, source(`t.Fatal("arbitrary CLI failure reached")`))
	out, err := run()
	if err == nil || !strings.Contains(string(out), "--- FAIL: "+testName) || !strings.Contains(string(out), "arbitrary CLI failure reached") {
		t.Fatalf("want failure from arbitrary CLI test, got %v (%s)", err, out)
	}
	write(testFile, source(""))
	if out, err := run(); err != nil {
		t.Fatalf("passing control failed: %v (%s)", err, out)
	}
}

func TestCheckTimeoutHonoursPolicyUpToCeiling(t *testing.T) {
	for _, tc := range []struct {
		configured string
		want       time.Duration
	}{
		{"", defaultCheckTimeout},
		{"bad", defaultCheckTimeout},
		{"0s", defaultCheckTimeout},
		{"-5m", defaultCheckTimeout},
		{"5m", 5 * time.Minute},
		{"30m", 30 * time.Minute},
		{"3h", maxCheckTimeout},
	} {
		if got := checkTimeout(tc.configured); got != tc.want {
			t.Errorf("checkTimeout(%q) = %s, want %s", tc.configured, got, tc.want)
		}
	}
}

func TestRequiredCheckDurationHistory(t *testing.T) {
	useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	base := gitRevision(t, repo, "HEAD")
	// Use the repository's actual approved command when checking dispatch.
	spec, err := loadTrustedValidationSpec(repo, base)
	if err != nil {
		t.Fatal(err)
	}
	check := spec.RequiredChecks[0]
	policy := checkTimeout(check.Timeout)
	recorded := min(max(policy, time.Second), 25*time.Minute)
	recordRequiredDurations(t.Context(), repo, []CheckRunOutcome{{Name: check.Name, Command: check.Command, Passed: true, Duration: recorded}})
	if got := requiredCheckBudget(t.Context(), repo, check); got != max(policy, recorded*2) {
		t.Fatalf("budget %v", got)
	}
	budgets := []time.Duration{}
	report, err := Validate(t.Context(), repo, nil, ValidateOptions{BaseSHA: base, runCommand: func(ctx context.Context, cmd string, budget time.Duration, stdout, stderr io.Writer) error {
		budgets = append(budgets, budget)
		return nil
	}})
	if err != nil || !report.Passed || len(budgets) == 0 || budgets[len(budgets)-1] != max(policy, recorded*2) {
		t.Fatalf("dispatch %v: %+v %v", budgets, report, err)
	}
	check.Command += " "
	if got := requiredCheckBudget(t.Context(), repo, check); got != policy {
		t.Fatalf("changed command %v", got)
	}
	recordRequiredDurations(t.Context(), repo, []CheckRunOutcome{{Name: check.Name, Command: check.Command, Passed: true, Duration: 50 * time.Minute}})
	if got := requiredCheckBudget(t.Context(), repo, check); got != time.Hour {
		t.Fatalf("ceiling %v", got)
	}
	check.Name += "-different"
	if got := requiredCheckBudget(t.Context(), repo, check); got != policy {
		t.Fatalf("name reset %v", got)
	}
}

func TestRequiredCheckCPUStall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux procfs")
	}
	for _, tc := range []struct {
		name, command string
		stalled       bool
	}{
		{"sleeping descendants", "sleep 10 & echo child:$!; wait", true},
		{"active grandchild", "sh -c 'i=0; while [ $i -lt 400000 ]; do i=$((i+1)); done' & wait", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &checkRunner{worktree: t.TempDir(), home: t.TempDir(), stallWindow: 150 * time.Millisecond, sampleInterval: 20 * time.Millisecond}
			start := time.Now()
			c, err := r.runCheck(t.Context(), tc.name, tc.command, 5*time.Second, nil)
			if tc.stalled {
				if err == nil || c.Outcome != OutcomeStalled || time.Since(start) > 3*time.Second {
					t.Fatalf("%+v %v", c, err)
				}
				pid := strings.TrimSpace(strings.TrimPrefix(c.Output, "child:"))
				if raw, e := os.ReadFile(filepath.Join("/proc", pid, "stat")); e == nil {
					end := strings.LastIndexByte(string(raw), ')')
					if end < 0 || !strings.HasPrefix(string(raw[end+1:]), " Z ") {
						t.Fatalf("stalled child still running: %s", raw)
					}
				}
			} else if err != nil || !c.Passed {
				t.Fatalf("CPU progress killed: %+v %v", c, err)
			}
		})
	}
	t.Run("supervisor preserves output and exit", func(t *testing.T) {
		marker := "swarm-test-marker"
		cmd := exec.Command("sh", "-c", nativeCheckSupervisor("printf '%s' \"quoted ' value\"; exit 7", marker))
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if string(out) != "quoted ' value" || !errors.As(err, &exit) || exit.ExitCode() != 7 {
			t.Fatalf("%q %v", out, err)
		}
	})
}

func TestRequiredCheckUnobservableTree(t *testing.T) {
	r := &checkRunner{worktree: t.TempDir(), home: t.TempDir(), stallWindow: 10 * time.Millisecond, sampleInterval: time.Millisecond, sampleProcesses: func() (map[int]checkProcess, error) { return nil, errors.New("unreadable procfs") }}
	c, err := r.runCheck(t.Context(), "unreadable", "sleep 10", 100*time.Millisecond, nil)
	if err == nil || c.Outcome != OutcomeTimeLimit {
		t.Fatalf("%+v %v", c, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c, err = r.runCheck(ctx, "cancelled", "sleep 10", time.Second, nil)
	if err == nil || validLimit(c.Outcome) {
		t.Fatalf("parent cancellation classified as limit: %+v %v", c, err)
	}
	// All backends receive the centrally computed deadline; OCI never starts a
	// procfs observer, even when its client is an idle descendant.
	r.execution = contract.ExecutionConfig{Mode: contract.ExecutionIsolated, Backend: contract.BackendOCI}
	var observations atomic.Int32
	r.sampleProcesses = func() (map[int]checkProcess, error) {
		observations.Add(1)
		return nil, errors.New("must not observe OCI")
	}
	r.runOCI = func(ctx context.Context, _ string, budget time.Duration, _, _ io.Writer) error {
		if budget != 20*time.Millisecond {
			t.Errorf("OCI budget %v", budget)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	c, err = r.runCheck(t.Context(), "OCI", "fake runtime", 20*time.Millisecond, nil)
	if err == nil || c.Outcome != OutcomeTimeLimit {
		t.Fatalf("OCI: %+v %v", c, err)
	}
	if observations.Load() != 0 {
		t.Fatal("OCI client was observed")
	}
}

func TestRequiredCheckCPUStallObserver(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux observer")
	}
	for _, tc := range []struct {
		name     string
		errAfter int
		mutate   func(int, map[int]checkProcess)
		stalled  bool
	}{
		{name: "idle native root", stalled: true},
		{name: "unreadable after discovery", errAfter: 2},
		{name: "missing intermediate ancestry", mutate: func(i int, m map[int]checkProcess) {
			if i > 1 {
				delete(m, 100002)
				m[100003] = checkProcess{pid: 100003, parent: 100002, start: 3, cpu: 100}
			}
		}},
		{name: "active grandchild", mutate: func(i int, m map[int]checkProcess) {
			m[100003] = checkProcess{pid: 100003, parent: 100002, start: 3, cpu: uint64(i)}
		}},
		{name: "short lived children", mutate: func(i int, m map[int]checkProcess) {
			p := m[100001]
			p.cpu = uint64(i)
			m[100001] = p
			m[100004+i] = checkProcess{pid: 100004 + i, parent: 100001, start: uint64(4 + i), cpu: 1}
		}},
		{name: "PID identity changed", mutate: func(i int, m map[int]checkProcess) {
			if i > 1 {
				p := m[100001]
				p.start = 99
				m[100001] = p
			}
		}},
		{name: "ambiguous marker", mutate: func(_ int, m map[int]checkProcess) {
			m[100005] = checkProcess{pid: 100005, parent: os.Getpid(), start: 5, args: []string{"sh", "-c", "supervisor", "marker"}}
		}},
		{name: "missing root", mutate: func(_ int, m map[int]checkProcess) { delete(m, 100001) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			i := 0
			r := &checkRunner{stallWindow: 30 * time.Millisecond, sampleInterval: time.Millisecond, now: func() time.Time { return time.Unix(1, int64(i)*int64(10*time.Millisecond)) }, sampleProcesses: func() (map[int]checkProcess, error) {
				i++
				if tc.errAfter != 0 && i >= tc.errAfter {
					return nil, errors.New("procfs unreadable")
				}
				m := map[int]checkProcess{os.Getpid(): {pid: os.Getpid(), start: 1}, 100001: {pid: 100001, parent: os.Getpid(), start: 1, args: []string{"sh", "-c", "supervisor", "marker"}}, 100002: {pid: 100002, parent: 100001, start: 2}, 100006: {pid: 100006, parent: os.Getpid(), start: 6, cpu: 999, args: []string{"sh", "-c", "unrelated", "other-marker"}}}
				if tc.mutate != nil {
					tc.mutate(i, m)
				}
				return m, nil
			}}
			stop := r.observeCPU(ctx, cancel, 0, "marker")
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Millisecond):
			}
			stop() // also proves monitor termination without a running child.
			if errors.Is(context.Cause(ctx), errCheckStalled) != tc.stalled {
				t.Fatalf("cause: %v", context.Cause(ctx))
			}
		})
	}
	t.Run("direct root pinned before wait", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		calls := 0
		r := &checkRunner{stallWindow: 3 * time.Millisecond, sampleInterval: time.Millisecond, sampleProcesses: func() (map[int]checkProcess, error) {
			calls++
			start := uint64(1)
			if calls > 1 {
				start = 99
			}
			return map[int]checkProcess{100001: {pid: 100001, start: start}}, nil
		}}
		stop := r.observeCPU(ctx, cancel, 100001, "")
		select {
		case <-ctx.Done():
		case <-time.After(15 * time.Millisecond):
		}
		stop()
		if context.Cause(ctx) != nil {
			t.Fatalf("reused direct PID triggered stall: %v", context.Cause(ctx))
		}
	})
	if cpuObservationEnabled("darwin", contract.ExecutionConfig{}, false) || cpuObservationEnabled("linux", contract.ExecutionConfig{Mode: contract.ExecutionIsolated, Backend: contract.BackendOCI}, false) {
		t.Fatal("unobservable backend enabled")
	}
}

// requiredReceive bounds every synchronization wait, including negative controls.
func requiredReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("required check synchronization timed out")
	}
	var zero T
	return zero
}

func TestRequiredChecksConcurrency(t *testing.T) {
	for _, tc := range []struct{ checks, cpus, want int }{{0, 0, 0}, {1, 0, 1}, {9, 2, 2}, {2, 99, 2}} {
		if got := requiredCheckParallelism(tc.checks, tc.cpus); got != tc.want {
			t.Fatalf("bound: %d != %d", got, tc.want)
		}
	}
	for _, bound := range []int{1, 2, 4} {
		t.Run(fmt.Sprint(bound), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan string, 4)
			releases := make(chan struct{})
			var active, maximum, calls atomic.Int32
			runner := &checkRunner{runCommand: func(ctx context.Context, command string, _ time.Duration, _, _ io.Writer) error {
				n := active.Add(1)
				defer active.Add(-1)
				calls.Add(1)
				for old := maximum.Load(); n > old; old = maximum.Load() {
					if maximum.CompareAndSwap(old, n) {
						break
					}
				}
				entered <- command
				select {
				case <-releases:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}}
			jobs := []requiredCheckJob{{"a", "a", time.Minute}, {"b", "b", time.Minute}, {"c", "c", time.Minute}, {"d", "d", time.Minute}}
			done := make(chan struct{})
			var outcomes []CheckRunOutcome
			go func() {
				defer close(done)
				runner.runRequiredChecks(ctx, jobs, bound, nil, func(o CheckRunOutcome, _ error) { outcomes = append(outcomes, o) })
			}()
			for i := 0; i < 4; i++ {
				name := requiredReceive(t, entered)
				if bound == 1 && name != jobs[i].name {
					t.Fatalf("serial dispatch: %s", name)
				}
				// Fill every available worker before releasing any: serial scheduling deadlocks here.
				if i == bound-1 || i >= bound {
					releases <- struct{}{}
				}
			}
			for i := 1; i < bound; i++ {
				releases <- struct{}{}
			}
			requiredReceive(t, done)
			if maximum.Load() != int32(bound) || calls.Load() != 4 || len(outcomes) != 4 {
				t.Fatalf("bound %d: maximum=%d calls=%d outcomes=%d", bound, maximum.Load(), calls.Load(), len(outcomes))
			}
		})
	}
	(&checkRunner{}).runRequiredChecks(t.Context(), nil, 0, nil, func(CheckRunOutcome, error) { t.Fatal("empty policy ran") })
}

func requiredPolicyRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := setupGitRepoForValidation(t)
	policy := "required_checks:\n"
	for _, name := range []string{"a", "b", "c"} {
		policy += fmt.Sprintf("  - name: %s\n    command: %s\n    timeout: 1m\n", name, name)
	}
	policy += "protected_paths:\n  - protected/**\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "parallel policy"}} {
		if out, err := gitfixture.Command(t, repo, args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	return repo, gitRevision(t, repo, "HEAD")
}

func TestRequiredChecksPolicyOrder(t *testing.T) { testRequiredChecksPolicyOrder(t, false) }
func TestRequiredChecksProgressSessionPolicyOrder(t *testing.T) {
	testRequiredChecksPolicyOrder(t, true)
}

func testRequiredChecksPolicyOrder(t *testing.T, withSession bool) {
	repo, base := requiredPolicyRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan string, 3)
	completed := make(chan string, 3)
	releases := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{}), "c": make(chan struct{})}
	events := make(chan progress.Event, 32)
	session := progress.New(ctx, 1, 0, func(e progress.Event) { events <- e }, make(chan time.Time), time.Now, func() {})
	<-events
	session.Next("validation")
	<-events
	<-events
	defer session.Finish("completed")
	var raw, visible []progress.Event
	var observing atomic.Bool
	type result struct {
		report *ValidationReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := Validate(ctx, repo, nil, ValidateOptions{BaseSHA: base, CIMode: true, requiredCheckWorkers: 3,
			runCommand: func(ctx context.Context, command string, _ time.Duration, out, _ io.Writer) error {
				entered <- command
				select {
				case <-releases[command]:
				case <-ctx.Done():
					return ctx.Err()
				}
				fmt.Fprint(out, "PRIVATE_OUTPUT")
				completed <- command
				if command == "a" || command == "b" {
					return errors.New("intentional " + command)
				}
				return nil
			}, OnProgress: func(e progress.Event) {
				if strings.HasPrefix(e.Check, "preparation:") {
					return
				}
				if observing.Swap(true) {
					t.Error("concurrent observer")
				}
				defer observing.Store(false)
				raw = append(raw, e)
				if withSession {
					session.Observe(e)
					visible = append(visible, <-events)
				}
			}})
		done <- result{report, err}
	}()
	for i := 0; i < 3; i++ {
		requiredReceive(t, entered)
	}
	for _, name := range []string{"c", "b", "a"} {
		close(releases[name])
		if got := requiredReceive(t, completed); got != name {
			t.Fatalf("completion %s", got)
		}
	}
	got := requiredReceive(t, done)
	if got.err == nil || got.report == nil || got.report.Passed || !strings.Contains(got.err.Error(), `check "a"`) {
		t.Fatalf("result %+v %v", got.report, got.err)
	}
	if len(raw) != 6 {
		t.Fatalf("raw events %+v", raw)
	}
	lastRow, lastDetail := -1, -1
	for i, name := range []string{"a", "b", "c"} {
		o := got.report.CheckOutcomes[i]
		if o.Name != name {
			t.Fatalf("outcome order %+v", got.report.CheckOutcomes)
		}
		row := strings.Index(got.report.Artifact.Body, "| `"+name+"`")
		detail := strings.Index(got.report.Artifact.Body, "### Output: `"+name+"`")
		if row <= lastRow || detail <= lastDetail {
			t.Fatal("unordered report")
		}
		lastRow, lastDetail = row, detail
		status := "FAIL"
		if name == "c" {
			status = "PASS"
		}
		for _, stream := range [][]progress.Event{visible, raw} {
			if len(stream) == 0 {
				continue
			}
			start, end := stream[2*i], stream[2*i+1]
			if start.Check != name || start.Status != "started" || end.Check != name || end.Status != status || end.StageElapsed != o.Duration {
				t.Fatalf("lifecycle %+v", stream)
			}
			if strings.Contains(fmt.Sprint(stream), "PRIVATE_OUTPUT") {
				t.Fatal("output leaked")
			}
		}
	}
}

func TestRequiredChecksCancellation(t *testing.T) {
	for _, mode := range []string{"running", "queued", "started", "nil-after-cancel"} {
		t.Run(mode, func(t *testing.T) {
			repo, base := requiredPolicyRepo(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan struct{}, 3)
			var active, calls atomic.Int32
			type result struct {
				report *ValidationReport
				err    error
			}
			done := make(chan result, 1)
			bound := 3
			if mode == "queued" {
				bound = 1
			}
			go func() {
				report, err := Validate(ctx, repo, nil, ValidateOptions{BaseSHA: base, CIMode: true, requiredCheckWorkers: bound,
					OnProgress: func(e progress.Event) {
						if mode == "started" && e.Check == "a" && e.Status == "started" {
							cancel()
						}
					},
					runCommand: func(ctx context.Context, _ string, _ time.Duration, _, _ io.Writer) error {
						active.Add(1)
						defer active.Add(-1)
						calls.Add(1)
						entered <- struct{}{}
						<-ctx.Done()
						if mode == "nil-after-cancel" {
							return nil
						}
						return ctx.Err()
					}})
				done <- result{report, err}
			}()
			if mode != "started" {
				for i := 0; i < bound; i++ {
					requiredReceive(t, entered)
				}
				cancel()
			}
			got := requiredReceive(t, done)
			if !errors.Is(got.err, context.Canceled) || got.report == nil || got.report.Passed || got.report.Artifact.Verdict != "FAIL" || got.report.Timings.Checks.Status != "FAIL" || len(got.report.CheckOutcomes) != 3 || active.Load() != 0 || calls.Load() != 3 {
				t.Fatalf("cancelled pass: %+v err=%v active=%d calls=%d", got.report, got.err, active.Load(), calls.Load())
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	count := 0
	runner := &checkRunner{runCommand: func(ctx context.Context, _ string, _ time.Duration, _, _ io.Writer) error { return ctx.Err() }}
	runner.runRequiredChecks(ctx, []requiredCheckJob{{"a", "a", time.Minute}}, 1, nil, func(o CheckRunOutcome, err error) {
		count++
		if o.Passed || !errors.Is(err, context.Canceled) {
			t.Error("cancelled success")
		}
	})
	if count != 1 {
		t.Fatal("missing cancelled outcome")
	}
}
