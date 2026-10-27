package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func requireNpm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is not on PATH")
	}
}

// npmPackage is one tarball an npmRegistry serves.
type npmPackage struct {
	name, version string
	files         map[string]string // under package/
	tarball       []byte
}

func (p *npmPackage) build(t *testing.T) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range p.files {
		if err := tw.WriteHeader(&tar.Header{Name: "package/" + name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p.tarball = buf.Bytes()
}

func (p *npmPackage) integrity() string {
	sum := sha512.Sum512(p.tarball)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// npmRegistry serves packuments and tarballs for packages, records the
// tarballs fetched, and answers 404 for anything else.
type npmRegistry struct {
	*httptest.Server
	packages map[string]*npmPackage
}

func newNpmRegistry(t *testing.T, packages ...*npmPackage) *npmRegistry {
	t.Helper()
	r := &npmRegistry{packages: map[string]*npmPackage{}}
	for _, p := range packages {
		p.build(t)
		r.packages[p.name] = p
	}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name, tarball, isTarball := strings.Cut(strings.TrimPrefix(req.URL.Path, "/"), "/-/")
		p, ok := r.packages[name]
		switch {
		case ok && isTarball && tarball == p.name+"-"+p.version+".tgz":
			_, _ = w.Write(p.tarball)
		case ok && !isTarball:
			sum := sha1.Sum(p.tarball)
			manifest := map[string]any{"name": p.name, "version": p.version, "dist": map[string]string{
				"tarball": r.tarballURL(p), "integrity": p.integrity(), "shasum": hex.EncodeToString(sum[:]),
			}}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": p.name, "dist-tags": map[string]string{"latest": p.version},
				"versions": map[string]any{p.version: manifest},
			})
		default:
			http.Error(w, `{"error":"Not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(r.Close)
	t.Setenv("npm_config_registry", r.URL+"/")
	return r
}

func (r *npmRegistry) tarballURL(p *npmPackage) string {
	return r.URL + "/" + p.name + "/-/" + p.name + "-" + p.version + ".tgz"
}

// npmLockfile is a lockfileVersion 3 package-lock.json for a root package
// depending on deps, all resolved from r. A dependency r does not serve
// resolves to a tarball URL r answers 404 for.
func (r *npmRegistry) npmLockfile(t *testing.T, deps ...string) string {
	t.Helper()
	root := map[string]any{"name": "fixture", "version": "1.0.0"}
	packages := map[string]any{"": root}
	requires := map[string]string{}
	for _, dep := range deps {
		name, version, _ := strings.Cut(dep, "@")
		requires[name] = version
		entry := map[string]any{"version": version}
		if p, ok := r.packages[name]; ok {
			entry["resolved"], entry["integrity"] = r.tarballURL(p), p.integrity()
			if _, ok := p.files["binding.gyp"]; ok || strings.Contains(p.files["package.json"], "install") {
				entry["hasInstallScript"] = true
			}
		} else {
			entry["resolved"] = r.tarballURL(&npmPackage{name: name, version: version})
			entry["integrity"] = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
		}
		packages["node_modules/"+name] = entry
	}
	root["dependencies"] = requires
	data, err := json.MarshalIndent(map[string]any{"name": "fixture", "version": "1.0.0", "lockfileVersion": 3, "requires": true, "packages": packages}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

// packageJSON is a root manifest depending on deps, with extra fields.
func packageJSON(t *testing.T, extra map[string]any, deps ...string) string {
	t.Helper()
	requires := map[string]string{}
	for _, dep := range deps {
		name, version, _ := strings.Cut(dep, "@")
		requires[name] = version
	}
	manifest := map[string]any{"name": "fixture", "version": "1.0.0", "dependencies": requires}
	for k, v := range extra {
		manifest[k] = v
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

// markerScript is a node -e lifecycle script that records it ran.
func markerScript(dir, name string) string {
	path, _ := json.Marshal(filepath.Join(dir, name))
	return "node -e \"require('fs').writeFileSync(" + strings.ReplaceAll(string(path), `"`, `'`) + ",'ran')\""
}

// hostNodeEnv points HostToolchains at a synthetic HOME holding empty npm,
// pnpm, Yarn, Bun and Playwright caches, and returns those paths.
func hostNodeEnv(t *testing.T) (home string, caches map[string]string) {
	t.Helper()
	home = resolvedTempDir(t)
	caches = map[string]string{}
	for _, rel := range []string{".npm", ".local/share/pnpm/store", ".cache/yarn", ".bun/install/cache", ".cache/ms-playwright"} {
		caches[rel] = filepath.Join(home, rel)
		if err := os.MkdirAll(caches[rel], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	for _, key := range []string{"XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "npm_config_cache", "PNPM_STORE_PATH",
		"YARN_CACHE_FOLDER", "BUN_INSTALL", "BUN_INSTALL_CACHE_DIR", "PLAYWRIGHT_BROWSERS_PATH", "CYPRESS_CACHE_FOLDER"} {
		t.Setenv(key, "")
	}
	return home, caches
}

// nodeRepo commits files and returns repo and base.
func nodeRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	repo := cacheTestRepo(t)
	for name, data := range files {
		writeWorkerFixture(t, repo, name, data)
	}
	return repo, gitCommitAll(t, repo, "base")
}

type prefetchCall struct {
	dir       string
	env, argv []string
	files     map[string]string
}

// recordNodePrefetch stubs the runner, recording each call and the files
// in its working directory, and fails any call when fail is set.
func recordNodePrefetch(t *testing.T, fail error) *[]prefetchCall {
	t.Helper()
	var calls []prefetchCall
	previous := runPrefetchCommand
	runPrefetchCommand = func(_ context.Context, dir string, env, argv []string) ([]byte, []byte, error) {
		files := map[string]string{}
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				data, _ := os.ReadFile(path)
				rel, _ := filepath.Rel(dir, path)
				files[filepath.ToSlash(rel)] = string(data)
			}
			return nil
		})
		calls = append(calls, prefetchCall{dir, slices.Clone(env), slices.Clone(argv), files})
		return nil, nil, fail
	}
	t.Cleanup(func() { runPrefetchCommand = previous })
	return &calls
}

func TestNodeDependencyCacheSyntheticHome(t *testing.T) {
	berry := "__metadata:\n  version: 8\n\n\"left-pad@npm:1.3.0\":\n  version: 1.3.0\n"
	for _, tc := range []struct {
		lockfile, data string
		argv           []string
	}{
		{"package-lock.json", `{"lockfileVersion": 3, "packages": {}}`, []string{"npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund"}},
		{"pnpm-lock.yaml", "lockfileVersion: '9.0'\n", []string{"pnpm", "fetch", "--frozen-lockfile"}},
		{"yarn.lock", "# yarn lockfile v1\n\nleft-pad@1.3.0:\n  version \"1.3.0\"\n", []string{"yarn", "install", "--frozen-lockfile", "--ignore-scripts", "--non-interactive"}},
		{"yarn.lock", berry, []string{"yarn", "install", "--immutable", "--mode=skip-build"}},
	} {
		t.Run(strings.Join(tc.argv[:2], " "), func(t *testing.T) {
			_, hostCaches := hostNodeEnv(t)
			calls := recordNodePrefetch(t, nil)
			repo, base := nodeRepo(t, map[string]string{"package.json": `{"name":"fixture"}`, tc.lockfile: tc.data})
			userCache := resolvedTempDir(t)
			deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository)
			if err != nil {
				t.Fatal(err)
			}
			key, err := repositoryCacheKey(t.Context(), repo)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(userCache, "swarm", string(AgentDependencyCache), key, "node")
			if len(deps.Caches) != 1 || deps.Caches[0] != (DependencyCache{Ecosystem: "node", EnvKey: "npm_config_cache", Dir: dir}) {
				t.Fatalf("caches = %+v, want one node cache at %s", deps.Caches, dir)
			}
			if len(*calls) != 1 || !slices.Equal((*calls)[0].argv, tc.argv) {
				t.Fatalf("prefetch commands = %+v, want %q", *calls, tc.argv)
			}

			work := resolvedTempDir(t)
			flags, err := executorFlags(work, false, nil, deps, "true", nil)
			if err != nil {
				t.Fatal(err)
			}
			policy := codexPolicy("/usr/bin/codex", work, resolvedTempDir(t), false, nil, deps)
			env := nativeEnv(resolvedTempDir(t), false, deps)
			if !containsRun(flags, []string{"--ro-bind", dir, dir}) || containsRun(flags, []string{"--bind", dir, dir}) {
				t.Errorf("bwrap must bind the node cache read-only: %v", flags)
			}
			if !strings.Contains(strings.Join(policy, "\n"), strconv.Quote(dir)+`="read"`) {
				t.Errorf("codex policy does not read the node cache: %v", policy)
			}
			for _, k := range nodeCacheEnv {
				value := filepath.Join(dir, k.Subdir)
				if !containsRun(flags, []string{"--setenv", k.Key, value}) {
					t.Errorf("bwrap does not set %s=%s", k.Key, value)
				}
				if !slices.Contains(env, k.Key+"="+value) {
					t.Errorf("native env does not set %s=%s: %v", k.Key, value, env)
				}
				if !strings.Contains(strings.Join(policy, "\n"), k.Key+"="+strconv.Quote(value)) {
					t.Errorf("codex env does not set %s", k.Key)
				}
			}
			for rel, path := range hostCaches {
				for name, args := range map[string][]string{"bwrap": flags, "codex": policy, "native env": env} {
					got := strings.Contains(strings.Join(args, "\n"), path)
					if want := rel == ".cache/ms-playwright"; got != want {
						t.Errorf("%s exposes host %s = %t, want %t", name, rel, got, want)
					}
				}
			}
			if pw := hostCaches[".cache/ms-playwright"]; !containsRun(flags, []string{"--ro-bind", pw, pw}) {
				t.Errorf("Playwright browsers must stay mounted read-only: %v", flags)
			}
		})
	}
}

func TestNodePrefetchReadsBaseNotWorktree(t *testing.T) {
	hostNodeEnv(t)
	calls := recordNodePrefetch(t, nil)
	baseLock := `{"lockfileVersion": 3, "packages": {"": {"name": "base"}}}`
	repo, base := nodeRepo(t, map[string]string{"package.json": `{"name":"base"}`, "package-lock.json": baseLock})
	writeWorkerFixture(t, repo, "package-lock.json", `{"lockfileVersion": 3, "packages": {"": {"name": "candidate"}}}`)
	writeWorkerFixture(t, repo, "yarn.lock", "# yarn lockfile v1\n")
	if _, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), PerRepository); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].argv[0] != "npm" {
		t.Fatalf("prefetch commands = %+v, want only npm", *calls)
	}
	if got := (*calls)[0].files["package-lock.json"]; got != baseLock {
		t.Fatalf("scratch package-lock.json = %q, want the base bytes", got)
	}
	if insideDir(repo, (*calls)[0].dir) {
		t.Fatalf("prefetch ran inside the worktree: %s", (*calls)[0].dir)
	}
}

func TestNodePrefetchScratchExcludesRepoConfig(t *testing.T) {
	hostNodeEnv(t)
	calls := recordNodePrefetch(t, nil)
	repo, base := nodeRepo(t, map[string]string{
		"package.json":                    `{"name":"fixture","packageManager":"yarn@4.0.0","workspaces":["packages/*"]}`,
		"package-lock.json":               `{"lockfileVersion": 3, "packages": {}}`,
		".npmrc":                          "registry=https://evil.example/\n",
		".yarnrc.yml":                     "yarnPath: .yarn/releases/x.cjs\n",
		".yarn/releases/x.cjs":            "process.exit(1)\n",
		".pnpmfile.cjs":                   "module.exports = {}\n",
		"packages/a/package.json":         `{"name":"a"}`,
		"node_modules/x/package.json":     `{"name":"x"}`,
		"packages/a/node_modules/y/p.txt": "y",
		"packages/a/index.js":             "module.exports = 1\n",
	})
	if _, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), PerRepository); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("prefetch commands = %+v", *calls)
	}
	files := (*calls)[0].files
	var names []string
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"package-lock.json", "package.json", "packages/a/package.json"}; !slices.Equal(names, want) {
		t.Fatalf("scratch files = %q, want %q", names, want)
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(files["package.json"]), &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["packageManager"]; ok || root["workspaces"] == nil {
		t.Fatalf("scratch package.json = %v, want workspaces and no packageManager", root)
	}
}

func TestNodeAdapterDeclaresFetchOnly(t *testing.T) {
	eco, ok := dependencyEcosystemNamed("node")
	if !ok || eco.Prefetch == nil || eco.NoPrefetch != "" || eco.envKey() != "npm_config_cache" || !eco.NeedsManifests {
		t.Fatalf("node table entry = %+v, want a fetch-only adapter", eco)
	}
	for _, name := range nodeLockfiles {
		if !slices.Contains(eco.Detect, name) || !slices.Contains(eco.Lockfiles, name) {
			t.Errorf("node does not detect and read %s", name)
		}
	}
	for _, tc := range []struct {
		lockfile, data string
		want           []string
	}{
		{"package-lock.json", "{}", []string{"npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund"}},
		{"pnpm-lock.yaml", "lockfileVersion: '9.0'\n", []string{"pnpm", "fetch", "--frozen-lockfile"}},
		{"yarn.lock", "# yarn lockfile v1\n", []string{"yarn", "install", "--frozen-lockfile", "--ignore-scripts", "--non-interactive"}},
		{"yarn.lock", "__metadata:\n  version: 8\n", []string{"yarn", "install", "--immutable", "--mode=skip-build"}},
	} {
		if got := nodeFetchArgv(tc.lockfile, []byte(tc.data)); !slices.Equal(got, tc.want) {
			t.Errorf("nodeFetchArgv(%s) = %q, want %q", tc.lockfile, got, tc.want)
		}
	}
	if eco, ok := dependencyEcosystemNamed("node-browsers"); !ok || eco.Prefetch != nil {
		t.Error("node-browsers must be classified no-prefetch")
	}
}

func TestNodePrefetchEnvHardened(t *testing.T) {
	host := map[string]string{
		"PATH": "/usr/bin", "HOME": "/home/user", "npm_config_registry": "https://registry.example/",
		"GH_TOKEN": "t", "GITHUB_TOKEN": "t", "SWARM_GITHUB_TOKEN": "t", "SWARM_GITHUB_TOKEN_COMMAND": "t",
		"SSH_AUTH_SOCK": "/tmp/agent", "NODE_OPTIONS": "--require /tmp/x.js", "npm_config__authToken": "t", "NPM_TOKEN": "t",
		"npm_config_userconfig": "/home/user/.npmrc", "YARN_NPM_AUTH_TOKEN": "t",
	}
	tmp := resolvedTempDir(t)
	env, err := nodePrefetchEnv(func(k string) string { return host[k] }, tmp, "/cache/node")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		if _, dup := values[k]; dup {
			t.Fatalf("duplicate %s in %v", k, env)
		}
		values[k] = v
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SWARM_GITHUB_TOKEN", "SWARM_GITHUB_TOKEN_COMMAND", "SSH_AUTH_SOCK",
		"NODE_OPTIONS", "npm_config__authToken", "NPM_TOKEN", "YARN_NPM_AUTH_TOKEN"} {
		if _, ok := values[key]; ok {
			t.Errorf("prefetch env passes %s", key)
		}
	}
	for key, want := range map[string]string{
		"HOME": tmp, "TMPDIR": tmp, "npm_config_ignore_scripts": "true", "npm_config_ignore_pnpmfile": "true",
		"YARN_ENABLE_SCRIPTS": "0", "YARN_IGNORE_PATH": "1", "YARN_ENABLE_GLOBAL_CACHE": "0",
		"COREPACK_ENABLE_NETWORK": "0", "COREPACK_ENABLE_AUTO_PIN": "0",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
		"npm_config_cache": "/cache/node/npm", "npm_config_store_dir": "/cache/node/pnpm-store", "YARN_CACHE_FOLDER": "/cache/node/yarn",
		"npm_config_registry": "https://registry.example/",
	} {
		if got, ok := values[key]; !ok || got != want {
			t.Errorf("%s = %q (set %t), want %q", key, got, ok, want)
		}
	}
	for _, key := range []string{"npm_config_userconfig", "npm_config_globalconfig"} {
		if !strings.HasPrefix(values[key], tmp+string(filepath.Separator)) {
			t.Errorf("%s = %q, want a file in the scratch HOME", key, values[key])
		} else if data, err := os.ReadFile(values[key]); err != nil || len(data) != 0 {
			t.Errorf("%s is not an empty file: %q %v", key, data, err)
		}
	}
	if _, ok := values["YARN_NPM_REGISTRY_SERVER"]; ok {
		t.Error("an unset registry must not be passed")
	}
}

// hookPackage has every npm lifecycle script and a binding.gyp, each of
// which would write a marker into markers.
func hookPackage(t *testing.T, markers string) *npmPackage {
	t.Helper()
	scripts := map[string]string{}
	for _, s := range []string{"preinstall", "install", "postinstall", "prepare"} {
		scripts[s] = markerScript(markers, "hook-"+s)
	}
	manifest, err := json.Marshal(map[string]any{"name": "hook", "version": "1.0.0", "scripts": scripts})
	if err != nil {
		t.Fatal(err)
	}
	return &npmPackage{name: "hook", version: "1.0.0", files: map[string]string{
		"package.json": string(manifest),
		"binding.gyp":  `{"targets": [{"target_name": "hook", "sources": ["hook.cc"]}]}`,
		"index.js":     "module.exports = 1\n",
	}}
}

func TestNodePrefetchRunsNoPackageCode(t *testing.T) {
	requireNpm(t)
	hostNodeEnv(t)
	markers := resolvedTempDir(t)
	registry := newNpmRegistry(t, hookPackage(t, markers))
	root := packageJSON(t, map[string]any{"scripts": map[string]string{
		"preinstall": markerScript(markers, "root-preinstall"), "prepare": markerScript(markers, "root-prepare"),
	}}, "hook@1.0.0")
	lock := registry.npmLockfile(t, "hook@1.0.0")
	repo, base := nodeRepo(t, map[string]string{"package.json": root, "package-lock.json": lock})
	userCache := resolvedTempDir(t)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps.Caches) != 1 {
		t.Fatalf("caches = %+v", deps.Caches)
	}
	if entries, _ := os.ReadDir(markers); len(entries) != 0 {
		var ran []string
		for _, e := range entries {
			ran = append(ran, e.Name())
		}
		t.Fatalf("prefetch ran package code: %q", ran)
	}
	if _, err := os.Stat(filepath.Join(deps.Caches[0].Dir, "npm", "_cacache")); err != nil {
		t.Fatalf("npm cache was not filled: %v", err)
	}

	// Control: the same fixture with scripts enabled does write markers, so
	// the assertion above can fail.
	project := resolvedTempDir(t)
	writeWorkerFixture(t, project, "package.json", root)
	writeWorkerFixture(t, project, "package-lock.json", lock)
	scratch := resolvedTempDir(t)
	env, err := nodePrefetchEnv(os.Getenv, scratch, resolvedTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	env = slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, "npm_config_ignore_scripts=") })
	cmd := exec.Command("npm", "ci", "--no-audit", "--no-fund")
	cmd.Dir, cmd.Env = project, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("control npm ci: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(markers, "root-preinstall")); err != nil {
		t.Fatalf("control run with scripts enabled wrote no marker: %v", err)
	}
}

func TestNodeDependencyAbsentFromHostIsPrefetched(t *testing.T) {
	requireNpm(t)
	_, hostCaches := hostNodeEnv(t)
	fresh := &npmPackage{name: "fresh", version: "1.0.0", files: map[string]string{
		"package.json": `{"name":"fresh","version":"1.0.0"}`, "index.js": "module.exports = 'fresh'\n",
	}}
	registry := newNpmRegistry(t, fresh)
	root, lock := packageJSON(t, nil, "fresh@1.0.0"), registry.npmLockfile(t, "fresh@1.0.0")
	repo, base := nodeRepo(t, map[string]string{"package.json": root, "package-lock.json": lock})
	userCache := resolvedTempDir(t)
	makeWritableOnCleanup(t, userCache)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	dir := deps.Caches[0].Dir
	if !cacheIndexMentions(t, filepath.Join(dir, "npm", "_cacache", "index-v5"), registry.tarballURL(fresh)) {
		t.Fatalf("per-repository cache does not hold %s", registry.tarballURL(fresh))
	}
	if entries, _ := os.ReadDir(hostCaches[".npm"]); len(entries) != 0 {
		t.Fatalf("prefetch wrote the host npm cache: %v", entries)
	}

	// The agent installs offline from the read-only cache with only the
	// sandbox environment.
	registry.Close()
	_ = filepath.WalkDir(filepath.Join(userCache, "swarm"), func(path string, d os.DirEntry, err error) error {
		if err == nil {
			mode := os.FileMode(0o400)
			if d.IsDir() {
				mode = 0o500
			}
			_ = os.Chmod(path, mode)
		}
		return nil
	})
	project, home := resolvedTempDir(t), resolvedTempDir(t)
	writeWorkerFixture(t, project, "package.json", root)
	writeWorkerFixture(t, project, "package-lock.json", lock)
	cmd := exec.Command("npm", "ci", "--ignore-scripts")
	cmd.Dir, cmd.Env = project, nativeEnv(home, false, deps)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("offline npm ci from the read-only cache: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(project, "node_modules", "fresh", "index.js")); err != nil {
		t.Fatalf("fresh was not installed: %v", err)
	}
}

// cacheIndexMentions reports whether any npm cacache index file holds s.
func cacheIndexMentions(t *testing.T, index, s string) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(index, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if data, _ := os.ReadFile(path); bytes.Contains(data, []byte(s)) {
				found = true
			}
		}
		return nil
	})
	return found
}

func TestNodePrefetchFailureNamesDependency(t *testing.T) {
	requireNpm(t)
	hostNodeEnv(t)
	registry := newNpmRegistry(t)
	repo, base := nodeRepo(t, map[string]string{
		"package.json": packageJSON(t, nil, "absent@1.0.0"), "package-lock.json": registry.npmLockfile(t, "absent@1.0.0"),
	})
	userCache := resolvedTempDir(t)
	_, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository)
	if err == nil {
		t.Fatal("a dependency missing from the registry must fail the prefetch")
	}
	t.Logf("preflight error: %v", err)
	for _, want := range []string{"prefetch node", "absent@1.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), userCache) {
		t.Errorf("error %q exposes the user cache path", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(userCache, "swarm", "*", "*", ".prefetched-*")); len(matches) != 0 {
		t.Fatalf("failed prefetch recorded %v", matches)
	}
}

func TestNodePrefetchFailureParsesManagers(t *testing.T) {
	for _, tc := range []struct {
		name, stderr, want string
	}{
		{"npm E404", "npm ERR! code E404\nnpm ERR! 404 Not Found - GET https://registry.npmjs.org/absent/-/absent-1.0.0.tgz - Not found\n", "absent@1.0.0"},
		{"npm scoped E404", "npm ERR! 404 Not Found - GET https://r.example/@scope/my-pkg/-/my-pkg-2.1.0-rc.1.tgz - Not found\n", "@scope/my-pkg@2.1.0-rc.1"},
		{"npm not in registry", "npm ERR! 404  'absent@1.0.0' is not in this registry.\n", "absent@1.0.0"},
		{"npm ETARGET", "npm ERR! code ETARGET\nnpm ERR! notarget No matching version found for left-pad@9.9.9.\n", "left-pad@9.9.9"},
		{"pnpm", " ERR_PNPM_FETCH_404  GET https://registry.npmjs.org/absent/-/absent-1.0.0.tgz: Not Found - 404\n", "absent@1.0.0"},
		{"yarn", "error Couldn't find package \"absent@1.0.0\" required by \"fixture@1.0.0\" on the \"npm\" registry.\n", "absent@1.0.0"},
		{"other", "npm ERR! code EACCES\n", "npm ERR! code EACCES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodePrefetchFailure(nil, []byte(tc.stderr), errors.New("exit status 1")); !strings.HasPrefix(got, tc.want) {
				t.Fatalf("nodePrefetchFailure = %q, want %q first", got, tc.want)
			}
		})
	}
	if got := nodePrefetchFailure(nil, nil, errors.New("exit status 1")); got != "exit status 1" {
		t.Fatalf("empty output = %q", got)
	}
}

func TestNodePrefetchRefusesGitDependencies(t *testing.T) {
	for _, tc := range []struct {
		name, lockfile, data, want string
	}{
		{"npm git+ssh", "package-lock.json", `{
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "fixture"},
    "node_modules/gitdep": {
      "version": "1.0.0",
      "resolved": "git+ssh://git@github.com/o/gitdep.git#0123456789abcdef"
    }
  }
}`, "gitdep"},
		{"npm github shorthand", "package-lock.json", `{
  "packages": {
    "": {
      "dependencies": {
        "shorthand": "github:o/shorthand"
      }
    }
  }
}`, "shorthand"},
		{"pnpm git resolution", "pnpm-lock.yaml", `lockfileVersion: '9.0'
packages:
  pnpmgit@https://codeload.github.com/o/pnpmgit/tar.gz/0123:
    resolution: {tarball: https://codeload.github.com/o/pnpmgit/tar.gz/0123}
  repodep@git+https://github.com/o/repodep.git#0123:
    resolution: {commit: 0123, repo: https://github.com/o/repodep.git, type: git}
`, "pnpmgit"},
		{"yarn berry commit", "yarn.lock", `__metadata:
  version: 8

"berrydep@https://github.com/o/berrydep.git#commit=0123":
  version: 1.0.0
  resolution: "berrydep@https://github.com/o/berrydep.git#commit=0123"
`, "berrydep"},
		{"yarn v1 resolved", "yarn.lock", `# yarn lockfile v1

v1dep@^1.0.0:
  version "1.0.0"
  resolved "git+https://github.com/o/v1dep.git#0123"
`, "v1dep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostNodeEnv(t)
			calls := recordNodePrefetch(t, nil)
			repo, base := nodeRepo(t, map[string]string{"package.json": `{"name":"fixture"}`, tc.lockfile: tc.data})
			_, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), PerRepository)
			if err == nil || !strings.Contains(err.Error(), "git-hosted") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want a git-hosted refusal naming %s", err, tc.want)
			}
			if len(*calls) != 0 {
				t.Fatalf("prefetch ran %+v before refusing", *calls)
			}
		})
	}
	// Registry dependencies, including integrity hashes containing
	// "git+", are not refused.
	clean := map[string][]byte{"package-lock.json": []byte(`{
  "packages": {
    "node_modules/left-pad": {
      "version": "1.3.0",
      "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz",
      "integrity": "sha512-agit+Aw=="
    }
  }
}`), "pnpm-lock.yaml": []byte("packages:\n  left-pad@1.3.0:\n    resolution: {integrity: sha512-xgit+y==}\n")}
	if refused := nodeGitDependencies(clean); len(refused) != 0 {
		t.Fatalf("registry dependencies refused: %q", refused)
	}
}

func TestNodePrefetchMarkerSkipsRepeat(t *testing.T) {
	hostNodeEnv(t)
	calls := recordNodePrefetch(t, nil)
	repo, base := nodeRepo(t, map[string]string{"package.json": `{"name":"fixture"}`, "package-lock.json": "{}"})
	userCache := resolvedTempDir(t)
	prepare := func(base string) {
		t.Helper()
		if _, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository); err != nil {
			t.Fatal(err)
		}
	}
	prepare(base)
	prepare(base)
	if len(*calls) != 1 {
		t.Fatalf("unchanged lockfiles prefetched %d times", len(*calls))
	}
	writeWorkerFixture(t, repo, "README.md", "unrelated\n")
	prepare(gitCommitAll(t, repo, "docs"))
	if len(*calls) != 1 {
		t.Fatal("a change outside the manifests prefetched again")
	}
	writeWorkerFixture(t, repo, "packages/a/package.json", `{"name":"a"}`)
	prepare(gitCommitAll(t, repo, "workspace"))
	if len(*calls) != 2 {
		t.Fatalf("a new workspace manifest must prefetch again, got %d", len(*calls))
	}
}

func TestNodePrefetchFailureStopsWithoutMarker(t *testing.T) {
	hostNodeEnv(t)
	recordNodePrefetch(t, errors.New("exit status 1"))
	repo, base := nodeRepo(t, map[string]string{"package.json": `{"name":"fixture"}`, "pnpm-lock.yaml": "lockfileVersion: '9.0'\n"})
	userCache := resolvedTempDir(t)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, userCache, PerRepository)
	if err == nil || len(deps.Caches) != 0 || !strings.Contains(err.Error(), "prefetch node") || !strings.Contains(err.Error(), "pnpm-lock.yaml") {
		t.Fatalf("PrepareDependencyCaches = %+v, %v; want a preflight error and no cache", deps, err)
	}
}
