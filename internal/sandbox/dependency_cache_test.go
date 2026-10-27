package sandbox

import (
	"archive/zip"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// writeModuleProxy lays out a file:// module proxy (the same layout as a
// GOMODCACHE's cache/download) holding each path@version.
func writeModuleProxy(t *testing.T, root string, modules ...string) {
	t.Helper()
	for _, mod := range modules {
		path, version, _ := strings.Cut(mod, "@")
		dir := filepath.Join(root, filepath.FromSlash(path), "@v")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		gomod := "module " + path + "\n\ngo 1.21\n"
		for name, data := range map[string]string{
			version + ".info": `{"Version":"` + version + `","Time":"2024-01-01T00:00:00Z"}`,
			version + ".mod":  gomod,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		f, err := os.Create(filepath.Join(dir, version+".zip"))
		if err != nil {
			t.Fatal(err)
		}
		zw := zip.NewWriter(f)
		pkg := filepath.Base(path)
		for name, data := range map[string]string{"go.mod": gomod, pkg + ".go": "package " + pkg + "\n"} {
			w, err := zw.Create(mod + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(data)); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// makeWritableOnCleanup lets t.TempDir remove module caches, whose
// directories go makes read-only. Call it after creating dir.
func makeWritableOnCleanup(t *testing.T, dir string) {
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
}

func gitCommitAll(t *testing.T, repo, message string) string {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", message}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func goModRequiring(modules ...string) string {
	var b strings.Builder
	b.WriteString("module example.com/repo\n\ngo 1.21\n")
	for _, mod := range modules {
		path, version, _ := strings.Cut(mod, "@")
		b.WriteString("\nrequire " + path + " " + version + "\n")
	}
	return b.String()
}

// goModuleRepo commits a go.mod requiring modules and returns repo and base.
func goModuleRepo(t *testing.T, modules ...string) (string, string) {
	t.Helper()
	repo := cacheTestRepo(t)
	writeWorkerFixture(t, repo, "go.mod", goModRequiring(modules...))
	return repo, gitCommitAll(t, repo, "base")
}

// hostGoEnv points HostToolchains at a synthetic HOME whose GOMODCACHE holds
// modules, and makes the upstream proxy upstream (or off).
func hostGoEnv(t *testing.T, upstream string, modules ...string) (home, hostCache string) {
	t.Helper()
	home = resolvedTempDir(t)
	hostCache = filepath.Join(home, "go", "pkg", "mod")
	writeModuleProxy(t, filepath.Join(hostCache, "cache", "download"), modules...)
	for _, mod := range modules {
		dir := filepath.Join(hostCache, filepath.FromSlash(mod))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeWorkerFixture(t, dir, "go.mod", "module "+strings.Split(mod, "@")[0]+"\n")
	}
	t.Setenv("HOME", home)
	t.Setenv("GOMODCACHE", hostCache)
	t.Setenv("GOPATH", "")
	t.Setenv("GOPROXY", upstream)
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOFLAGS", "")
	for _, key := range []string{"GOPRIVATE", "GONOPROXY", "GONOSUMDB"} {
		t.Setenv(key, "")
	}
	return home, hostCache
}

func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
}

// stubPrefetch replaces the host prefetch runner. GOVERSION reports version;
// every other command succeeds.
func stubPrefetch(t *testing.T, version string, record func(dir string, env, argv []string)) {
	t.Helper()
	previous := runPrefetchCommand
	runPrefetchCommand = func(_ context.Context, dir string, env, argv []string) ([]byte, []byte, error) {
		if record != nil {
			record(dir, env, argv)
		}
		if slices.Equal(argv, []string{"go", "env", "GOVERSION"}) {
			return []byte(version + "\n"), nil, nil
		}
		return nil, nil, nil
	}
	t.Cleanup(func() { runPrefetchCommand = previous })
}

func moduleExtracted(cache, mod string) bool {
	_, err := os.Stat(filepath.Join(cache, filepath.FromSlash(mod), "go.mod"))
	return err == nil
}

func TestDependencyCacheExcludesUnrequiredHostModule(t *testing.T) {
	requireGo(t)
	_, hostCache := hostGoEnv(t, "off", "example.com/needed@v1.0.0", "example.com/unrequired@v1.0.0")
	repo, base := goModuleRepo(t, "example.com/needed@v1.0.0")
	userCache := resolvedTempDir(t)
	makeWritableOnCleanup(t, userCache)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps.Caches) != 1 || deps.Caches[0].EnvKey != "GOMODCACHE" {
		t.Fatalf("caches = %+v", deps.Caches)
	}
	dir := deps.Caches[0].Dir
	if !moduleExtracted(dir, "example.com/needed@v1.0.0") || moduleExtracted(dir, "example.com/unrequired@v1.0.0") {
		t.Fatalf("per-repository cache must hold only required modules")
	}

	work := resolvedTempDir(t)
	flags, err := executorFlags(work, false, nil, deps, "true", nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := codexPolicy("/usr/bin/codex", work, resolvedTempDir(t), false, nil, deps)
	env := nativeEnv(resolvedTempDir(t), false, deps)
	for name, args := range map[string][]string{"bwrap": flags, "codex": policy, "native env": env} {
		joined := strings.Join(args, "\n")
		if strings.Contains(joined, hostCache) {
			t.Errorf("%s exposes the host module cache: %s", name, joined)
		}
		if !strings.Contains(joined, dir) {
			t.Errorf("%s does not use the per-repository cache %s", name, dir)
		}
	}
	if !containsRun(flags, []string{"--ro-bind", dir, dir}) || !containsRun(flags, []string{"--setenv", "GOMODCACHE", dir}) {
		t.Errorf("bwrap must bind the cache read-only and set GOMODCACHE: %v", flags)
	}
	if !slices.Contains(env, "GOMODCACHE="+dir) {
		t.Errorf("native env = %v", env)
	}
}

func TestDependencyCachePathAndOwners(t *testing.T) {
	stubPrefetch(t, "go1.21.0", nil)
	repo, base := goModuleRepo(t)
	linked := filepath.Join(resolvedTempDir(t), "linked")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "--detach", linked, base).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v (%s)", err, out)
	}
	userCache := resolvedTempDir(t)
	key, err := repositoryCacheKey(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[BuildCacheOwner]string{}
	for _, owner := range []BuildCacheOwner{AgentDependencyCache, ValidatorDependencyCache} {
		for _, wt := range []string{repo, linked} {
			deps, err := PrepareDependencyCaches(t.Context(), owner, wt, base, userCache, PerRepository)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(userCache, "swarm", string(owner), key, "go")
			if len(deps.Caches) != 1 || deps.Caches[0].Dir != want {
				t.Fatalf("%s caches = %+v, want %s", owner, deps.Caches, want)
			}
			dirs[owner] = want
		}
	}
	if dirs[AgentDependencyCache] == dirs[ValidatorDependencyCache] {
		t.Fatal("agent and validator dependency caches must differ")
	}
	for _, c := range mustPrepare(t, AgentBuildCache, repo, userCache, resolvedTempDir(t)) {
		if c.Dir == dirs[AgentDependencyCache] || c.Dir == dirs[ValidatorDependencyCache] {
			t.Fatalf("build cache %s shares a dependency cache directory", c.Dir)
		}
	}
	if _, err := PrepareDependencyCaches(t.Context(), AgentBuildCache, repo, base, userCache, PerRepository); err == nil {
		t.Fatal("a build cache owner must not own dependency caches")
	}
}

func TestDependencyCacheNoFallback(t *testing.T) {
	stubPrefetch(t, "go1.21.0", nil)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, repo string) string
	}{
		{"user cache unavailable", func(*testing.T, string) string { return "" }},
		{"relative user cache", func(*testing.T, string) string { return "relative/cache" }},
		{"symlinked component", func(t *testing.T, _ string) string {
			userCache := resolvedTempDir(t)
			if err := os.Symlink(resolvedTempDir(t), filepath.Join(userCache, "swarm")); err != nil {
				t.Fatal(err)
			}
			return userCache
		}},
		{"group-writable component", func(t *testing.T, _ string) string {
			userCache := resolvedTempDir(t)
			dir := filepath.Join(userCache, "swarm", string(AgentDependencyCache))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o770); err != nil {
				t.Fatal(err)
			}
			return userCache
		}},
		{"inside worktree", func(_ *testing.T, repo string) string { return filepath.Join(repo, ".cache") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && tc.name == "group-writable component" {
				t.Skip("permission bits are not checked on Windows")
			}
			repo, base := goModuleRepo(t)
			deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, tc.setup(t, repo), PerRepository)
			if err == nil || len(deps.Caches) != 0 {
				t.Fatalf("PrepareDependencyCaches = %+v, %v; want an error and no cache", deps, err)
			}
		})
	}
}

func TestDependencyCacheSkipsRepositoriesWithoutAdapter(t *testing.T) {
	stubPrefetch(t, "go1.21.0", func(string, []string, []string) { t.Fatal("prefetch ran without go.mod") })
	repo := cacheTestRepo(t)
	writeWorkerFixture(t, repo, "README.md", "no modules\n")
	base := gitCommitAll(t, repo, "base")
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, "", PerRepository)
	if err != nil || len(deps.Caches) != 0 || deps.Mode != PerRepository {
		t.Fatalf("PrepareDependencyCaches = %+v, %v", deps, err)
	}
	if _, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, strings.Repeat("0", 40), "", PerRepository); err == nil {
		t.Fatal("an unknown base must be an error")
	}
}

func TestGoPrefetchReadsBaseNotWorktree(t *testing.T) {
	requireGo(t)
	hostGoEnv(t, "off", "example.com/needed@v1.0.0", "example.com/evil@v1.0.0")
	repo, base := goModuleRepo(t, "example.com/needed@v1.0.0")
	candidate := goModRequiring("example.com/evil@v1.0.0")
	writeWorkerFixture(t, repo, "go.mod", candidate)
	userCache := resolvedTempDir(t)
	makeWritableOnCleanup(t, userCache)
	deps, err := PrepareDependencyCaches(t.Context(), ValidatorDependencyCache, repo, base, userCache, PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	dir := deps.Caches[0].Dir
	if !moduleExtracted(dir, "example.com/needed@v1.0.0") || moduleExtracted(dir, "example.com/evil@v1.0.0") {
		t.Fatal("prefetch must read go.mod from the approved base")
	}
	if got, err := os.ReadFile(filepath.Join(repo, "go.mod")); err != nil || string(got) != candidate {
		t.Fatalf("worktree go.mod changed: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "go.sum")); !os.IsNotExist(err) {
		t.Fatalf("prefetch wrote go.sum into the worktree: %v", err)
	}
}

func TestGoPrefetchSeedsFromHostCacheFirst(t *testing.T) {
	requireGo(t)
	upstream := resolvedTempDir(t)
	writeModuleProxy(t, upstream, "example.com/other@v1.0.0")
	hostGoEnv(t, "file://"+filepath.ToSlash(upstream), "example.com/needed@v1.0.0")
	repo, base := goModuleRepo(t, "example.com/needed@v1.0.0", "example.com/other@v1.0.0")
	userCache := resolvedTempDir(t)
	makeWritableOnCleanup(t, userCache)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	for _, mod := range []string{"example.com/needed@v1.0.0", "example.com/other@v1.0.0"} {
		if !moduleExtracted(deps.Caches[0].Dir, mod) {
			t.Errorf("%s was not downloaded", mod)
		}
	}
}

func TestGoPrefetchFailureNamesModule(t *testing.T) {
	requireGo(t)
	_, hostCache := hostGoEnv(t, "off", "example.com/needed@v1.0.0")
	repo, base := goModuleRepo(t, "example.com/needed@v1.0.0", "example.com/missing@v1.0.0")
	userCache := resolvedTempDir(t)
	makeWritableOnCleanup(t, userCache)
	_, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository)
	if err == nil {
		t.Fatal("a module missing from every source must fail the prefetch")
	}
	t.Logf("preflight error: %v", err)
	for _, want := range []string{"prefetch go modules", "example.com/missing@v1.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	for _, path := range []string{hostCache, userCache} {
		if strings.Contains(err.Error(), path) {
			t.Errorf("error %q exposes local path %q", err, path)
		}
	}
	// A failed prefetch leaves no marker, so the next run tries again.
	matches, _ := filepath.Glob(filepath.Join(userCache, "swarm", "*", "*", ".prefetched-*"))
	if len(matches) != 0 {
		t.Fatalf("failed prefetch recorded %v", matches)
	}
}

func TestGoPrefetchEnvHardened(t *testing.T) {
	host := map[string]string{
		"PATH": "/usr/bin", "HOME": "/home/user", "GOPROXY": "https://proxy.example",
		"SWARM_GITHUB_TOKEN": "t", "SWARM_GITHUB_TOKEN_COMMAND": "t", "GH_TOKEN": "t", "GITHUB_TOKEN": "t",
		"SSH_AUTH_SOCK": "/tmp/agent", "GIT_ASKPASS": "/bin/askpass", "GOFLAGS": "-toolexec=x",
		"GOPRIVATE": "example.com/private", "GOSUMDB": "sum.example",
	}
	hostCache := resolvedTempDir(t)
	if err := os.MkdirAll(filepath.Join(hostCache, "cache", "download"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := goPrefetchEnv(func(k string) string { return host[k] }, "/tmp/prefetch", "/cache/go", hostCache)
	values := map[string]string{}
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		if _, dup := values[k]; dup {
			t.Fatalf("duplicate %s in %v", k, env)
		}
		values[k] = v
	}
	for _, key := range []string{"SWARM_GITHUB_TOKEN", "SWARM_GITHUB_TOKEN_COMMAND", "GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "GIT_ASKPASS"} {
		if _, ok := values[key]; ok {
			t.Errorf("prefetch env passes %s", key)
		}
	}
	for key, want := range map[string]string{
		"GOFLAGS": "", "HOME": "/tmp/prefetch", "GOTOOLCHAIN": "local", "GOENV": "off", "GOWORK": "off",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
		"GOMODCACHE": "/cache/go", "GOPRIVATE": "example.com/private", "GOSUMDB": "sum.example",
		"GOPROXY": "file://" + filepath.ToSlash(filepath.Join(hostCache, "cache", "download")) + ",https://proxy.example",
	} {
		if got, ok := values[key]; !ok || got != want {
			t.Errorf("%s = %q (set %t), want %q", key, got, ok, want)
		}
	}
	if got := goPrefetchEnv(func(string) string { return "" }, "/t", "/c", ""); !slices.Contains(got, "GOPROXY=https://proxy.golang.org,direct") {
		t.Errorf("default upstream missing: %v", got)
	}
}

func TestGoPrefetchMarkerSkipsRepeat(t *testing.T) {
	downloads := 0
	stubPrefetch(t, "go1.21.0", func(_ string, _, argv []string) {
		if slices.Equal(argv, goModDownloadArgv) {
			downloads++
		}
	})
	repo, base := goModuleRepo(t)
	userCache := resolvedTempDir(t)
	prepare := func(base string) {
		t.Helper()
		if _, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository); err != nil {
			t.Fatal(err)
		}
	}
	prepare(base)
	prepare(base)
	if downloads != 1 {
		t.Fatalf("unchanged lockfiles downloaded %d times", downloads)
	}
	writeWorkerFixture(t, repo, "README.md", "unrelated change\n")
	prepare(gitCommitAll(t, repo, "docs"))
	if downloads != 1 {
		t.Fatalf("a change outside the lockfiles downloaded again")
	}
	writeWorkerFixture(t, repo, "go.sum", "example.com/x v1.0.0/go.mod h1:x=\n")
	prepare(gitCommitAll(t, repo, "sum"))
	if downloads != 2 {
		t.Fatalf("a changed go.sum must download again, got %d", downloads)
	}
}

func TestGoPrefetchCommandIsFetchOnly(t *testing.T) {
	for _, tc := range []struct {
		name, gomod, local string
		toolchain          string
		wantGoLine         string
	}{
		{"same toolchain", "module example.com/repo\n\ngo 1.21.0\n", "go1.21.0", "", "go 1.21.0"},
		{"newer go line", "module example.com/repo\n\ngo 1.99.1\n", "go1.21.0", "go1.99.1", "go 1.21.0"},
		{"newer toolchain line", "module example.com/repo\n\ngo 1.21\n\ntoolchain go1.99.2 // pinned\n", "go1.21.0", "go1.99.2", "go 1.21.0"},
		{"unknown local version", "module example.com/repo\n\ngo 1.99.1\n", "devel", "", "go 1.99.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			var gomod string
			stubPrefetch(t, tc.local, func(dir string, env, argv []string) {
				calls = append(calls, argv)
				if !slices.Contains(env, "GOTOOLCHAIN=local") || !slices.Contains(env, "GOFLAGS=") {
					t.Errorf("%v runs without GOTOOLCHAIN=local and empty GOFLAGS", argv)
				}
				if slices.Equal(argv, goModDownloadArgv) {
					data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
					if err != nil {
						t.Fatal(err)
					}
					gomod = string(data)
				}
			})
			repo := cacheTestRepo(t)
			writeWorkerFixture(t, repo, "go.mod", tc.gomod)
			base := gitCommitAll(t, repo, "base")
			var dirs []string
			previous := runPrefetchCommand
			runPrefetchCommand = func(ctx context.Context, dir string, env, argv []string) ([]byte, []byte, error) {
				dirs = append(dirs, dir)
				return previous(ctx, dir, env, argv)
			}
			if _, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), PerRepository); err != nil {
				t.Fatal(err)
			}
			want := [][]string{{"go", "env", "GOVERSION"}}
			if tc.toolchain != "" {
				want = append(want, append(slices.Clone(goModDownloadArgv), "golang.org/toolchain@v0.0.1-"+tc.toolchain+"."+runtime.GOOS+"-"+runtime.GOARCH))
			}
			want = append(want, goModDownloadArgv)
			if !slices.EqualFunc(calls, want, slices.Equal) {
				t.Fatalf("commands = %q, want %q", calls, want)
			}
			if !strings.Contains(gomod, tc.wantGoLine+"\n") || strings.Contains(gomod, "toolchain") {
				t.Fatalf("prefetched go.mod = %q, want %q and no toolchain line", gomod, tc.wantGoLine)
			}
			for _, dir := range dirs {
				if insideDir(repo, dir) {
					t.Fatalf("prefetch ran inside the worktree: %s", dir)
				}
			}
		})
	}
}

func TestGoVersionOrdering(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.26.10", "1.26.8", 1},
		{"1.26.8", "1.26.10", -1},
		{"1.21", "1.21rc1", -1},
		{"1.21rc1", "1.21.0", -1},
		{"1.22beta1", "1.22rc1", -1},
		{"go1.21.0", "1.21.0", 0},
		{"2.0.0", "1.99.99", 1},
	} {
		if got := compareGoVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareGoVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for gomod, want := range map[string]string{
		"go 1.26.10\n":                   "1.26.10",
		"go 1.21\n":                      "1.21.0",
		"go 1.20\n":                      "1.20",
		"go 1.21\ntoolchain go1.22.3\n":  "1.22.3",
		"go 1.21.1\ntoolchain default\n": "1.21.1",
		"module x\n":                     "",
	} {
		if got := requiredGoToolchain([]byte(gomod)); got != want {
			t.Errorf("requiredGoToolchain(%q) = %q, want %q", gomod, got, want)
		}
	}
}

// TestEveryMountedEcosystemIsClassified reads toolchain.go, so a new
// addCache call fails here until its ecosystem is in dependencyEcosystems.
func TestEveryMountedEcosystemIsClassified(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "toolchain.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); !ok || ident.Name != "addCache" {
			return true
		}
		calls++
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("addCache call without a literal ecosystem")
			return true
		}
		name, _ := strconv.Unquote(lit.Value)
		if _, ok := dependencyEcosystemNamed(name); !ok {
			t.Errorf("addCache ecosystem %q is not classified", name)
		}
		return true
	})
	if calls == 0 {
		t.Fatal("found no addCache calls")
	}

	home := resolvedTempDir(t)
	for _, rel := range []string{"go/pkg/mod", ".cargo/registry", ".cargo/git", ".cache/pip", ".cache/uv", ".local/share/uv/python",
		".cache/pypoetry", ".npm", ".local/share/pnpm/store", ".cache/yarn", ".bun/install/cache", ".cache/ms-playwright",
		".cache/Cypress", ".nuget/packages", ".cache/vcpkg", ".m2/repository", ".gradle/caches", ".gradle/wrapper"} {
		if err := os.MkdirAll(filepath.Join(home, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	binds := resolveHostToolchains(home, func(string) string { return "" })
	cached := 0
	for _, b := range binds {
		if b.Ecosystem == "" {
			continue
		}
		cached++
		if _, ok := dependencyEcosystemNamed(b.Ecosystem); !ok {
			t.Errorf("bind %+v has an unclassified ecosystem", b)
		}
	}
	if cached != 18 {
		t.Errorf("found %d cache binds, want 18", cached)
	}
}

func TestDependencyTableSafety(t *testing.T) {
	denied := []string{"HOME", "CARGO_HOME", "GRADLE_USER_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "GOPATH", "PATH"}
	seen, keys := map[string]bool{}, map[string]bool{}
	for _, eco := range dependencyEcosystems {
		if seen[eco.Name] || eco.Name == "" || strings.ContainsAny(eco.Name, "/= ") {
			t.Errorf("ecosystem name %q is empty, duplicated or not plain", eco.Name)
		}
		seen[eco.Name] = true
		if (eco.Prefetch == nil) == (eco.NoPrefetch == "") {
			t.Errorf("%s must have exactly one of Prefetch and NoPrefetch", eco.Name)
		}
		if (eco.Prefetch != nil) != (eco.envKey() != "") || (eco.Prefetch != nil) != (len(eco.Detect) > 0 && len(eco.Lockfiles) > 0) {
			t.Errorf("%s: adapters need an env key, detection files and lockfiles", eco.Name)
		}
		for _, k := range eco.Env {
			if IsBuildCacheKey(k.Key) || slices.Contains(denied, k.Key) || keys[k.Key] {
				t.Errorf("%s env key %q is a build cache or credential home key, or duplicated", eco.Name, k.Key)
			}
			keys[k.Key] = true
			if !filepath.IsLocal(filepath.Join(".", k.Subdir)) {
				t.Errorf("%s env key %s subdirectory %q leaves the cache", eco.Name, k.Key, k.Subdir)
			}
		}
	}
	for _, name := range []string{"dotnet", "maven", "gradle", "c-cpp"} {
		if eco, ok := dependencyEcosystemNamed(name); !ok || eco.Prefetch != nil {
			t.Errorf("%s must be classified no-prefetch", name)
		}
	}
	for _, name := range []string{"go", "node"} {
		if eco, ok := dependencyEcosystemNamed(name); !ok || eco.Prefetch == nil {
			t.Errorf("%s must be adapter-backed", name)
		}
	}
}

func TestNoPrefetchEcosystemsKeepHostMounts(t *testing.T) {
	home := resolvedTempDir(t)
	paths := map[string]string{}
	for _, rel := range []string{"go/pkg/mod", ".m2/repository", ".gradle/caches", ".nuget/packages", ".cache/vcpkg", ".npm", ".cache/ms-playwright"} {
		paths[rel] = filepath.Join(home, rel)
		if err := os.MkdirAll(paths[rel], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	binds := resolveHostToolchains(home, func(string) string { return "" })
	for _, tc := range []struct {
		mode   DependencyCacheMode
		goMod  bool // also the host npm cache
		reason string
	}{{PerRepository, false, "per-repository"}, {"", false, "zero value"}, {SharedHostCaches, true, "opt-in"}} {
		mounts := ToolchainMounts(filterDependencyBinds(binds, tc.mode), true)
		for rel, path := range paths {
			want := (rel != "go/pkg/mod" && rel != ".npm") || tc.goMod
			if got := slices.Contains(mounts, path); got != want {
				t.Errorf("%s: mount %s = %t, want %t", tc.reason, rel, got, want)
			}
		}
	}
}

func TestSharedHostCachesMountsHostGoModCache(t *testing.T) {
	stubPrefetch(t, "go1.21.0", func(string, []string, []string) { t.Fatal("shared mode must not prefetch") })
	_, hostCache := hostGoEnv(t, "off", "example.com/needed@v1.0.0")
	repo, base := goModuleRepo(t, "example.com/needed@v1.0.0")
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), SharedHostCaches)
	if err != nil || len(deps.Caches) != 0 || deps.Mode != SharedHostCaches {
		t.Fatalf("PrepareDependencyCaches = %+v, %v", deps, err)
	}
	flags, err := executorFlags(resolvedTempDir(t), false, nil, deps, "true", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsRun(flags, []string{"--ro-bind", hostCache, hostCache}) || !containsRun(flags, []string{"--setenv", "GOMODCACHE", hostCache}) {
		t.Fatalf("shared mode must mount the host module cache read-only: %v", flags)
	}
	if containsRun(flags, []string{"--bind", hostCache, hostCache}) {
		t.Fatal("host module cache is writable")
	}
}

func TestDependencyCacheFlagsRoundTrip(t *testing.T) {
	work := resolvedTempDir(t)
	dir := resolvedTempDir(t)
	for _, deps := range []Dependencies{
		{Mode: PerRepository, Caches: []DependencyCache{{Ecosystem: "go", EnvKey: "GOMODCACHE", Dir: dir}}},
		{Mode: PerRepository, Caches: []DependencyCache{{Ecosystem: "node", EnvKey: "npm_config_cache", Dir: dir}}},
		{Mode: SharedHostCaches},
	} {
		args := internalToolArgs(ToolExecutor{Work: work, Dependencies: deps})
		if got := slices.Contains(args, "--share-host-dependency-caches"); got != (deps.Mode == SharedHostCaches) {
			t.Errorf("share flag = %t for %s: %v", got, deps.Mode, args)
		}
		for _, c := range deps.Caches {
			i := slices.Index(args, "--dependency-cache")
			if i < 0 || args[i+1] != c.Ecosystem+"="+dir {
				t.Fatalf("args = %v", args)
			}
			parsed, err := ParseDependencyCache(args[i+1], work)
			if err != nil || parsed != c {
				t.Fatalf("ParseDependencyCache = %+v, %v; want %+v", parsed, err, c)
			}
		}
	}
	for _, value := range []string{"go", "go=relative", "go=/", "rust=" + dir, "maven=" + dir, "node-browsers=" + dir, "unknown=" + dir, "go=" + work, "go=" + filepath.Join(work, "sub")} {
		if c, err := ParseDependencyCache(value, work); err == nil {
			t.Errorf("ParseDependencyCache(%q) = %+v", value, c)
		}
	}
	for _, bad := range []DependencyCache{{Ecosystem: "go", EnvKey: "GOCACHE", Dir: dir}, {Ecosystem: "node", EnvKey: "YARN_CACHE_FOLDER", Dir: dir}} {
		if _, err := executorFlags(work, false, nil, Dependencies{Caches: []DependencyCache{bad}}, "true", nil); err == nil {
			t.Fatalf("executor accepted dependency cache %+v outside the table", bad)
		}
	}
}

func TestDependencyCacheModes(t *testing.T) {
	for _, tc := range []struct {
		mode DependencyCacheMode
		want string
	}{{PerRepository, "per-repository"}, {SharedHostCaches, "host-shared (opt-in)"}} {
		modes := DependencyCacheModes(tc.mode)
		if len(modes) != len(dependencyEcosystems) {
			t.Fatalf("modes = %+v", modes)
		}
		for _, m := range modes {
			if (m.Name == "go" || m.Name == "node") && m.Mode != tc.want {
				t.Errorf("%s mode = %q, want %q", m.Name, m.Mode, tc.want)
			}
			if (m.Name == "maven" || m.Name == "node-browsers") && !strings.HasPrefix(m.Mode, "host-shared (no fetch-only prefetch") {
				t.Errorf("%s mode = %q", m.Name, m.Mode)
			}
		}
	}
}
