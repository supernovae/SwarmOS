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
	"sync"
	"testing"
)

// TestMain points the SwarmOS build cache at a scratch directory so tests
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
	root := t.TempDir()
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

func TestHostChecksUseRepositoryGoCache(t *testing.T) {
	cacheRoot := useCacheRoot(t)
	userCache := filepath.Join(t.TempDir(), "user-gocache")
	t.Setenv("GOCACHE", userCache)
	repo := setupGitRepoForValidation(t)
	policy := "required_checks:\n  - name: env\n    command: 'printf \"GOCACHE=%s\\nHOME=%s\\n\" \"$GOCACHE\" \"$HOME\"; touch \"$GOCACHE/marker\"'\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n"
	base := commitPolicy(t, repo, policy)

	for _, runnerIsolated := range []bool{false, true} {
		var caches, homes []string
		for range 2 {
			report, err := Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, RunnerIsolated: runnerIsolated, BaseSHA: base})
			if err != nil || !report.Passed {
				t.Fatalf("runnerIsolated=%v: validate = %+v, %v", runnerIsolated, report, err)
			}
			out := report.CheckOutcomes[0].Output
			caches = append(caches, checkEnvOutput(t, out, "GOCACHE"))
			homes = append(homes, checkEnvOutput(t, out, "HOME"))
		}
		if caches[0] != caches[1] {
			t.Fatalf("runnerIsolated=%v: cache changed across runs: %q != %q", runnerIsolated, caches[0], caches[1])
		}
		if homes[0] == homes[1] {
			t.Fatalf("runnerIsolated=%v: expected distinct scratch homes, got %q", runnerIsolated, homes[0])
		}
		cache := caches[0]
		if !filepath.IsAbs(cache) || filepath.Dir(cache) != filepath.Join(cacheRoot, "swarm", "go-build") {
			t.Fatalf("cache %q is not under %q", cache, filepath.Join(cacheRoot, "swarm", "go-build"))
		}
		if cache == userCache {
			t.Fatalf("check used the user's GOCACHE %q", userCache)
		}
		for _, home := range homes {
			if insideDir(home, cache) {
				t.Fatalf("cache %q is inside scratch HOME %q", cache, home)
			}
			if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch HOME %q survived validation: %v", home, err)
			}
		}
		if _, err := os.Stat(filepath.Join(cache, "marker")); err != nil {
			t.Fatalf("cache content did not persist: %v", err)
		}
		if info, err := os.Stat(cache); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
			t.Fatalf("cache directory is not private: %v, %v", info, err)
		}
	}
	if _, err := os.Stat(userCache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("user GOCACHE was touched: %v", err)
	}
}

func TestHostGoCacheIsPerRepository(t *testing.T) {
	useCacheRoot(t)
	repo := setupGitRepoForValidation(t)
	linked := filepath.Join(t.TempDir(), "linked")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", "issue-1", linked).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v (%s)", err, out)
	}
	other := setupGitRepoForValidation(t)

	main, err := hostGoCache(repo)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := hostGoCache(linked)
	if err != nil {
		t.Fatal(err)
	}
	separate, err := hostGoCache(other)
	if err != nil {
		t.Fatal(err)
	}
	if main != shared {
		t.Fatalf("linked worktree cache %q differs from %q", shared, main)
	}
	if main == separate {
		t.Fatalf("unrelated repositories share cache %q", main)
	}
	if strings.Contains(main, filepath.Base(repo)) {
		t.Fatalf("cache path %q exposes the repository location", main)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			got, err := hostGoCache(repo)
			if err == nil && got != main {
				err = fmt.Errorf("concurrent cache %q != %q", got, main)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostGoCacheRejectsUnsafeLocations(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	tests := []struct {
		name  string
		setup func(t *testing.T) func() (string, error)
	}{
		{"no user cache directory", func(t *testing.T) func() (string, error) {
			return func() (string, error) { return "", errors.New("no cache") }
		}},
		{"relative user cache directory", func(t *testing.T) func() (string, error) {
			return func() (string, error) { return "cache", nil }
		}},
		{"inside worktree", func(t *testing.T) func() (string, error) {
			return func() (string, error) { return filepath.Join(repo, "cache"), nil }
		}},
		{"symlinked swarm directory", func(t *testing.T) func() (string, error) {
			root := t.TempDir()
			if err := os.Symlink(t.TempDir(), filepath.Join(root, "swarm")); err != nil {
				t.Skip("symlinks unavailable:", err)
			}
			return func() (string, error) { return root, nil }
		}},
		{"special file in cache path", func(t *testing.T) func() (string, error) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "swarm"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "swarm", "go-build"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return func() (string, error) { return root, nil }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previous := userCacheDir
			userCacheDir = tt.setup(t)
			t.Cleanup(func() { userCacheDir = previous })
			if dir, err := hostGoCache(repo); !errors.Is(err, ErrGoCacheUnavailable) {
				t.Fatalf("hostGoCache = %q, %v; want ErrGoCacheUnavailable", dir, err)
			}
			base := gitRevision(t, repo, "HEAD")
			if _, err := Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: base}); !errors.Is(err, ErrGoCacheUnavailable) {
				t.Fatalf("host validation error = %v; want ErrGoCacheUnavailable", err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(repo, "cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache was created inside the worktree: %v", err)
	}
}

func TestHostGoCacheRequiresRepository(t *testing.T) {
	useCacheRoot(t)
	if dir, err := hostGoCache(t.TempDir()); !errors.Is(err, ErrGoCacheUnavailable) {
		t.Fatalf("hostGoCache outside Git = %q, %v; want ErrGoCacheUnavailable", dir, err)
	}
}

func TestSandboxedChecksDoNotResolveHostGoCache(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	config := "version: 1\ndefault_agent: codex\ndefault_gate: agent\nexecution:\n  mode: isolated\n  backend: native\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_isolation: true\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  codex:\n    binary: swarm-test-missing-codex\n"
	if err := os.WriteFile(filepath.Join(repo, ".swarm", "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "native isolation policy"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=T", "-c", "user.email=t@localhost"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	previous := userCacheDir
	resolved := false
	userCacheDir = func() (string, error) { resolved = true; return "", errors.New("must not resolve") }
	t.Cleanup(func() { userCacheDir = previous })
	_, err := Validate(context.Background(), repo, nil, ValidateOptions{CIMode: true, BaseSHA: gitRevision(t, repo, "HEAD")})
	if resolved || errors.Is(err, ErrGoCacheUnavailable) {
		t.Fatalf("sandboxed validation resolved the host cache: %v", err)
	}
}

func TestCleanCheckEnvSetsOnlySwarmGoCache(t *testing.T) {
	t.Setenv("GOCACHE", "/user/gocache")
	t.Setenv("GITHUB_TOKEN", "secret")
	env := cleanCheckEnv("/scratch/home", "/cache/swarm/go-build/key")
	var caches []string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "GOCACHE="); ok {
			caches = append(caches, value)
		}
		if strings.HasPrefix(entry, "GITHUB_TOKEN=") {
			t.Fatalf("credential leaked into check environment: %q", entry)
		}
	}
	if len(caches) != 1 || caches[0] != "/cache/swarm/go-build/key" {
		t.Fatalf("GOCACHE entries = %q", caches)
	}
	for _, entry := range cleanCheckEnv("/scratch/home", "") {
		if strings.HasPrefix(entry, "GOCACHE=") {
			t.Fatalf("empty cache still set %q", entry)
		}
	}
}
