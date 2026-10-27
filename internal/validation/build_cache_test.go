package validation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// TestMain points the SwarmOS build caches at a scratch directory so tests
// never write the real user cache.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "swarm-validation-cache-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	userCacheDir = func() (string, error) { return dir, nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func useCacheRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	previous := userCacheDir
	userCacheDir = func() (string, error) { return root, nil }
	t.Cleanup(func() { userCacheDir = previous })
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
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
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
	checks := []contract.CheckSpec{{Name: "record", Command: `cat version >> "$GOCACHE/warm-log"`, Timeout: "1m"}}
	runner := warmupRunner(t, repo)
	goCache := runner.buildCaches[0].Dir
	log := func() string {
		data, _ := os.ReadFile(filepath.Join(goCache, "warm-log"))
		return string(data)
	}
	if note := warmBuildCaches(context.Background(), runner, checks, base); !strings.Contains(note, "Warmed the validator build cache") {
		t.Fatalf("first warm-up note = %q", note)
	}
	if log() != "base\n" {
		t.Fatalf("warm-up log = %q, want the base only", log())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(goCache), ".warm-"+base)); err != nil {
		t.Fatalf("warm marker missing: %v", err)
	}
	if note := warmBuildCaches(context.Background(), runner, checks, base); note != "" || log() != "base\n" {
		t.Fatalf("second warm-up at the same base ran: %q, log %q", note, log())
	}
	if out, err := exec.Command("git", "-C", repo, "stash", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git stash: %v (%s)", err, out)
	}
	next := commitRepoChange(t, repo, "version", "next\n")
	if note := warmBuildCaches(context.Background(), runner, checks, next); note == "" || log() != "base\nnext\n" {
		t.Fatalf("new base did not warm: %q, log %q", note, log())
	}
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
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
	checks := []contract.CheckSpec{{Name: "candidate-only", Command: `test "$(cat version)" = candidate`, Timeout: "1m"}}
	runner := warmupRunner(t, repo)
	note := warmBuildCaches(context.Background(), runner, checks, base)
	if !strings.Contains(note, "`candidate-only` failed on the base") {
		t.Fatalf("warm-up failure not reported: %q", note)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(runner.buildCaches[0].Dir), ".warm-"+base)); err != nil {
		t.Fatalf("finished warm-up not recorded: %v", err)
	}
	if outcome, err := runner.runCheck(context.Background(), checks[0].Name, checks[0].Command, checkTimeout(checks[0].Timeout), nil); err != nil || !outcome.Passed {
		t.Fatalf("candidate check after a failed warm-up = %+v, %v", outcome, err)
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
	repo := setupGitRepoForValidation(t)
	config := "version: 1\ndefault_agent: codex\ndefault_gate: agent\nexecution:\n  mode: isolated\n  backend: oci\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_isolation: true\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  codex:\n    binary: swarm-test-missing-codex\n"
	base := commitRepoChange(t, repo, ".swarm/config.yaml", config)
	previous := userCacheDir
	resolved := false
	userCacheDir = func() (string, error) { resolved = true; return "", errors.New("must not resolve") }
	t.Cleanup(func() { userCacheDir = previous })
	_, _ = Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: base})
	if resolved {
		t.Fatal("OCI validation resolved a build cache")
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
	env := cleanCheckEnv("/scratch/home", caches)
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
	for _, entry := range cleanCheckEnv("/scratch/home", nil) {
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
	commitRepoChange(t, repo, "version", "base\n")
	policy := "required_checks:\n  - name: cache\n    command: 'cat \"$GOCACHE/seen\" 2>/dev/null || true; cat version >> \"$GOCACHE/seen\"'\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n"
	base := commitPolicy(t, repo, policy)
	commitRepoChange(t, repo, "version", "candidate\n")
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
		// The candidate reads the trusted warm-up's entry...
		if out := report.CheckOutcomes[0].Output; !strings.Contains(out, "base") {
			t.Fatalf("run %d: candidate did not reuse the warm cache: %q", run, out)
		}
		// ...and its own write is discarded.
		if data, err := os.ReadFile(filepath.Join(validatorCacheDirs(t, cacheRoot)["go"], "seen")); err != nil || string(data) != "base\n" {
			t.Fatalf("run %d: persistent cache = %q, %v", run, data, err)
		}
	}
}
