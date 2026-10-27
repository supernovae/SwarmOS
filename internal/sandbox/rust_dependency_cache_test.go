package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func requireCargo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo is not on PATH")
	}
}

// sentinelBuildScript writes a build.rs that records it ran at sentinel.
func sentinelBuildScript(sentinel string) string {
	return "fn main() { std::fs::write(" + strconv.Quote(sentinel) + ", \"ran\").unwrap(); }\n"
}

func crateManifest(name, extra string) string {
	return "[package]\nname = \"" + name + "\"\nversion = \"0.1.0\"\nedition = \"2021\"\n" + extra
}

// gitCrate commits a crate named name, whose build script writes sentinel,
// and returns its file:// URL.
func gitCrate(t *testing.T, name, sentinel string) string {
	t.Helper()
	dir := filepath.Join(resolvedTempDir(t), name)
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	writeWorkerFixture(t, dir, "Cargo.toml", crateManifest(name, ""))
	writeWorkerFixture(t, dir, "src/lib.rs", "")
	writeWorkerFixture(t, dir, "build.rs", sentinelBuildScript(sentinel))
	gitCommitAll(t, dir, "crate")
	return "file://" + filepath.ToSlash(dir)
}

type rustFixture struct {
	repo, base, depDir string
	sentinels          []string
}

// rustRepo commits a workspace whose root package depends on a git crate
// named dep. The root package, its member and dep all have sentinel build
// scripts, and the base commits a .cargo/config.toml whose rustc wrapper
// writes a sentinel and a rust-toolchain.toml naming a missing toolchain.
func rustRepo(t *testing.T, dep string) rustFixture {
	t.Helper()
	sentinels := resolvedTempDir(t)
	f := rustFixture{}
	for _, name := range []string{"dep-build", "root-build", "member-build", "rustc-wrapper"} {
		f.sentinels = append(f.sentinels, filepath.Join(sentinels, name))
	}
	url := gitCrate(t, dep, f.sentinels[0])
	f.depDir = strings.TrimPrefix(url, "file://")
	root := crateManifest("app", "\n[dependencies]\n"+dep+" = { git = \""+url+"\" }\n\n[workspace]\nmembers = [\"crates/member\"]\n")
	member := crateManifest("member", "")

	// Lock in a clean copy, so no fixture code runs while locking.
	lock := resolvedTempDir(t)
	writeWorkerFixture(t, lock, "Cargo.toml", root)
	writeWorkerFixture(t, lock, "src/lib.rs", "")
	writeWorkerFixture(t, lock, "crates/member/Cargo.toml", member)
	writeWorkerFixture(t, lock, "crates/member/src/lib.rs", "")
	cmd := exec.Command("cargo", "generate-lockfile", "--offline")
	cmd.Dir = lock
	cmd.Env = append(os.Environ(), "CARGO_HOME="+filepath.Join(resolvedTempDir(t), "cargo"))
	if out, err := cmd.CombinedOutput(); err != nil {
		cmd = exec.Command("cargo", "generate-lockfile")
		cmd.Dir, cmd.Env = lock, append(os.Environ(), "CARGO_HOME="+filepath.Join(resolvedTempDir(t), "cargo"))
		if out2, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("cargo generate-lockfile: %v (%s %s)", err, out, out2)
		}
	}
	lockfile, err := os.ReadFile(filepath.Join(lock, "Cargo.lock"))
	if err != nil {
		t.Fatal(err)
	}

	wrapper := filepath.Join(resolvedTempDir(t), "wrapper.sh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ntouch "+f.sentinels[3]+"\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.repo = cacheTestRepo(t)
	for name, content := range map[string]string{
		"Cargo.toml":               root,
		"Cargo.lock":               string(lockfile),
		"build.rs":                 sentinelBuildScript(f.sentinels[1]),
		"src/lib.rs":               "",
		"crates/member/Cargo.toml": member,
		"crates/member/build.rs":   sentinelBuildScript(f.sentinels[2]),
		"crates/member/src/lib.rs": "",
		".cargo/config.toml":       "[build]\nrustc-wrapper = " + strconv.Quote(wrapper) + "\n",
		"rust-toolchain.toml":      "[toolchain]\nchannel = \"swarm-missing-toolchain\"\n",
	} {
		writeWorkerFixture(t, f.repo, name, content)
	}
	f.base = gitCommitAll(t, f.repo, "base")
	return f
}

// hostRustEnv points HostToolchains at a synthetic HOME with an empty
// .cargo, keeping the real rustup home so a rustup cargo proxy still works.
func hostRustEnv(t *testing.T) (home string) {
	t.Helper()
	rustup := os.Getenv("RUSTUP_HOME")
	if rustup == "" {
		if real, err := os.UserHomeDir(); err == nil {
			rustup = filepath.Join(real, ".rustup")
		}
	}
	home = resolvedTempDir(t)
	if err := os.MkdirAll(filepath.Join(home, ".cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CARGO_HOME", "")
	t.Setenv("RUSTUP_HOME", rustup)
	return home
}

func assertNoSentinels(t *testing.T, sentinels []string) {
	t.Helper()
	for _, s := range sentinels {
		if _, err := os.Stat(s); err == nil {
			t.Errorf("prefetch ran package or repository code: %s exists", filepath.Base(s))
		}
	}
}

func gitCheckedOut(cache, dep string) bool {
	matches, _ := filepath.Glob(filepath.Join(cache, "git", "checkouts", dep+"-*", "*", "Cargo.toml"))
	return len(matches) > 0
}

func TestCargoPrefetchRunsNoPackageCode(t *testing.T) {
	requireCargo(t)
	f := rustRepo(t, "dep")
	hostRustEnv(t)
	userCache := resolvedTempDir(t)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, f.repo, f.base, userCache, PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps.Caches) != 1 || deps.Caches[0].Ecosystem != "rust" || deps.Caches[0].EnvKey != "CARGO_HOME" {
		t.Fatalf("caches = %+v", deps.Caches)
	}
	if !gitCheckedOut(deps.Caches[0].Dir, "dep") {
		t.Fatal("dep is not in the per-repository cache")
	}
	assertNoSentinels(t, f.sentinels)
	if err := checkCargoHome(deps.Caches[0].Dir); err != nil {
		t.Fatal(err)
	}
}

func TestRustDependencyAbsentFromHostIsPrefetched(t *testing.T) {
	requireCargo(t)
	f := rustRepo(t, "dep")
	home := hostRustEnv(t)
	userCache := resolvedTempDir(t)
	fetches := 0
	previous := runPrefetchCommand
	runPrefetchCommand = func(ctx context.Context, dir string, env, argv []string) ([]byte, []byte, error) {
		if slices.Equal(argv, cargoFetchArgv) {
			fetches++
		}
		return previous(ctx, dir, env, argv)
	}
	t.Cleanup(func() { runPrefetchCommand = previous })
	prepare := func(base string) Dependencies {
		t.Helper()
		deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, f.repo, base, userCache, PerRepository)
		if err != nil {
			t.Fatal(err)
		}
		return deps
	}
	deps := prepare(f.base)
	if !gitCheckedOut(deps.Caches[0].Dir, "dep") {
		t.Fatal("dep absent from the host was not prefetched")
	}
	if entries, err := os.ReadDir(filepath.Join(home, ".cargo")); err != nil || len(entries) != 0 {
		t.Fatalf("prefetch touched the host Cargo home: %v %v", entries, err)
	}
	prepare(f.base)
	if fetches != 1 {
		t.Fatalf("unchanged manifests fetched %d times", fetches)
	}
	writeWorkerFixture(t, f.repo, "crates/member/Cargo.toml", crateManifest("member", "description = \"changed\"\n"))
	prepare(gitCommitAll(t, f.repo, "member"))
	if fetches != 2 {
		t.Fatalf("a changed nested Cargo.toml must fetch again, got %d", fetches)
	}
}

func TestCargoPrefetchFailureNamesDependency(t *testing.T) {
	requireCargo(t)
	f := rustRepo(t, "missingdep")
	if err := os.RemoveAll(f.depDir); err != nil {
		t.Fatal(err)
	}
	hostRustEnv(t)
	userCache := resolvedTempDir(t)
	_, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, f.repo, f.base, userCache, PerRepository)
	if err == nil {
		t.Fatal("a dependency missing from every source must fail the prefetch")
	}
	t.Logf("preflight error: %v", err)
	for _, want := range []string{"prefetch rust", "missingdep"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), userCache) || strings.Contains(err.Error(), ".prefetch-rust-") {
		t.Errorf("error %q exposes the cache or temporary path", err)
	}
	matches, _ := filepath.Glob(filepath.Join(userCache, "swarm", "*", "*", ".prefetched-*"))
	if len(matches) != 0 {
		t.Fatalf("failed prefetch recorded %v", matches)
	}
}

func TestCargoFetchFailure(t *testing.T) {
	for _, tc := range []struct{ name, stderr, want string }{
		{"git source", "error: failed to get `dep` as a dependency of package `app v0.1.0 (/w)`\n\nCaused by:\n  failed to load source for dependency `dep`\n\nCaused by:\n  unable to update file:///gone\n", "dependency dep: unable to update file:///gone"},
		{"registry download", "error: failed to download from `https://x/serde/1.0.0/download`\n\nCaused by:\n  failed to download `serde v1.0.0`\n\nCaused by:\n  HTTP 404\n", "dependency serde: HTTP 404"},
		{"warnings skipped", "warning: spurious network error\nerror: failed to get `a` as a dependency of package `b`\nCaused by:\n  boom\n", "dependency a: boom"},
		{"no dependency", "error: the lock file needs to be updated but --locked was passed\n", "error: the lock file needs to be updated but --locked was passed"},
		{"no output", "", "exit status 101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cargoFetchFailure([]byte(tc.stderr), errors.New("exit status 101")); got != tc.want {
				t.Fatalf("cargoFetchFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCargoPrefetchEnvHardened(t *testing.T) {
	host := map[string]string{
		"PATH": "/usr/bin", "HOME": "/home/user", "RUSTUP_TOOLCHAIN": "stable",
		"SWARM_GITHUB_TOKEN": "t", "SWARM_GITHUB_TOKEN_COMMAND": "t", "GH_TOKEN": "t", "GITHUB_TOKEN": "t",
		"SSH_AUTH_SOCK": "/tmp/agent", "GIT_ASKPASS": "/bin/askpass", "RUSTC_WRAPPER": "/bin/evil",
		"RUSTFLAGS": "-C x", "CARGO_REGISTRIES_PRIVATE_TOKEN": "t", "CARGO_REGISTRY_TOKEN": "t",
	}
	env := cargoPrefetchEnv(func(k string) string { return host[k] }, "/tmp/prefetch", "/cache/rust", "/rustup")
	values := map[string]string{}
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		if _, dup := values[k]; dup {
			t.Fatalf("duplicate %s in %v", k, env)
		}
		values[k] = v
	}
	for _, key := range []string{"SWARM_GITHUB_TOKEN", "SWARM_GITHUB_TOKEN_COMMAND", "GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "GIT_ASKPASS", "RUSTFLAGS", "CARGO_REGISTRIES_PRIVATE_TOKEN", "CARGO_REGISTRY_TOKEN"} {
		if _, ok := values[key]; ok {
			t.Errorf("prefetch env passes %s", key)
		}
	}
	for key, want := range map[string]string{
		"PATH": "/usr/bin", "HOME": "/tmp/prefetch", "TMPDIR": "/tmp/prefetch", "CARGO_HOME": "/cache/rust",
		"RUSTC_WRAPPER": "", "RUSTC_WORKSPACE_WRAPPER": "", "CARGO_BUILD_RUSTC_WRAPPER": "", "CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER": "",
		"RUSTUP_AUTO_INSTALL": "0", "RUSTUP_HOME": "/rustup", "RUSTUP_TOOLCHAIN": "stable",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
	} {
		if got, ok := values[key]; !ok || got != want {
			t.Errorf("%s = %q (set %t), want %q", key, got, ok, want)
		}
	}
}

// stubRustRepo commits a minimal Cargo repository for stubbed prefetches.
func stubRustRepo(t *testing.T, extra map[string]string) (string, string) {
	t.Helper()
	repo := cacheTestRepo(t)
	files := map[string]string{"Cargo.toml": crateManifest("app", ""), "Cargo.lock": "version = 4\n"}
	for name, content := range extra {
		files[name] = content
	}
	for name, content := range files {
		writeWorkerFixture(t, repo, name, content)
	}
	return repo, gitCommitAll(t, repo, "base")
}

func TestCargoPrefetchCommandIsFetchOnly(t *testing.T) {
	repo, base := stubRustRepo(t, map[string]string{
		"build.rs": "fn main() {}\n", ".cargo/config.toml": "[build]\n", "rust-toolchain.toml": "[toolchain]\n",
		"crates/a/Cargo.toml": crateManifest("a", ""), "crates/a/build.rs": "fn main() {}\n",
	})
	userCache := resolvedTempDir(t)
	var calls [][]string
	var copied []string
	var cwd string
	stubPrefetch(t, "", func(dir string, _, argv []string) {
		calls, cwd = append(calls, argv), dir
		_ = filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
			if err == nil && path != dir {
				rel, _ := filepath.Rel(dir, path)
				copied = append(copied, filepath.ToSlash(rel))
			}
			return nil
		})
	})
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(calls, [][]string{{"cargo", "fetch", "--locked"}}, slices.Equal) {
		t.Fatalf("commands = %q", calls)
	}
	keyDir := filepath.Dir(deps.Caches[0].Dir)
	if !insideDir(keyDir, cwd) || insideDir(repo, cwd) || insideDir(deps.Caches[0].Dir, cwd) {
		t.Fatalf("cargo fetch ran in %s, want a copy under %s", cwd, keyDir)
	}
	slices.Sort(copied)
	want := []string{"Cargo.lock", "Cargo.toml", "crates", "crates/a", "crates/a/Cargo.toml", "crates/a/src", "crates/a/src/lib.rs", "src", "src/lib.rs"}
	if !slices.Equal(copied, want) {
		t.Fatalf("fetch copy = %q, want %q", copied, want)
	}
	if matches, _ := filepath.Glob(filepath.Join(keyDir, ".prefetch-rust-*")); len(matches) != 0 {
		t.Fatalf("temporary copy left behind: %v", matches)
	}
}

func TestRustDetectionReadsBaseNotWorktree(t *testing.T) {
	baseManifest := crateManifest("app", "")
	repo, base := stubRustRepo(t, map[string]string{"Cargo.toml": baseManifest, "Cargo.lock": "version = 4\n# base\n"})
	writeWorkerFixture(t, repo, "Cargo.toml", crateManifest("evil", ""))
	writeWorkerFixture(t, repo, "Cargo.lock", "version = 4\n# candidate\n")
	writeWorkerFixture(t, repo, "nested/Cargo.toml", crateManifest("untracked", ""))
	var manifest, lock string
	var copied []string
	stubPrefetch(t, "", func(dir string, _, _ []string) {
		m, _ := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
		l, _ := os.ReadFile(filepath.Join(dir, "Cargo.lock"))
		manifest, lock = string(m), string(l)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			copied = append(copied, e.Name())
		}
	})
	deps, err := PrepareDependencyCaches(t.Context(), ValidatorDependencyCache, repo, base, resolvedTempDir(t), PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps.Caches) != 1 || deps.Caches[0].Ecosystem != "rust" {
		t.Fatalf("caches = %+v", deps.Caches)
	}
	if manifest != baseManifest || lock != "version = 4\n# base\n" || slices.Contains(copied, "nested") {
		t.Fatalf("prefetch read the worktree: manifest %q lock %q copy %v", manifest, lock, copied)
	}

	// A base with Cargo.toml but no Cargo.lock gets no rust cache.
	unlocked := cacheTestRepo(t)
	writeWorkerFixture(t, unlocked, "Cargo.toml", baseManifest)
	unlockedBase := gitCommitAll(t, unlocked, "base")
	writeWorkerFixture(t, unlocked, "Cargo.lock", "version = 4\n")
	stubPrefetch(t, "", func(string, []string, []string) { t.Fatal("prefetch ran without a base Cargo.lock") })
	deps, err = PrepareDependencyCaches(t.Context(), AgentDependencyCache, unlocked, unlockedBase, resolvedTempDir(t), PerRepository)
	if err != nil || len(deps.Caches) != 0 {
		t.Fatalf("PrepareDependencyCaches = %+v, %v", deps, err)
	}
}

func TestRustCacheMountedReadOnlyNoHostCargo(t *testing.T) {
	home := hostRustEnv(t)
	hostCargo := []string{filepath.Join(home, ".cargo", "registry"), filepath.Join(home, ".cargo", "git")}
	for _, dir := range hostCargo {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repo, base := stubRustRepo(t, nil)
	stubPrefetch(t, "", nil)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	dir := deps.Caches[0].Dir
	work := resolvedTempDir(t)
	flags, err := executorFlags(work, false, nil, deps, "true", nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := codexPolicy("/usr/bin/codex", work, resolvedTempDir(t), false, nil, deps)
	env := nativeEnv(resolvedTempDir(t), false, deps)
	for name, args := range map[string][]string{"bwrap": flags, "codex": policy, "native env": env} {
		joined := strings.Join(args, "\n")
		for _, host := range hostCargo {
			if strings.Contains(joined, host) {
				t.Errorf("%s exposes the host Cargo cache %s", name, host)
			}
		}
		if !strings.Contains(joined, dir) {
			t.Errorf("%s does not use the per-repository cache %s", name, dir)
		}
	}
	if !containsRun(flags, []string{"--ro-bind", dir, dir}) || !containsRun(flags, []string{"--setenv", "CARGO_HOME", dir}) || containsRun(flags, []string{"--bind", dir, dir}) {
		t.Errorf("bwrap must bind the cache read-only and set CARGO_HOME: %v", flags)
	}
	if !slices.Contains(env, "CARGO_HOME="+dir) {
		t.Errorf("native env = %v", env)
	}

	stubPrefetch(t, "", func(string, []string, []string) { t.Fatal("shared mode must not prefetch") })
	shared, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), SharedHostCaches)
	if err != nil || len(shared.Caches) != 0 {
		t.Fatalf("PrepareDependencyCaches = %+v, %v", shared, err)
	}
	flags, err = executorFlags(work, false, nil, shared, "true", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range hostCargo {
		if !containsRun(flags, []string{"--ro-bind", host, host}) {
			t.Errorf("shared mode must mount %s read-only: %v", host, flags)
		}
	}
}

func TestCargoCacheHoldsNoConfigOrCredentials(t *testing.T) {
	for _, name := range []string{"credentials.toml", "config.toml", "bin"} {
		t.Run(name, func(t *testing.T) {
			repo, base := stubRustRepo(t, nil)
			userCache := resolvedTempDir(t)
			stubPrefetch(t, "", func(_ string, env, _ []string) {
				for _, entry := range env {
					if cache, ok := strings.CutPrefix(entry, "CARGO_HOME="); ok {
						writeWorkerFixture(t, cache, name, "token = \"secret\"\n")
					}
				}
			})
			deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository)
			if err == nil || len(deps.Caches) != 0 || !strings.Contains(err.Error(), name) {
				t.Fatalf("PrepareDependencyCaches = %+v, %v; want an error naming %s", deps, err, name)
			}
		})
	}
}
