package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const privateLib = "example.com/priv/lib@v1.0.0"

// privateHostEnv marks example.com/priv private on the host and sets the
// credentials a user's shell would have, including tokens the prefetch must
// never pass on.
func privateHostEnv(t *testing.T) (home string) {
	t.Helper()
	home = resolvedTempDir(t)
	for key, value := range map[string]string{
		"HOME": home, "GOPRIVATE": "example.com/priv", "GONOPROXY": "", "GONOSUMDB": "", "GOFLAGS": "",
		"SSH_AUTH_SOCK": "/tmp/ssh-agent.sock", "NETRC": filepath.Join(home, ".netrc"), "GOAUTH": "netrc",
		"GH_TOKEN": "gh-secret", "GITHUB_TOKEN": "github-secret", "SWARM_GITHUB_TOKEN": "swarm-secret",
	} {
		t.Setenv(key, value)
	}
	return home
}

type prefetchCall struct {
	dir  string
	env  map[string]string
	argv []string
}

// recordPrefetch stubs the prefetch runner. A download of named modules
// extracts them into GOMODCACHE, as the go command would.
func recordPrefetch(t *testing.T) *[]prefetchCall {
	t.Helper()
	var calls []prefetchCall
	stubPrefetch(t, "go1.21.0", func(dir string, env, argv []string) {
		values := map[string]string{}
		for _, entry := range env {
			k, v, _ := strings.Cut(entry, "=")
			values[k] = v
		}
		calls = append(calls, prefetchCall{dir: dir, env: values, argv: argv})
		if len(argv) > len(goModDownloadArgv) && slices.Equal(argv[:len(goModDownloadArgv)], goModDownloadArgv) {
			for _, mod := range argv[len(goModDownloadArgv):] {
				dir := filepath.Join(values["GOMODCACHE"], filepath.FromSlash(mod))
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				writeWorkerFixture(t, dir, "go.mod", "module "+strings.Split(mod, "@")[0]+"\n")
			}
		}
	})
	return &calls
}

func downloads(calls []prefetchCall) []prefetchCall {
	var out []prefetchCall
	for _, c := range calls {
		if slices.Equal(c.argv[:min(len(c.argv), len(goModDownloadArgv))], goModDownloadArgv) {
			out = append(out, c)
		}
	}
	return out
}

func allowPrivate(repo string, patterns ...string) DependencyPolicy {
	return DependencyPolicy{Mode: PerRepository, PrivateGo: PrivateGo{Repository: repo, Patterns: patterns}}
}

func TestPrivateGoModulePreflight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gomod   string
		gosum   string
		allow   PrivateGo
		wantErr []string
	}{
		{"no allowance", goModRequiring(privateLib), "", PrivateGo{Repository: "acme/a"}, []string{privateLib, `private_go_modules["acme/a"]`}},
		{"unrelated pattern", goModRequiring(privateLib), "", PrivateGo{Repository: "acme/a", Patterns: []string{"example.com/other"}}, []string{privateLib, "private_go_modules"}},
		{"unregistered repository", goModRequiring(privateLib), "", PrivateGo{}, []string{privateLib, "register this repository", "private_go_modules"}},
		{"allowed", goModRequiring(privateLib), "", PrivateGo{Repository: "acme/a", Patterns: []string{"example.com/priv"}}, nil},
		{"transitive in go.sum", goModRequiring(), "example.com/priv/dep v1.2.0/go.mod h1:x=\n", PrivateGo{Repository: "acme/a"}, []string{"example.com/priv/dep@v1.2.0", "private_go_modules"}},
		{"public module", goModRequiring("example.com/public@v1.0.0"), "", PrivateGo{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			privateHostEnv(t)
			calls := recordPrefetch(t)
			repo := cacheTestRepo(t)
			writeWorkerFixture(t, repo, "go.mod", tc.gomod)
			if tc.gosum != "" {
				writeWorkerFixture(t, repo, "go.sum", tc.gosum)
			}
			base := gitCommitAll(t, repo, "base")
			_, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), DependencyPolicy{Mode: PerRepository, PrivateGo: tc.allow})
			if tc.wantErr == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("a private module without a matching allowance must fail")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if len(*calls) != 0 {
				t.Errorf("preflight failure still ran %v", (*calls)[0].argv)
			}
		})
	}
}

func TestPrivateGoModuleFetchedWithCredentials(t *testing.T) {
	home := privateHostEnv(t)
	calls := recordPrefetch(t)
	repo, base := goModuleRepo(t, privateLib, "example.com/public@v1.0.0")
	userCache := resolvedTempDir(t)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, allowPrivate("acme/a", "example.com/priv"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := repositoryCacheKey(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(userCache, "swarm", string(AgentDependencyCache), key, "go")
	if len(deps.Caches) != 1 || deps.Caches[0].Dir != cache {
		t.Fatalf("caches = %+v, want %s", deps.Caches, cache)
	}
	got := downloads(*calls)
	if len(got) != 2 {
		t.Fatalf("downloads = %v, want the private fetch then the full download", got)
	}
	private, full := got[0], got[1]
	if want := append(slices.Clone(goModDownloadArgv), privateLib); !slices.Equal(private.argv, want) {
		t.Fatalf("credentialed fetch argv = %q, want only the allowed module %q", private.argv, want)
	}
	for key, want := range map[string]string{
		"HOME": home, "SSH_AUTH_SOCK": "/tmp/ssh-agent.sock", "GOAUTH": "netrc", "GOPROXY": privateGoProxy,
		"GOMODCACHE": cache, "GOPRIVATE": "example.com/priv", "GONOPROXY": "none", "GONOSUMDB": "example.com/priv",
		"GOTOOLCHAIN": "local", "GOFLAGS": "", "GIT_TERMINAL_PROMPT": "0",
	} {
		if v, ok := private.env[key]; !ok || v != want {
			t.Errorf("credentialed fetch %s = %q (set %t), want %q", key, v, ok, want)
		}
	}
	if !slices.Equal(full.argv, goModDownloadArgv) {
		t.Fatalf("full download argv = %q", full.argv)
	}
	for _, key := range []string{"SSH_AUTH_SOCK", "NETRC", "GOAUTH"} {
		if _, ok := full.env[key]; ok {
			t.Errorf("credential-free download passes %s", key)
		}
	}
	if full.env["HOME"] == home || full.env["GIT_CONFIG_GLOBAL"] != os.DevNull {
		t.Errorf("credential-free download uses the user's HOME or git config: %v", full.env)
	}
	for _, key := range []string{"GOPRIVATE", "GONOPROXY", "GONOSUMDB"} {
		if !slices.Contains(strings.Split(full.env[key], ","), "example.com/priv") {
			t.Errorf("credential-free download %s = %q, want the allowed pattern", key, full.env[key])
		}
	}
	if !moduleExtracted(cache, privateLib) {
		t.Fatal("allowed private module is not in the repository's cache")
	}
}

func TestPrivateGoCredentialsNeverReachSandbox(t *testing.T) {
	home := privateHostEnv(t)
	calls := recordPrefetch(t)
	repo, base := goModuleRepo(t, privateLib)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), allowPrivate("acme/a", "example.com/priv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range downloads(*calls) {
		for key := range c.env {
			if key == "GH_TOKEN" || key == "GITHUB_TOKEN" || strings.HasPrefix(key, "SWARM_") {
				t.Errorf("prefetch %q passes %s", c.argv, key)
			}
		}
	}
	work := resolvedTempDir(t)
	flags, err := executorFlags(work, false, nil, deps, "true", nil)
	if err != nil {
		t.Fatal(err)
	}
	surfaces := map[string][]string{
		"dependency env": DependencyCacheEnv(deps.Caches),
		"bwrap":          flags,
		"codex":          codexPolicy("/usr/bin/codex", work, resolvedTempDir(t), false, nil, deps),
		"native env":     nativeEnv(resolvedTempDir(t), false, deps),
		"internal tools": internalToolArgs(ToolExecutor{Work: work, Dependencies: deps}),
	}
	for name, args := range surfaces {
		joined := strings.Join(args, "\n")
		for _, secret := range []string{"SSH_AUTH_SOCK", "/tmp/ssh-agent.sock", "NETRC", "GOAUTH", "GIT_CONFIG", home, "example.com/priv", "secret"} {
			if strings.Contains(joined, secret) {
				t.Errorf("%s exposes %q: %s", name, secret, joined)
			}
		}
	}
}

func TestPrivateGoModulesStayInOneRepository(t *testing.T) {
	privateHostEnv(t)
	recordPrefetch(t)
	userCache := resolvedTempDir(t)
	repoA, baseA := goModuleRepo(t, privateLib)
	repoB, baseB := goModuleRepo(t, privateLib)
	depsA, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repoA, baseA, userCache, allowPrivate("acme/a", "example.com/priv"))
	if err != nil {
		t.Fatal(err)
	}
	if !moduleExtracted(depsA.Caches[0].Dir, privateLib) {
		t.Fatal("repository A's cache lacks its allowed module")
	}
	// B names the same module, but the allowance is A's.
	_, err = PrepareDependencyCaches(t.Context(), AgentDependencyCache, repoB, baseB, userCache, DependencyPolicy{Mode: PerRepository, PrivateGo: PrivateGo{Repository: "acme/b"}})
	if err == nil || !strings.Contains(err.Error(), privateLib) || !strings.Contains(err.Error(), `private_go_modules["acme/b"]`) {
		t.Fatalf("repository B error = %v, want the private module preflight error", err)
	}
	keyB, err := repositoryCacheKey(t.Context(), repoB)
	if err != nil {
		t.Fatal(err)
	}
	dirB := filepath.Join(userCache, "swarm", string(AgentDependencyCache), keyB, "go")
	if dirB == depsA.Caches[0].Dir || moduleExtracted(dirB, privateLib) {
		t.Fatalf("repository B's cache %s holds A's private module", dirB)
	}
}

func TestWithdrawnOptInFailsWithWarmCache(t *testing.T) {
	privateHostEnv(t)
	calls := recordPrefetch(t)
	repo, base := goModuleRepo(t, privateLib)
	userCache := resolvedTempDir(t)
	allowed := allowPrivate("acme/a", "example.com/priv")
	for range 2 {
		if _, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, allowed); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(downloads(*calls)); n != 2 {
		t.Fatalf("a filled cache downloaded again: %d downloads", n)
	}
	markers, _ := filepath.Glob(filepath.Join(userCache, "swarm", "*", "*", ".prefetched-*"))
	if len(markers) != 1 {
		t.Fatalf("markers = %v", markers)
	}
	_, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, DependencyPolicy{Mode: PerRepository, PrivateGo: PrivateGo{Repository: "acme/a"}})
	if err == nil || !strings.Contains(err.Error(), "private_go_modules") {
		t.Fatalf("withdrawn allowance with a warm cache = %v, want the preflight error", err)
	}
}

func TestPrefetchMarkerUnchangedWithoutAllowance(t *testing.T) {
	files := map[string][]byte{"go.mod": []byte(goModRequiring())}
	if prefetchDigest(files, nil) != lockfileDigest(files) {
		t.Fatal("caches without an allowance must keep their markers")
	}
	if prefetchDigest(files, []string{"a.com/x", "b.com/y"}) != prefetchDigest(files, []string{"b.com/y", "a.com/x"}) {
		t.Fatal("pattern order must not change the marker")
	}
	if prefetchDigest(files, []string{"a.com/x"}) == lockfileDigest(files) {
		t.Fatal("an allowance must change the marker")
	}
}

// TestPrivateGoEndToEnd runs the real go command. The credentialed phase
// reads a file:// fixture proxy; the proxy is removed before the
// credential-free download, which must then find the module in the
// repository's cache rather than fetch it again.
func TestPrivateGoEndToEnd(t *testing.T) {
	requireGo(t)
	hostGoEnv(t, "off")
	t.Setenv("GOPRIVATE", "example.com/priv")
	proxy := resolvedTempDir(t)
	writeModuleProxy(t, proxy, privateLib)
	previousProxy := privateGoProxy
	privateGoProxy = "file://" + filepath.ToSlash(proxy)
	t.Cleanup(func() { privateGoProxy = previousProxy })
	previous := runPrefetchCommand
	var fetched []string
	runPrefetchCommand = func(ctx context.Context, dir string, env, argv []string) ([]byte, []byte, error) {
		stdout, stderr, err := previous(ctx, dir, env, argv)
		if len(argv) > len(goModDownloadArgv) && slices.Contains(argv, privateLib) {
			fetched = append(fetched, privateLib)
			if rmErr := os.RemoveAll(proxy); rmErr != nil {
				t.Error(rmErr)
			}
		}
		return stdout, stderr, err
	}
	t.Cleanup(func() { runPrefetchCommand = previous })
	repo, base := goModuleRepo(t, privateLib)
	userCache := resolvedTempDir(t)
	makeWritableOnCleanup(t, userCache)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, allowPrivate("acme/a", "example.com/priv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 || !moduleExtracted(deps.Caches[0].Dir, privateLib) {
		t.Fatalf("private module fetched %d times; in cache: %t", len(fetched), moduleExtracted(deps.Caches[0].Dir, privateLib))
	}
}
