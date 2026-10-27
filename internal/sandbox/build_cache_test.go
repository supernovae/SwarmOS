package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

func useUserCache(t *testing.T) string {
	t.Helper()
	root := resolvedTempDir(t)
	previous := userCacheDir
	userCacheDir = func() (string, error) { return root, nil }
	t.Cleanup(func() { userCacheDir = previous })
	return root
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func cacheTestRepo(t *testing.T) string {
	t.Helper()
	repo := resolvedTempDir(t)
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	return repo
}

func cachedEcosystems() []buildCacheEcosystem {
	var cached []buildCacheEcosystem
	for _, eco := range buildCacheEcosystems {
		if eco.Deferred == "" {
			cached = append(cached, eco)
		}
	}
	return cached
}

func mustPrepare(t *testing.T, owner BuildCacheOwner, repo, userCache, scratch string) []BuildCache {
	t.Helper()
	caches, err := PrepareBuildCaches(context.Background(), owner, repo, userCache, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(caches) != len(cachedEcosystems()) {
		t.Fatalf("caches = %+v, want one per cached ecosystem", caches)
	}
	return caches
}

func TestBuildCacheTableSafety(t *testing.T) {
	// Credential homes and dependency caches must never become a build cache.
	denied := []string{"HOME", "CARGO_HOME", "RUSTUP_HOME", "GRADLE_USER_HOME", "NUGET_PACKAGES", "NUGET_HTTP_CACHE_PATH", "NUGET_PLUGINS_CACHE_PATH", "DOTNET_CLI_HOME", "MAVEN_OPTS", "M2_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "GOPATH", "GOMODCACHE"}
	names, keys := map[string]bool{}, map[string]bool{}
	for _, eco := range buildCacheEcosystems {
		if names[eco.Name] || !regexp.MustCompile(`^[a-z][a-z-]*$`).MatchString(eco.Name) {
			t.Errorf("ecosystem name %q is duplicated or not a plain directory name", eco.Name)
		}
		names[eco.Name] = true
		if eco.Deferred != "" {
			if eco.EnvKey != "" {
				t.Errorf("deferred ecosystem %s sets %s", eco.Name, eco.EnvKey)
			}
			continue
		}
		if eco.EnvKey == "" || keys[eco.EnvKey] || slices.Contains(denied, eco.EnvKey) {
			t.Errorf("ecosystem %s has unsafe env key %q", eco.Name, eco.EnvKey)
		}
		keys[eco.EnvKey] = true
		if !IsBuildCacheKey(eco.EnvKey) {
			t.Errorf("IsBuildCacheKey(%q) = false", eco.EnvKey)
		}
	}
	for _, name := range []string{"go", "rust", "c-cpp"} {
		if !names[name] {
			t.Errorf("cached ecosystem %s missing", name)
		}
	}
	for _, name := range []string{"dotnet", "gradle", "maven"} {
		i := slices.IndexFunc(buildCacheEcosystems, func(e buildCacheEcosystem) bool { return e.Name == name })
		if i < 0 || buildCacheEcosystems[i].Deferred == "" {
			t.Errorf("%s must be listed as deferred", name)
		}
	}
	for _, key := range append(denied, "", "PATH") {
		if IsBuildCacheKey(key) {
			t.Errorf("IsBuildCacheKey(%q) = true", key)
		}
	}
}

func TestPrepareBuildCachesLayout(t *testing.T) {
	repo := cacheTestRepo(t)
	key, err := repositoryCacheKey(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []BuildCacheOwner{AgentBuildCache, ValidatorBuildCache} {
		t.Run(string(owner), func(t *testing.T) {
			root := resolvedTempDir(t)
			caches := mustPrepare(t, owner, repo, root, resolvedTempDir(t))
			for i, eco := range cachedEcosystems() {
				c := caches[i]
				want := filepath.Join(root, "swarm", string(owner), key, eco.Name)
				if c.Ecosystem != eco.Name || c.EnvKey != eco.EnvKey || c.Dir != want || !c.Persistent || c.Overlay {
					t.Fatalf("cache = %+v, want %s at %s", c, eco.EnvKey, want)
				}
				info, err := os.Stat(c.Dir)
				if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
					t.Fatalf("cache dir %s: %v, %v", c.Dir, info, err)
				}
			}
		})
	}
}

func TestPrepareBuildCachesReuseAcrossRuns(t *testing.T) {
	repo := cacheTestRepo(t)
	for _, owner := range []BuildCacheOwner{AgentBuildCache, ValidatorBuildCache} {
		t.Run(string(owner), func(t *testing.T) {
			root := resolvedTempDir(t)
			first := mustPrepare(t, owner, repo, root, resolvedTempDir(t))
			for _, c := range first {
				if err := os.WriteFile(filepath.Join(c.Dir, "marker"), []byte(c.Ecosystem), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			second := mustPrepare(t, owner, repo, root, resolvedTempDir(t))
			if !slices.Equal(first, second) {
				t.Fatalf("second run caches %+v != %+v", second, first)
			}
			for _, c := range second {
				if data, err := os.ReadFile(filepath.Join(c.Dir, "marker")); err != nil || string(data) != c.Ecosystem {
					t.Fatalf("%s cache did not persist: %q, %v", c.Ecosystem, data, err)
				}
			}
		})
	}
}

func TestAgentAndValidatorCachesNeverShareADirectory(t *testing.T) {
	repo := cacheTestRepo(t)
	root := resolvedTempDir(t)
	agent := mustPrepare(t, AgentBuildCache, repo, root, resolvedTempDir(t))
	validator := mustPrepare(t, ValidatorBuildCache, repo, root, resolvedTempDir(t))
	for _, a := range agent {
		for _, v := range validator {
			if insideDir(a.Dir, v.Dir) || insideDir(v.Dir, a.Dir) {
				t.Fatalf("agent cache %s overlaps validator cache %s", a.Dir, v.Dir)
			}
		}
	}
}

func TestValidatorMigratesLegacyGoCache(t *testing.T) {
	repo := cacheTestRepo(t)
	key, err := repositoryCacheKey(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := func(root string) string { return filepath.Join(root, "swarm", "go-build", key) }
	goDir := func(root string) string { return filepath.Join(root, "swarm", "validate-build", key, "go") }
	mkdir := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("moves", func(t *testing.T) {
		root := resolvedTempDir(t)
		mkdir(t, legacyPath(root))
		if err := os.WriteFile(filepath.Join(legacyPath(root), "x"), []byte("compiled"), 0o600); err != nil {
			t.Fatal(err)
		}
		mustPrepare(t, ValidatorBuildCache, repo, root, resolvedTempDir(t))
		if data, err := os.ReadFile(filepath.Join(goDir(root), "x")); err != nil || string(data) != "compiled" {
			t.Fatalf("legacy cache not moved: %q, %v", data, err)
		}
		if _, err := os.Lstat(legacyPath(root)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy cache still present: %v", err)
		}
	})
	t.Run("destination exists", func(t *testing.T) {
		root := resolvedTempDir(t)
		mkdir(t, legacyPath(root))
		mkdir(t, goDir(root))
		mustPrepare(t, ValidatorBuildCache, repo, root, resolvedTempDir(t))
		if _, err := os.Stat(legacyPath(root)); err != nil {
			t.Fatalf("legacy cache was touched although the destination existed: %v", err)
		}
	})
	t.Run("agent caches never migrate", func(t *testing.T) {
		root := resolvedTempDir(t)
		mkdir(t, legacyPath(root))
		mustPrepare(t, AgentBuildCache, repo, root, resolvedTempDir(t))
		if _, err := os.Stat(legacyPath(root)); err != nil {
			t.Fatalf("agent run moved the validator cache: %v", err)
		}
	})
	t.Run("symlink not followed", func(t *testing.T) {
		root, outside := resolvedTempDir(t), resolvedTempDir(t)
		mkdir(t, filepath.Dir(legacyPath(root)))
		if err := os.Symlink(outside, legacyPath(root)); err != nil {
			t.Skip(err)
		}
		caches := mustPrepare(t, ValidatorBuildCache, repo, root, resolvedTempDir(t))
		if caches[0].Dir != goDir(root) || !caches[0].Persistent {
			t.Fatalf("go cache = %+v, want a fresh %s", caches[0], goDir(root))
		}
		if info, err := os.Lstat(goDir(root)); err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("go cache is not a fresh directory: %v, %v", info, err)
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("symlink target was used: %v", entries)
		}
	})
}

func TestRepositoryCacheKeyPerRepository(t *testing.T) {
	ctx := context.Background()
	repo := cacheTestRepo(t)
	gitCommit := exec.Command("git", "-C", repo, "-c", "user.name=T", "-c", "user.email=t@localhost", "commit", "-q", "--allow-empty", "-m", "init")
	if out, err := gitCommit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v (%s)", err, out)
	}
	linked := filepath.Join(resolvedTempDir(t), "linked")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "--detach", linked).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v (%s)", err, out)
	}
	main, err := repositoryCacheKey(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := repositoryCacheKey(ctx, linked)
	if err != nil {
		t.Fatal(err)
	}
	separate, err := repositoryCacheKey(ctx, cacheTestRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	if main != shared || main == separate {
		t.Fatalf("keys main=%s linked=%s other=%s", main, shared, separate)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(main) || strings.Contains(main, filepath.Base(repo)) {
		t.Fatalf("key %q is not an opaque hash", main)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			got, err := repositoryCacheKey(ctx, repo)
			if err == nil && got != main {
				err = fmt.Errorf("concurrent key %q != %q", got, main)
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

func TestPrepareBuildCachesFallsBackToScratch(t *testing.T) {
	repo := cacheTestRepo(t)
	userGo, userCC := resolvedTempDir(t), resolvedTempDir(t)
	t.Setenv("GOCACHE", userGo)
	t.Setenv("CCACHE_DIR", userCC)
	tests := []struct {
		name  string
		repo  string
		cache func(t *testing.T) string
	}{
		{"no user cache directory", repo, func(*testing.T) string { return "" }},
		{"relative user cache directory", repo, func(*testing.T) string { return "cache" }},
		{"user cache inside the worktree", repo, func(*testing.T) string { return filepath.Join(repo, "cache") }},
		{"not a git repository", "", func(t *testing.T) string { return resolvedTempDir(t) }},
		{"group-writable swarm directory", repo, func(t *testing.T) string {
			root := resolvedTempDir(t)
			if err := os.Mkdir(filepath.Join(root, "swarm"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(root, "swarm"), 0o770); err != nil {
				t.Fatal(err)
			}
			return root
		}},
		{"symlinked swarm directory", repo, func(t *testing.T) string {
			root := resolvedTempDir(t)
			if err := os.Symlink(resolvedTempDir(t), filepath.Join(root, "swarm")); err != nil {
				t.Skip(err)
			}
			return root
		}},
		{"file in cache path", repo, func(t *testing.T) string {
			root := resolvedTempDir(t)
			if err := os.WriteFile(filepath.Join(root, "swarm"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return root
		}},
	}
	for _, tt := range tests {
		if runtime.GOOS == "windows" && strings.Contains(tt.name, "group-writable") {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			worktree := tt.repo
			if worktree == "" {
				worktree = resolvedTempDir(t)
			}
			userCache := tt.cache(t)
			for _, owner := range []BuildCacheOwner{AgentBuildCache, ValidatorBuildCache} {
				scratch := resolvedTempDir(t)
				for _, c := range mustPrepare(t, owner, worktree, userCache, scratch) {
					if c.Persistent || c.Dir != filepath.Join(scratch, "build-cache", c.Ecosystem) {
						t.Fatalf("%s: cache %+v is not scratch", owner, c)
					}
					for _, user := range []string{userGo, userCC} {
						if insideDir(user, c.Dir) {
							t.Fatalf("%s: cache %s is under the user's cache %s", owner, c.Dir, user)
						}
					}
					if info, err := os.Stat(c.Dir); err != nil || !info.IsDir() {
						t.Fatalf("scratch cache %s missing: %v", c.Dir, err)
					}
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(repo, "cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache was created inside the worktree: %v", err)
	}
}

func TestPrepareBuildCachesNeverCredentialHome(t *testing.T) {
	repo := cacheTestRepo(t)
	home := resolvedTempDir(t)
	t.Setenv("HOME", home)
	var homes []string
	for _, rel := range []string{".cargo", ".gradle", ".nuget", ".m2", ".config/NuGet", ".cache"} {
		dir := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if rel != ".cache" {
			homes = append(homes, dir)
		}
	}
	for _, owner := range []BuildCacheOwner{AgentBuildCache, ValidatorBuildCache} {
		for _, c := range mustPrepare(t, owner, repo, filepath.Join(home, ".cache"), resolvedTempDir(t)) {
			if !c.Persistent {
				t.Fatalf("expected a persistent cache, got %+v", c)
			}
			if c.Dir == home || !insideDir(filepath.Join(home, ".cache", "swarm"), c.Dir) {
				t.Fatalf("cache %s is not under the SwarmOS cache root", c.Dir)
			}
			for _, credentials := range homes {
				if insideDir(credentials, c.Dir) || insideDir(c.Dir, credentials) {
					t.Fatalf("cache %s overlaps credential home %s", c.Dir, credentials)
				}
			}
		}
	}
}

func TestValidatorCacheMountsPolicy(t *testing.T) {
	repo := cacheTestRepo(t)
	root := resolvedTempDir(t)
	persistent := mustPrepare(t, ValidatorBuildCache, repo, root, resolvedTempDir(t))
	tests := []struct {
		backend          ValidationBackend
		warmup, overlay  bool
		wantPersistent   bool
		wantOverlay      bool
		wantReusedByRuns bool
	}{
		{ValidationOnHost, true, false, true, false, false},
		{ValidationCodex, true, false, true, false, false},
		{ValidationBubblewrap, true, true, true, false, true},
		{ValidationBubblewrap, true, false, true, false, false},
		{ValidationOnHost, false, false, false, false, false},
		{ValidationCodex, false, false, false, false, false},
		{ValidationCodex, false, true, false, false, false},
		{ValidationBubblewrap, false, true, true, true, true},
		{ValidationBubblewrap, false, false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s warmup=%v overlay=%v", tt.backend, tt.warmup, tt.overlay), func(t *testing.T) {
			scratch := resolvedTempDir(t)
			mounts, err := ValidatorCacheMounts(persistent, tt.backend, tt.warmup, tt.overlay, scratch)
			if err != nil {
				t.Fatal(err)
			}
			for i, m := range mounts {
				if m.EnvKey != persistent[i].EnvKey || m.Overlay != tt.wantOverlay {
					t.Fatalf("mount %+v, want overlay=%v", m, tt.wantOverlay)
				}
				if tt.wantPersistent {
					if m.Dir != persistent[i].Dir || !m.Persistent {
						t.Fatalf("mount %+v, want persistent %s", m, persistent[i].Dir)
					}
				} else if m.Persistent || m.Dir != filepath.Join(scratch, "build-cache", m.Ecosystem) || insideDir(root, m.Dir) {
					t.Fatalf("mount %+v, want scratch under %s", m, scratch)
				}
				// The rule: a candidate may never write a persistent cache.
				if !tt.warmup && m.Persistent && !m.Overlay {
					t.Fatalf("candidate gets writable persistent cache %+v", m)
				}
			}
			if !tt.warmup && ValidatorCacheReusable(tt.backend, tt.overlay) != tt.wantReusedByRuns {
				t.Fatalf("ValidatorCacheReusable = %v, want %v", !tt.wantReusedByRuns, tt.wantReusedByRuns)
			}
		})
	}
	// Scratch fallbacks pass through unchanged for every run kind.
	scratchCaches := mustPrepare(t, ValidatorBuildCache, repo, "", resolvedTempDir(t))
	mounts, err := ValidatorCacheMounts(scratchCaches, ValidationBubblewrap, false, true, resolvedTempDir(t))
	if err != nil || !slices.Equal(mounts, scratchCaches) {
		t.Fatalf("scratch mounts = %+v, %v", mounts, err)
	}
}

// buildCacheRoles mirrors how each run selects its caches.
func buildCacheRoles(t *testing.T, repo, scratch string) map[string][]BuildCache {
	t.Helper()
	ctx := context.Background()
	roles := map[string][]BuildCache{}
	for role, opts := range map[string]WorkerOptions{
		"developer": {Role: "developer"}, "tester": {Role: "tester"}, "resolver": {Role: "resolver"},
		"planner": {Role: "planner", ReadOnly: true}, "reviewer": {Role: "reviewer", ReadOnly: true},
	} {
		caches, err := agentBuildCaches(ctx, repo, filepath.Join(scratch, role), opts)
		if err != nil {
			t.Fatal(err)
		}
		roles[role] = caches
	}
	persistent := mustPrepare(t, ValidatorBuildCache, repo, userCacheDirForTest(t), filepath.Join(scratch, "validation"))
	for _, backend := range []ValidationBackend{ValidationCodex, ValidationBubblewrap} {
		for _, warmup := range []bool{false, true} {
			mounts, err := ValidatorCacheMounts(persistent, backend, warmup, true, filepath.Join(scratch, "validation"))
			if err != nil {
				t.Fatal(err)
			}
			roles[fmt.Sprintf("validation %s warmup=%v", backend, warmup)] = mounts
		}
	}
	return roles
}

func userCacheDirForTest(t *testing.T) string {
	t.Helper()
	dir, err := userCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func checkRoleCaches(t *testing.T, role, scratch string, caches []BuildCache, args string) {
	t.Helper()
	agentDir := string(filepath.Separator) + "agent-build" + string(filepath.Separator)
	validatorDir := string(filepath.Separator) + "validate-build" + string(filepath.Separator)
	switch {
	case role == "developer" || role == "tester" || role == "resolver":
		for _, c := range caches {
			if !c.Persistent || !strings.Contains(c.Dir, agentDir) || !strings.Contains(args, c.Dir) {
				t.Fatalf("%s: cache %+v missing or not an agent cache", role, c)
			}
		}
		if strings.Contains(args, validatorDir) {
			t.Fatalf("%s: validator cache reaches the agent sandbox: %s", role, args)
		}
	case role == "planner" || role == "reviewer":
		for _, c := range caches {
			if c.Persistent || !insideDir(scratch, c.Dir) {
				t.Fatalf("%s: read-only role gets %+v", role, c)
			}
		}
		if strings.Contains(args, agentDir) || strings.Contains(args, validatorDir) {
			t.Fatalf("%s: read-only role sees a persistent cache: %s", role, args)
		}
	default:
		if strings.Contains(args, agentDir) {
			t.Fatalf("%s: validation reads an agent cache: %s", role, args)
		}
		for _, c := range caches {
			if c.Persistent && !strings.Contains(c.Dir, validatorDir) {
				t.Fatalf("%s: persistent cache %+v is not the validator's", role, c)
			}
		}
	}
}

func TestCodexPolicyBuildCacheIsolation(t *testing.T) {
	useUserCache(t)
	repo := cacheTestRepo(t)
	scratch := resolvedTempDir(t)
	work := resolvedTempDir(t)
	for role, caches := range buildCacheRoles(t, repo, scratch) {
		t.Run(role, func(t *testing.T) {
			readOnly := role == "planner" || role == "reviewer"
			args := strings.Join(codexPolicy("/usr/bin/codex", work, filepath.Join(scratch, "home"), readOnly, caches, Dependencies{}), "\n")
			checkRoleCaches(t, role, scratch, caches, args)
			for _, c := range caches {
				if !strings.Contains(args, fmt.Sprintf("%q=\"write\"", c.Dir)) {
					t.Fatalf("cache %s is not writable in the policy", c.Dir)
				}
			}
			for _, eco := range cachedEcosystems() {
				if n := strings.Count(args, eco.EnvKey+"="); n != 1 {
					t.Fatalf("%s set %d times in %s", eco.EnvKey, n, args)
				}
			}
		})
	}
}

func TestExecutorFlagsBuildCacheIsolation(t *testing.T) {
	useUserCache(t)
	repo := cacheTestRepo(t)
	scratch := resolvedTempDir(t)
	work := resolvedTempDir(t)
	userCC := filepath.Join(resolvedTempDir(t), "ccache")
	if err := os.Mkdir(userCC, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCACHE_DIR", userCC)
	for role, caches := range buildCacheRoles(t, repo, scratch) {
		t.Run(role, func(t *testing.T) {
			flags, err := executorFlags(work, role == "planner" || role == "reviewer", caches, Dependencies{}, "true", nil)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Join(flags, "\n")
			checkRoleCaches(t, role, scratch, caches, args)
			if strings.Contains(args, userCC) {
				t.Fatalf("user ccache mounted: %s", args)
			}
			for _, c := range caches {
				mount := []string{"--bind", c.Dir, c.Dir}
				if c.Overlay {
					mount = []string{"--overlay-src", c.Dir, "--tmp-overlay", c.Dir}
				}
				if !containsRun(flags, mount) || !containsRun(flags, []string{"--setenv", c.EnvKey, c.Dir}) {
					t.Fatalf("cache %+v not mounted as %v", c, mount)
				}
			}
			for _, eco := range cachedEcosystems() {
				if n := slices.Index(flags, eco.EnvKey); n < 0 || slices.Contains(flags[n+1:], eco.EnvKey) {
					t.Fatalf("%s must be set exactly once: %s", eco.EnvKey, args)
				}
			}
		})
	}
	inside := filepath.Join(work, "cache")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []BuildCache{
		{Ecosystem: "go", EnvKey: "GOCACHE", Dir: inside},
		{Ecosystem: "go", EnvKey: "GOCACHE", Dir: filepath.Dir(work)},
		{Ecosystem: "go", EnvKey: "GOCACHE", Dir: "relative"},
		{Ecosystem: "go", EnvKey: "HOME", Dir: scratch},
	} {
		if _, err := executorFlags(work, false, []BuildCache{bad}, Dependencies{}, "true", nil); err == nil {
			t.Fatalf("executorFlags accepted %+v", bad)
		}
	}
}

func containsRun(flags, run []string) bool {
	for i := range flags {
		if i+len(run) <= len(flags) && slices.Equal(flags[i:i+len(run)], run) {
			return true
		}
	}
	return false
}

func TestDelegatedToolArgsCarryOnlyAgentCaches(t *testing.T) {
	root := useUserCache(t)
	repo := cacheTestRepo(t)
	mustPrepare(t, ValidatorBuildCache, repo, root, resolvedTempDir(t))
	scratch := resolvedTempDir(t)
	work := resolvedTempDir(t)
	for _, opts := range []WorkerOptions{{Role: "developer"}, {Role: "reviewer", ReadOnly: true}} {
		caches, err := agentBuildCaches(context.Background(), repo, scratch, opts)
		if err != nil {
			t.Fatal(err)
		}
		args := internalToolArgs(ToolExecutor{Work: work, ReadOnly: opts.ReadOnly, BuildCaches: caches})
		joined := strings.Join(args, "\n")
		if strings.Contains(joined, "validate-build") || (opts.ReadOnly != slices.Contains(args, "--read-only")) {
			t.Fatalf("%s: tool args %q", opts.Role, args)
		}
		for _, c := range caches {
			if !containsRun(args, []string{"--build-cache", c.Ecosystem + "=" + c.Dir}) {
				t.Fatalf("%s: cache %+v not passed in %q", opts.Role, c, args)
			}
			// The separate tools process rebuilds the same cache from the flag.
			parsed, err := ParseBuildCache(c.Ecosystem+"="+c.Dir, work)
			if err != nil || parsed.EnvKey != c.EnvKey || parsed.Dir != c.Dir {
				t.Fatalf("parsed %+v, %v", parsed, err)
			}
			if opts.ReadOnly == c.Persistent || (c.Persistent && !strings.Contains(c.Dir, "agent-build")) {
				t.Fatalf("%s: cache %+v", opts.Role, c)
			}
		}
	}
}

func TestParseBuildCache(t *testing.T) {
	work := resolvedTempDir(t)
	if c, err := ParseBuildCache("go=/abs/cache", work); err != nil || c.EnvKey != "GOCACHE" || c.Dir != "/abs/cache" || c.Overlay {
		t.Fatalf("ParseBuildCache = %+v, %v", c, err)
	}
	for _, value := range []string{"GOCACHE=/abs", "gradle=/abs", "dotnet=/abs", "go=relative", "go=/", "go", "go=" + filepath.Join(work, "c"), "go=" + filepath.Dir(work), "HOME=/abs"} {
		if c, err := ParseBuildCache(value, work); err == nil {
			t.Errorf("ParseBuildCache(%q) = %+v", value, c)
		}
	}
}
