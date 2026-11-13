package validation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// TestMain points the SwarmOS build caches at a scratch directory so tests
// never write the real user cache.
func TestMain(m *testing.M) {
	ledgerRoot, ledgerErr := os.MkdirTemp("", "swarm-test-ledger-*")
	if ledgerErr != nil {
		panic(ledgerErr)
	}
	ledgerRoot, ledgerErr = filepath.EvalSymlinks(ledgerRoot)
	if ledgerErr != nil {
		panic(ledgerErr)
	}
	storageuse.UserCacheDir = func() (string, error) { return ledgerRoot, nil }
	checkLedger := func() error {
		control, err := storageuse.ControlRoot()
		return testutil.CheckStorageLedger(ledgerRoot, control, err)
	}
	ledgerExit := func(code int) {
		if err := checkLedger(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
		os.RemoveAll(ledgerRoot)
		os.Exit(code)
	}
	if err := checkLedger(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		ledgerExit(1)
	}

	if len(os.Args) > 3 && os.Args[1] == "internal-browser-exec" {
		if err := sandbox.ExecBrowserScope(os.Args[2], os.Args[3], os.Args[4:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			ledgerExit(1)
		}
		ledgerExit(0)
	}

	// Preserve the provisioned browser cache before test-only HOME/cache isolation.
	if os.Getenv("SWARM_TEST_EXECUTOR") == "1" && os.Getenv("PLAYWRIGHT_BROWSERS_PATH") == "" {
		for _, bind := range sandbox.HostToolchains() {
			if bind.Ecosystem == "node-browsers" && bind.EnvKey == "PLAYWRIGHT_BROWSERS_PATH" {
				os.Setenv("PLAYWRIGHT_BROWSERS_PATH", bind.EnvVal)
				break
			}
		}
	}
	dir, err := os.MkdirTemp("", "swarm-validation-cache-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		ledgerExit(1)
	}
	userCacheDir = func() (string, error) { return dir, nil }

	os.Setenv("XDG_CACHE_HOME", dir)
	os.Setenv("HOME", dir)
	code := m.Run()
	if err := checkLedger(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.RemoveAll(dir)
	ledgerExit(code)
}

// useCacheRoot changes a package-global hook; callers must remain serial.
func useCacheRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	previous := userCacheDir
	previousControl := storageuse.UserCacheDir
	userCacheDir = func() (string, error) { return root, nil }
	storageuse.UserCacheDir = userCacheDir
	t.Cleanup(func() {
		userCacheDir = previous
		storageuse.UserCacheDir = previousControl
	})
	return root
}

func checkEnvOutput(t *testing.T, output, key string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			return value
		}
	}
	t.Fatalf("%s missing from check output %q", key, output)
	return ""
}

func commitRepoChange(t *testing.T, repo, rel, contents string) string {
	t.Helper()
	full := filepath.Join(repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", rel}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	return gitRevision(t, repo, "HEAD")
}

func validatorCacheDirs(t *testing.T, cacheRoot string) map[string]string {
	t.Helper()
	dirs := map[string]string{}
	matches, _ := filepath.Glob(filepath.Join(cacheRoot, "swarm", "validate-build", "*", "*"))
	for _, dir := range matches {
		dirs[filepath.Base(dir)] = dir
	}
	return dirs
}

var cacheEnvKeys = map[string]string{"go": "GOCACHE", "rust": "CARGO_TARGET_DIR", "c-cpp": "CCACHE_DIR"}

func TestHostChecksUseRepositoryBuildCaches(t *testing.T) {
	userGo, userCC := filepath.Join(t.TempDir(), "user-gocache"), filepath.Join(t.TempDir(), "user-ccache")
	t.Setenv("GOCACHE", userGo)
	t.Setenv("CCACHE_DIR", userCC)
	repo := setupGitRepoForValidation(t)
	policy := "required_checks:\n  - name: env\n    command: 'for k in GOCACHE CARGO_TARGET_DIR CCACHE_DIR; do eval \"d=\\$$k\"; printf \"%s=%s\\n\" \"$k\" \"$d\"; touch \"$d/marker\"; done; printf \"HOME=%s\\n\" \"$HOME\"'\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n"
	base := commitPolicy(t, repo, policy)

	for _, runnerIsolated := range []bool{false, true} {
		cacheRoot := useCacheRoot(t)
		var runs []map[string]string
		for range 2 {
			report, err := Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, RunnerIsolated: runnerIsolated, BaseSHA: base})
			if err != nil || !report.Passed {
				t.Fatalf("runnerIsolated=%v: validate = %+v, %v", runnerIsolated, report, err)
			}
			assertValidationRepositoryCompleted(t, repo)
			out := report.CheckOutcomes[0].Output
			run := map[string]string{"HOME": checkEnvOutput(t, out, "HOME")}
			for _, key := range cacheEnvKeys {
				run[key] = checkEnvOutput(t, out, key)
			}
			runs = append(runs, run)
		}
		persistent := validatorCacheDirs(t, cacheRoot)
		if len(persistent) != len(cacheEnvKeys) {
			t.Fatalf("validator caches %v, want one per ecosystem under %s", persistent, cacheRoot)
		}
		for eco, key := range cacheEnvKeys {
			for _, run := range runs {
				// Host checks run candidate code, so they get scratch below the
				// per-run HOME and never write the persistent validator cache.
				if want := filepath.Join(run["HOME"], "build-cache", eco); run[key] != want {
					t.Fatalf("runnerIsolated=%v: %s=%q, want scratch %q", runnerIsolated, key, run[key], want)
				}
				if _, err := os.Stat(run["HOME"]); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("scratch HOME %q survived validation: %v", run["HOME"], err)
				}
			}
			if runs[0][key] == runs[1][key] {
				t.Fatalf("runnerIsolated=%v: candidate %s reused across runs: %q", runnerIsolated, key, runs[0][key])
			}
			if _, err := os.Stat(filepath.Join(persistent[eco], "marker")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("candidate host check wrote the persistent %s cache: %v", eco, err)
			}
		}
	}
	for _, user := range []string{userGo, userCC} {
		if _, err := os.Stat(user); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("user cache %s was touched: %v", user, err)
		}
	}
}

func warmupRunner(t *testing.T, repo string) *checkRunner {
	t.Helper()
	home := t.TempDir()
	caches, err := validatorBuildCaches(context.Background(), repo, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caches {
		if !c.Persistent {
			t.Fatalf("cache %+v is not persistent", c)
		}
	}
	return &checkRunner{worktree: repo, home: home, buildCaches: caches, execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}}
}

func TestWarmupRunsOncePerBase(t *testing.T) {
	useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	base := commitRepoChange(t, repo, "version", "base\n")
	// The candidate's working tree differs; warm-up must never see it.
	if err := os.WriteFile(filepath.Join(repo, "version"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := []contract.CheckSpec{{Name: "record", Command: "go test ./...", Timeout: "1m"}}
	runner := warmupRunner(t, repo)
	goCache := runner.buildCaches[0].Dir
	runner.runCommand = func(_ context.Context, _ string, _ time.Duration, _, _ io.Writer) error {
		entries, err := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "swarm-warmup-*", "base", "version"))
		if err != nil || len(entries) != 1 {
			return fmt.Errorf("base checkout: %v %v", entries, err)
		}
		data, err := os.ReadFile(entries[0])
		if err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(goCache, "warm-log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(data)
		return err
	}

	log := func() string {
		data, _ := os.ReadFile(filepath.Join(goCache, "warm-log"))
		return string(data)
	}
	timing := &PhaseTiming{}
	if note := warmBuildCaches(context.Background(), runner, checks, base, timing, nil); !strings.Contains(note, "Warmed the validator build cache") {
		t.Fatalf("first warm-up note = %q", note)
	}
	if timing.Status != "ran" || timing.Duration < 0 {
		t.Fatalf("timing: %+v", timing)
	}
	if log() != "base\n" {
		t.Fatalf("warm-up log = %q, want the base only", log())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(goCache), ".warm-"+base)); err != nil {
		t.Fatalf("warm marker missing: %v", err)
	}
	if note := warmBuildCaches(context.Background(), runner, checks, base, timing, nil); timing.Status != "skipped-marker" || note != "" || log() != "base\n" {
		t.Fatalf("second warm-up at the same base ran: %q, log %q", note, log())
	}
	if out, err := gitfixture.Command(t, "", "-C", repo, "stash", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git stash: %v (%s)", err, out)
	}
	next := commitRepoChange(t, repo, "version", "next\n")
	if note := warmBuildCaches(context.Background(), runner, checks, next, &PhaseTiming{}, nil); note == "" || log() != "base\nnext\n" {
		t.Fatalf("new base did not warm: %q, log %q", note, log())
	}
	out, err := gitfixture.Command(t, "", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil || strings.Count(string(out), "worktree ") != 1 {
		t.Fatalf("warm-up left a linked worktree: %s %v", out, err)
	}
}

func TestWarmupFailureDoesNotAffectCandidate(t *testing.T) {
	useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	base := commitRepoChange(t, repo, "version", "base\n")
	if err := os.WriteFile(filepath.Join(repo, "version"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := []contract.CheckSpec{{Name: "candidate-only", Command: "go test ./internal/cli", Timeout: "1m"}}
	runner := warmupRunner(t, repo)
	runner.runCommand = func(_ context.Context, command string, _ time.Duration, stdout, _ io.Writer) error {
		if command != checks[0].Command {
			return errors.New("injected base failure")
		}
		_, err := io.WriteString(stdout, "candidate output")
		return err
	}
	timing := &PhaseTiming{}
	note := warmBuildCaches(context.Background(), runner, checks, base, timing, nil)
	if timing.Status != "failed-checks" || timing.Duration < 0 || !strings.Contains(note, "`candidate-only` failed on the base") {
		t.Fatalf("warm-up failure not reported: %q", note)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(runner.buildCaches[0].Dir), ".warm-"+base)); err != nil {
		t.Fatalf("finished warm-up not recorded: %v", err)
	}
	if outcome, err := runner.runCheck(context.Background(), checks[0].Name, checks[0].Command, checkTimeout(checks[0].Timeout), nil); err != nil || !outcome.Passed {
		t.Fatalf("candidate check after a failed warm-up = %+v, %v", outcome, err)
	}
	// Compare candidate fields with and without warm-up failure. Timing is
	// intentionally excluded; warm-up diagnostics are separate report notes.
	for _, candidateFails := range []bool{false, true} {
		var control CheckRunOutcome
		for _, warmFails := range []bool{false, true} {
			runner.runCommand = func(_ context.Context, command string, _ time.Duration, stdout, _ io.Writer) error {
				if command != checks[0].Command {
					if warmFails {
						return errors.New("warm failure")
					}
					return nil
				}
				_, _ = io.WriteString(stdout, "candidate output")
				if candidateFails {
					return errors.New("candidate failure")
				}
				return nil
			}
			next := commitRepoChange(t, repo, "control", fmt.Sprintf("%v/%v", candidateFails, warmFails))
			warmBuildCaches(context.Background(), runner, checks, next, &PhaseTiming{}, nil)
			outcome, _ := runner.runCheck(context.Background(), checks[0].Name, checks[0].Command, time.Minute, nil)
			if !warmFails {
				control = outcome
				continue
			}
			if outcome.Passed != control.Passed || outcome.Outcome != control.Outcome || outcome.ExitCode != control.ExitCode || outcome.Command != control.Command || outcome.Output != control.Output {
				t.Fatalf("warm-up affected candidate: %+v vs %+v", outcome, control)
			}
		}
	}
	if note := warmBuildCaches(context.Background(), runner, nil, base, timing, nil); note != "" || timing.Status != "no-applicable-checks" {
		t.Fatalf("empty warm-up: %s %+v", note, timing)
	}
	if note := warmBuildCaches(context.Background(), runner, checks, "missing-base", timing, nil); note == "" || timing.Status != "skipped-error" {
		t.Fatalf("unavailable base: %s %+v", note, timing)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	warmBuildCaches(cancelled, runner, checks, "missing-base", timing, nil)
	if timing.Status != "cancelled" {
		t.Fatalf("cancelled warm-up: %+v", timing)
	}

}

func TestValidateFallsBackToScratchBuildCache(t *testing.T) {
	previous := userCacheDir
	userCacheDir = func() (string, error) { return "", errors.New("no cache") }
	t.Cleanup(func() { userCacheDir = previous })
	repo := setupGitRepoForValidation(t)
	policy := "required_checks:\n  - name: env\n    command: 'printf \"GOCACHE=%s\\nHOME=%s\\n\" \"$GOCACHE\" \"$HOME\"; touch \"$GOCACHE/marker\"'\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n"
	base := commitPolicy(t, repo, policy)
	report, err := Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: base})
	if err != nil || !report.Passed {
		t.Fatalf("validate without a user cache = %+v, %v", report, err)
	}
	out := report.CheckOutcomes[0].Output
	if cache, home := checkEnvOutput(t, out, "GOCACHE"), checkEnvOutput(t, out, "HOME"); cache != filepath.Join(home, "build-cache", "go") {
		t.Fatalf("GOCACHE %q is not scratch under %q", cache, home)
	}
}

func TestOCIChecksGetNoBuildCache(t *testing.T) {
	cacheRoot := useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	config := "version: 1\ndefault_agent: codex\ndefault_gate: agent\nexecution:\n  mode: isolated\n  backend: oci\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_isolation: true\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  codex:\n    binary: swarm-test-missing-codex\n"
	base := commitRepoChange(t, repo, ".swarm/config.yaml", config)
	// Duration history uses the coordinator cache on every backend. Check
	// the actual cache namespaces rather than treating any lookup as a build
	// cache: OCI must receive budgets without preparing candidate caches.
	runs := 0
	report, err := Validate(context.Background(), repo, nil, ValidateOptions{
		CIMode: true, BaseSHA: base,
		runCommand: func(context.Context, string, time.Duration, io.Writer, io.Writer) error {
			runs++
			return nil
		},
	})
	if err != nil || report == nil || !report.Passed || runs == 0 {
		t.Fatalf("OCI validation = %+v, %v; check runs = %d", report, err, runs)
	}
	for _, namespace := range []string{"validate-build", "validate-deps"} {
		if _, err := os.Lstat(filepath.Join(cacheRoot, "swarm", namespace)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("OCI validation prepared %s cache: %v", namespace, err)
		}
	}
	root, err := durationRoot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, outcome := range report.CheckOutcomes {
		if samples := readDurations(root, durationKey(outcome.Name, outcome.Command)).Samples; len(samples) != 1 {
			t.Fatalf("OCI passing duration samples = %v, want one", samples)
		}
	}
}

func TestCleanCheckEnvUsesOnlyValidatorCaches(t *testing.T) {
	cacheRoot := useCacheRoot(t)
	t.Setenv("GOCACHE", "/user/gocache")
	t.Setenv("CCACHE_DIR", "/user/ccache")
	t.Setenv("CARGO_TARGET_DIR", "/user/target")
	t.Setenv("GITHUB_TOKEN", "secret")
	repo := setupGitRepoForValidation(t)
	if _, err := sandbox.PrepareBuildCaches(context.Background(), sandbox.AgentBuildCache, repo, cacheRoot, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	caches, err := validatorBuildCaches(context.Background(), repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := cleanCheckEnv("/scratch/home", caches, sandbox.Dependencies{})
	for _, key := range cacheEnvKeys {
		var values []string
		for _, entry := range env {
			if value, ok := strings.CutPrefix(entry, key+"="); ok {
				values = append(values, value)
			}
		}
		want := filepath.Join(cacheRoot, "swarm", "validate-build")
		if len(values) != 1 || !strings.HasPrefix(values[0], want+string(filepath.Separator)) || strings.Contains(values[0], "agent-build") {
			t.Fatalf("%s entries = %q, want one under %s", key, values, want)
		}
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "GITHUB_TOKEN=") || strings.Contains(entry, "/user/") {
			t.Fatalf("host value leaked into check environment: %q", entry)
		}
	}
	for _, entry := range cleanCheckEnv("/scratch/home", nil, sandbox.Dependencies{}) {
		for _, key := range cacheEnvKeys {
			if strings.HasPrefix(entry, key+"=") {
				t.Fatalf("no caches still set %q", entry)
			}
		}
	}
}

func TestValidateWarmsBaseAndReusesReadOnlyOnBubblewrap(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for real Bubblewrap validation")
	}
	cacheRoot := useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	config := "version: 1\ndefault_agent: worker\ndefault_gate: none\nexecution:\n  mode: isolated\n  backend: native\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_isolation: true\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  worker:\n    binary: not-used-by-validation\n    harness: omp\n"
	commitRepoChange(t, repo, ".swarm/config.yaml", config)
	commitRepoChange(t, repo, "go.mod", "module fixture\ngo 1.24\n")
	commitRepoChange(t, repo, "cache_test.go", `package fixture
import ("os"; "path/filepath"; "testing")
func TestCache(t *testing.T) {
 entries, err := os.ReadDir(os.Getenv("GOCACHE"))
 if err != nil || len(entries) < 2 { t.Fatal("missing compiled entries", err) }
 if err := os.WriteFile(filepath.Join(os.Getenv("GOCACHE"), "candidate-write"), []byte("candidate"), 0600); err != nil { t.Fatal(err) }
}
`)
	policy := "required_checks:\n  - name: cache\n    command: 'go test -v ./...'\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n"
	base := commitPolicy(t, repo, policy)
	if !sandbox.NativeValidationReusesCache(&contract.SwarmConfig{DefaultAgent: "worker", Agents: map[string]contract.AgentConfig{"worker": {Harness: "omp"}}}) {
		t.Skip("Bubblewrap overlay mounts are unavailable here")
	}
	for run := range 2 {
		report, err := Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: base})
		if err != nil || !report.Passed {
			t.Fatalf("run %d: validate = %+v, %v", run, report, err)
		}
		warmed := strings.Contains(report.Artifact.Body, "Warmed the validator build cache")
		if warmed != (run == 0) {
			t.Fatalf("run %d: warmed=%v\n%s", run, warmed, report.Artifact.Body)
		}
		cache := validatorCacheDirs(t, cacheRoot)["go"]
		entries, err := os.ReadDir(cache)
		if err != nil || len(entries) < 2 {
			t.Fatalf("compiled cache entries: %v %v", entries, err)
		}
		if _, err := os.Stat(filepath.Join(cache, "candidate-write")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("candidate write persisted: %v", err)
		}
	}
}

// pendingValidationUses reads durable evidence rather than inferring completion
// from a maintenance denial (which can also mean legacy coverage).
func pendingValidationUses(t *testing.T) int { return pendingValidationRepositoryUses(t, "") }
func pendingValidationRepositoryUses(t *testing.T, repository string) int {
	t.Helper()
	root, err := storageuse.ControlRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	pending := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Uses     map[string]bool
			Resource storageuse.Resource
		}
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		if repository != "" && record.Resource.Repository != repository {
			continue
		}
		for _, done := range record.Uses {
			if !done {
				pending++
			}
		}
	}
	return pending
}

func TestValidationHostUseCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, command       string
		cancel, stopFailure bool
	}{
		{name: "success", command: "true"}, {name: "nonzero", command: "exit 7"},
		{name: "cancel-verified", command: "true", cancel: true},
		{name: "unverified-stop", command: "true", stopFailure: true},
		{name: "cancel-unverified", command: "true", cancel: true, stopFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateValidationControl(t)
			home := t.TempDir()
			cache := filepath.Join(t.TempDir(), "go")
			if err := os.Mkdir(cache, 0700); err != nil {
				t.Fatal(err)
			}
			r := &checkRunner{home: home, worktree: home, buildCaches: []sandbox.BuildCache{{Ecosystem: "go", EnvKey: "GOCACHE", Dir: cache, Persistent: true}}, execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			old := stopValidationProcess
			if tc.stopFailure {
				stopValidationProcess = func(cmd *exec.Cmd) error {
					_ = process.StopVerified(cmd)
					return errors.New("injected unverified stop")
				}
			}
			defer func() { stopValidationProcess = old }()
			outcome, err := r.runCheck(ctx, "host", tc.command, time.Second, func(event progress.Event) {
				if tc.cancel && event.Status == "started" {
					cancel()
				}
			})
			if tc.stopFailure && err == nil {
				t.Fatal("stop failure swallowed")
			}
			if tc.command == "exit 7" && (err == nil || outcome.Passed) {
				t.Fatal("nonzero check passed")
			}
			pending := pendingValidationUses(t)
			if tc.stopFailure && pending == 0 {
				t.Fatal("unverified stop erased pending use")
			}
			if !tc.stopFailure && pending != 0 {
				t.Fatalf("verified completion left %d pending uses", pending)
			}
		})
	}
}

func TestValidationWarmupMarkerUseCompletion(t *testing.T) {
	isolateValidationControl(t)
	useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	base := commitRepoChange(t, repo, "version", "base\n")
	ctx, owner, err := storageuse.Ensure(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	caches, err := validatorBuildCaches(ctx, repo, home)
	if err != nil {
		t.Fatal(err)
	}
	runner := &checkRunner{worktree: repo, home: home, buildCaches: caches, execution: contract.ExecutionConfig{Mode: contract.ExecutionTrustedLocal}}
	runner.runCommand = func(context.Context, string, time.Duration, io.Writer, io.Writer) error { return nil }
	checks := []contract.CheckSpec{{Name: "warm", Command: "go build ./...", Timeout: "1m"}}
	for _, want := range []string{"ran", "skipped-marker"} {
		timing := &PhaseTiming{}
		warmBuildCaches(ctx, runner, checks, base, timing, nil)
		if timing.Status != want {
			t.Fatalf("warmup=%s, want %s", timing.Status, want)
		}
	}
	if pendingValidationUses(t) == 0 {
		t.Fatal("outer observer no longer protected")
	}
	if err := owner.Finish(); err != nil {
		t.Fatal(err)
	}
	if pending := pendingValidationUses(t); pending != 0 {
		t.Fatalf("warmup and marker hit left %d pending records", pending)
	}
}

func assertValidationRepositoryCompleted(t *testing.T, repo string) {
	t.Helper()
	ctx, owner, err := storageuse.Ensure(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Finish()
	if pending := pendingValidationRepositoryUses(t, storageuse.FromContext(ctx).Repository()); pending != 0 {
		t.Fatalf("completed validation/diagnosis left %d pending uses", pending)
	}
}

func TestValidationObserverRetainsUseUntilReturn(t *testing.T) {
	isolateValidationControl(t)
	cacheRoot := useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	base := commitPolicy(t, repo, "required_checks:\n  - name: observe\n    command: 'true'\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n")
	held := false
	report, err := Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: base, OnProgress: func(event progress.Event) {
		if event.Check != "observe" || event.Status != "PASS" {
			return
		}
		held = true
		dirs, err := filepath.Glob(filepath.Join(cacheRoot, "swarm", "validate-build", "*", "go"))
		if err != nil || len(dirs) != 1 {
			t.Fatalf("caches=%v err=%v", dirs, err)
		}
		root, err := storageuse.ControlRoot()
		if err != nil {
			t.Fatal(err)
		}
		manager, err := storageuse.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close()
		resource := storageuse.ResourceFor(filepath.Base(filepath.Dir(dirs[0])), "validate-build", dirs[0])
		guard, err := manager.AcquireMaintenance(context.Background(), resource)
		if err == nil {
			guard.Close()
			t.Fatal("maintenance admitted during coordinator observer")
		}
		if !errors.Is(err, storageuse.ErrUnsafe) {
			t.Fatal(err)
		}
	}})
	if err != nil || report == nil || !report.Passed || !held {
		t.Fatalf("validation=%+v err=%v observer=%v", report, err, held)
	}
	if pending := pendingValidationUses(t); pending != 0 {
		t.Fatalf("completed validation left %d pending uses", pending)
	}
}

func isolateValidationControl(t *testing.T) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", root)
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", root)
	}
}

func TestStorageControlRootFollowsProducerCacheHook(t *testing.T) {
	sentinel := t.TempDir()
	t.Setenv("HOME", sentinel)
	t.Setenv("XDG_CACHE_HOME", sentinel)
	cache := useCacheRoot(t)
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture\ngo 1.24\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, owner, err := storageuse.Ensure(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = validatorBuildCaches(ctx, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err = owner.Finish(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(sentinel)
	if err != nil || len(entries) != 0 {
		t.Fatalf("real cache/home sentinel touched: %v %v", entries, err)
	}
	entries, err = os.ReadDir(filepath.Join(cache, "swarm-use"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("producer did not write isolated evidence: %v %v", entries, err)
	}
}

// Exercise the shared scratch producer used by coordinator workflows after a
// test changes user directories; the package resolver must remain fixed.
func TestStorageLedgerProducerIgnoresEnvironment(t *testing.T) {
	control, err := storageuse.ControlRoot()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("fixture override restores package resolver", func(t *testing.T) { useCacheRoot(t) })
	restoredControl, restoreErr := storageuse.ControlRoot()
	if restoreErr != nil || restoredControl != control {
		t.Fatal("fixture failed to restore package resolver", restoredControl, restoreErr)
	}
	decoy := t.TempDir()
	t.Setenv("HOME", decoy)
	t.Setenv("XDG_CACHE_HOME", decoy)
	ctx, scope, err := storageuse.Ensure(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Finish()
	path, err := scope.TempDir(ctx, t.TempDir(), "producer-*", "review-scratch")
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.Finish(); err != nil {
		t.Fatal(err)
	}
	resource := storageuse.ResourceFor(scope.Repository(), "review-scratch", path)
	observation, err := storageuse.Observe(ctx, control, resource, 16<<10, func(int64) error { return nil })
	if err != nil || !observation.CompleteCoverage || observation.Active == nil || *observation.Active {
		t.Fatalf("missing finished producer record: %+v %v", observation, err)
	}
	for _, candidate := range []string{filepath.Join(decoy, "swarm-use"), filepath.Join(decoy, "Library", "Caches", "swarm-use")} {
		if _, err := os.Stat(candidate); !os.IsNotExist(err) {
			t.Fatalf("producer wrote decoy ledger: %s %v", candidate, err)
		}
	}
	restored, err := storageuse.ControlRoot()
	if err != nil || restored != control {
		t.Fatal("resolver moved", restored, err)
	}
}

func TestHostValidationBoundaryEvidenceIsOutside(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process identities")
	}
	root := t.TempDir()
	previous := storageuse.UserCacheDir
	storageuse.UserCacheDir = func() (string, error) { return root, nil }
	t.Cleanup(func() { storageuse.UserCacheDir = previous })
	ctx, owner, err := storageuse.Ensure(context.Background(), strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Finish()
	scope := storageuse.FromContext(ctx)
	resource := storageuse.ResourceFor(scope.Repository(), "validate-build", filepath.Join(root, "cache"))
	if err := scope.AcquirePrepared(ctx, resource); err != nil {
		t.Fatal(err)
	}
	runner := checkRunner{worktree: t.TempDir(), home: t.TempDir()}
	var output strings.Builder
	if err := runner.runOnHost(ctx, "true", &output, &output); err != nil {
		t.Fatal(err)
	}
	records, _ := filepath.Glob(filepath.Join(root, "swarm-use", "*.json"))
	if len(records) != 1 {
		t.Fatal(records)
	}
	data, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Evidence map[string]struct{ Outside bool }
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Evidence) != 1 {
		t.Fatal("evidence missing")
	}
	for _, e := range v.Evidence {
		if !e.Outside {
			t.Fatal("host command retained qualifying evidence")
		}
	}
	scope.Retain()
}

func TestRequiredCheckDurationHistoryStorage(t *testing.T) {
	useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	c := contract.CheckSpec{Name: "history", Command: "true", Timeout: "1ms"}
	root, err := durationRoot(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	key := durationKey(c.Name, c.Command)
	for _, raw := range []string{`{}`, `garbage`, `{"version":1,"samples":[-1]}`, `{"version":1,"samples":[9223372036854775808]}`, `{"version":2,"samples":[10000000000]}`} {
		if err := root.WriteFile(key+".json", []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if got := requiredCheckBudget(t.Context(), repo, c); got != time.Millisecond {
			t.Fatalf("accepted %s: %v", raw, got)
		}
	}
	// Failed runs cannot become training samples.
	recordRequiredDurations(t.Context(), repo, []CheckRunOutcome{{Name: c.Name, Command: c.Command, Duration: time.Second}})
	if got := requiredCheckBudget(t.Context(), repo, c); got != time.Millisecond {
		t.Fatalf("invalid training: %v", got)
	}
	recordRequiredDurations(t.Context(), repo, []CheckRunOutcome{{Name: c.Name, Command: c.Command, Passed: true, Duration: time.Second}})
	linked := filepath.Join(t.TempDir(), "linked")
	if out, err := gitfixture.Command(t, "", "-C", repo, "worktree", "add", "--detach", linked, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	if got := requiredCheckBudget(t.Context(), linked, c); got != 2*time.Second {
		t.Fatalf("linked identity: %v", got)
	}
	other := setupGitRepoForValidation(t)
	if got := requiredCheckBudget(t.Context(), other, c); got != time.Millisecond {
		t.Fatalf("repository isolation: %v", got)
	}
	for i := 0; i < 30; i++ {
		recordRequiredDurations(t.Context(), repo, []CheckRunOutcome{{Name: c.Name, Command: c.Command, Passed: true, Duration: time.Second}})
	}
	if h := readDurations(root, key); len(h.Samples) != 20 {
		t.Fatalf("unbounded history: %+v", h)
	}
	// Root-bound writes reject a symlink in either the record or lock position.
	cache, _ := userCacheDir()
	matches, _ := filepath.Glob(filepath.Join(cache, "swarm", "validate-duration", "*", key+".json"))
	if len(matches) != 1 {
		t.Fatal(matches)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(matches[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, matches[0]); err != nil {
		t.Fatal(err)
	}
	recordRequiredDurations(t.Context(), repo, []CheckRunOutcome{{Name: c.Name, Command: c.Command, Passed: true, Duration: time.Minute}})
	if raw, _ := os.ReadFile(target); string(raw) != "untouched" {
		t.Fatal("symlink write escaped")
	}
	if got := requiredCheckBudget(t.Context(), repo, c); got != time.Millisecond {
		t.Fatalf("followed history link: %v", got)
	}
}

func TestRequiredCheckDurationHistoryConcurrent(t *testing.T) {
	useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	c := CheckRunOutcome{Name: "concurrent", Command: "true", Passed: true, Duration: time.Second}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); recordRequiredDurations(t.Context(), repo, []CheckRunOutcome{c}) }()
	}
	wg.Wait()
	root, err := durationRoot(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if h := readDurations(root, durationKey(c.Name, c.Command)); len(h.Samples) != 12 {
		t.Fatalf("lost updates: %+v", h)
	}
}

func TestRequiredCheckDurationHistoryCompleteRunsOnly(t *testing.T) {
	useCacheRoot(t)
	for _, kind := range []string{"failed", "cancelled", "passed"} {
		t.Run(kind, func(t *testing.T) {
			repo := setupGitRepoForValidation(t)
			last := "exit 1"
			if kind == "passed" {
				last = "true"
			}
			base := commitRepoChange(t, repo, ".swarm/validation/required-checks.yaml", "required_checks:\n  - name: first\n    command: true\n    timeout: 1m\n  - name: last\n    command: "+last+"\n    timeout: 1m\nprotected_paths: [protected/**]\n")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			report, runErr := Validate(ctx, repo, nil, ValidateOptions{BaseSHA: base, OnProgress: func(e progress.Event) {
				if kind == "cancelled" && e.Check == "last" && e.Status == "started" {
					cancel()
				}
			}})
			if report == nil {
				t.Fatalf("missing report: %v", runErr)
			}
			root, err := durationRoot(t.Context(), repo)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			samples := readDurations(root, durationKey("first", "true")).Samples
			if (len(samples) > 0) != (kind == "passed") {
				t.Fatalf("%s samples: %v", kind, samples)
			}
			if strings.Contains(report.Artifact.Body, "validate-duration") || strings.Contains(report.Artifact.Metadata["validation_timing_report"], "samples") {
				t.Fatal("history projected")
			}
		})
	}
}

func TestRequiredCheckDurationHistoryRejectsWorktreeCache(t *testing.T) {
	useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	// Coordinator configuration must never turn a candidate directory into state.
	userCacheDir = func() (string, error) { return repo, nil }
	if r, err := durationRoot(t.Context(), repo); err == nil {
		r.Close()
		t.Fatal("history inside worktree admitted")
	}
}

func TestRequiredCheckDurationHistoryUnsafeLayout(t *testing.T) {
	cache := useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	c := contract.CheckSpec{Name: "layout", Command: "true", Timeout: "1ms"}
	recordRequiredDurations(t.Context(), repo, []CheckRunOutcome{{Name: c.Name, Command: c.Command, Passed: true, Duration: time.Second}})
	records, _ := filepath.Glob(filepath.Join(cache, "swarm", "validate-duration", "*", durationKey(c.Name, c.Command)+".json"))
	if len(records) != 1 {
		t.Fatal(records)
	}
	record := records[0]
	if runtime.GOOS == "linux" {
		if err := os.Remove(record); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("mkfifo", record).CombinedOutput(); err != nil {
			t.Fatalf("mkfifo: %s %v", out, err)
		}
		if got := requiredCheckBudget(t.Context(), repo, c); got != time.Millisecond {
			t.Fatalf("special file read %v", got)
		}
		recordRequiredDurations(t.Context(), repo, []CheckRunOutcome{{Name: c.Name, Command: c.Command, Passed: true, Duration: time.Second}})
		if info, err := os.Lstat(record); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatal("special file was replaced")
		}
	}
	target := t.TempDir()
	parent := filepath.Join(cache, "swarm", "validate-duration")
	if err := os.Rename(parent, parent+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, parent); err != nil {
		t.Fatal(err)
	}
	recordRequiredDurations(t.Context(), repo, []CheckRunOutcome{{Name: c.Name, Command: c.Command, Passed: true, Duration: time.Second}})
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatalf("history followed parent link: %v %v", entries, err)
	}
	if got := requiredCheckBudget(t.Context(), repo, c); got != time.Millisecond {
		t.Fatalf("unsafe history budget %v", got)
	}
}

func TestWarmupCommand(t *testing.T) {
	for _, command := range []string{
		"go test -timeout=15m ./internal/cli",
		"go test -race -tags unit ./internal/cli ./internal/contract",
		"go test -run '^$' -bench Benchmark -fuzz Fuzz ./...",
		"go test -run TestFoo -run=TestBar -count 1 ./...",
	} {
		got, ok := warmupCommand(command)
		if !ok || strings.Count(got, "-run") != 1 || !strings.Contains(got, "-run '^$'") || strings.Contains(got, "-bench") || strings.Contains(got, "-fuzz") {
			t.Errorf("%q => %q, %v", command, got, ok)
		}
	}
	for _, command := range []string{"go build -race ./...", "go vet ./..."} {
		if got, ok := warmupCommand(command); !ok || got != command {
			t.Errorf("%q => %q %v", command, got, ok)
		}
	}
}

func TestWarmupSkipsUnsupportedCommands(t *testing.T) {
	useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	base := commitRepoChange(t, repo, "version", "base")
	runner := warmupRunner(t, repo)
	var commands []string
	runner.runCommand = func(_ context.Context, command string, _ time.Duration, _, _ io.Writer) error {
		commands = append(commands, command)
		return nil
	}
	var checks []contract.CheckSpec
	for _, command := range []string{
		"go test ./... | cat", "go test ./... && true", "go test $(echo ./...)", "go test ./... > output",
		"go test ./...\ntrue", "env X=1 go test ./...", "true", "go tool staticcheck ./...",
		"go test -exec wrapper ./...", "go test -toolexec wrapper ./...", "go test -c ./...",
		"go test -o binary ./...", "go test -json ./...", "go test ./... -args -test.run TestFoo",
	} {
		if got, ok := warmupCommand(command); ok {
			t.Fatalf("accepted %q as %q", command, got)
		}
		checks = append(checks, contract.CheckSpec{Name: "unsupported", Command: command})
	}
	timing := &PhaseTiming{}
	warmBuildCaches(context.Background(), runner, checks, base, timing, nil)
	if len(commands) != 0 || timing.Status != "no-applicable-checks" {
		t.Fatalf("commands=%v timing=%+v", commands, timing)
	}
	checks = append(checks, contract.CheckSpec{Name: "supported", Command: "go test -timeout=15m ./internal/cli"})
	original := checks[len(checks)-1]
	warmBuildCaches(context.Background(), runner, checks, base, timing, nil)
	if len(commands) != 1 || commands[0] == original.Command {
		t.Fatalf("warm commands=%v", commands)
	}
	_, err := runner.runCheck(context.Background(), original.Name, original.Command, time.Minute, nil)
	if err != nil || len(commands) != 2 || commands[1] != original.Command || checks[len(checks)-1] != original {
		t.Fatalf("candidate changed: %v %v", commands, err)
	}
}

func TestWarmupCompileOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "cli"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, source := range map[string]string{
		"go.mod": "module fixture\ngo 1.24\n",
		"internal/cli/cli_test.go": `package cli
import ("os"; "testing")
func TestSentinel(t *testing.T) { if err := os.WriteFile("sentinel", []byte("ran"), 0600); err != nil { t.Fatal(err) } }
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	original := "go test -v -timeout=15m ./internal/cli"
	command, ok := warmupCommand(original)
	if !ok {
		t.Fatal("not recognized")
	}
	run := func(command string) string {
		cmd := exec.Command("sh", "-c", command)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %s %v", command, out, err)
		}
		return string(out)
	}
	if out := run(command); strings.Contains(out, "=== RUN") {
		t.Fatal(out)
	}
	sentinel := filepath.Join(dir, "internal", "cli", "sentinel")
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test ran: %v", err)
	}
	if out := run(original); !strings.Contains(out, "=== RUN") {
		t.Fatal(out)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal(err)
	}
}
