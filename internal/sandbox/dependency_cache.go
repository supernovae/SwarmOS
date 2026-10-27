package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/safefs"
)

// dependencyEcosystem classifies one ecosystem's downloaded dependencies.
// An adapter-backed entry has a fetch-only Prefetch: its host-wide cache is
// never mounted, and each repository gets its own cache filled on the host
// from the approved base. A NoPrefetch entry keeps today's read-only host
// cache mounts, because resolving its dependencies can run package code.
type dependencyEcosystem struct {
	Name       string   // also the ToolchainBind.Ecosystem tag
	Detect     []string // base-tree files that enable the adapter
	Lockfiles  []string // read from the approved base, never the worktree
	Env        []cacheEnvKey
	Prefetch   prefetchFunc
	NoPrefetch string // why there is no fetch-only prefetch
	// NeedsManifests also reads every package manifest in the base tree.
	NeedsManifests bool
}

// cacheEnvKey points one environment variable at Subdir of the
// ecosystem's cache. The first key identifies the cache.
type cacheEnvKey struct{ Key, Subdir string }

// prefetchRequest is one host prefetch. Dir is the only directory it may
// write; Files holds the lockfiles present in the approved base, and
// Manifests every manifest by base-tree path when the adapter needs them.
type prefetchRequest struct {
	BaseSHA   string
	Dir       string
	Files     map[string][]byte
	Manifests map[string][]byte
}

type prefetchFunc func(context.Context, prefetchRequest) error

// dependencyEcosystems is the only source of dependency cache environment
// keys and classifies every ecosystem resolveHostToolchains mounts. Adding an
// entry is covered by TestDependencyTableSafety and
// TestEveryMountedEcosystemIsClassified; see "Dependency caches" in
// docs/project.md.
var dependencyEcosystems = []dependencyEcosystem{
	{Name: "go", Detect: []string{"go.mod"}, Lockfiles: []string{"go.mod", "go.sum"}, Env: []cacheEnvKey{{Key: "GOMODCACHE"}}, Prefetch: goModDownload},
	{Name: "rust", NoPrefetch: "adapter is a follow-up"},
	{Name: "python", NoPrefetch: "adapter is a follow-up"},
	{Name: "node", Detect: nodeLockfiles, Lockfiles: append(slices.Clone(nodeLockfiles), "package.json", "pnpm-workspace.yaml"), Env: nodeCacheEnv, Prefetch: nodePrefetch, NeedsManifests: true},
	{Name: "node-browsers", NoPrefetch: "browser binaries, not lockfile packages"},
	{Name: "dotnet", NoPrefetch: "restore runs MSBuild"},
	{Name: "maven", NoPrefetch: "plugins run during resolve"},
	{Name: "gradle", NoPrefetch: "build scripts run during resolve"},
	{Name: "c-cpp", NoPrefetch: "vcpkg ports run build scripts"},
}

// Dependency caches follow the build cache ownership rule: agents and the
// validator never share a directory. Only the host prefetch writes either.
const (
	AgentDependencyCache     BuildCacheOwner = "agent-deps"
	ValidatorDependencyCache BuildCacheOwner = "validate-deps"
)

// DependencyCacheMode selects how sandboxes see adapter-backed dependency
// caches. Only the node profile can choose SharedHostCaches; the zero value
// is PerRepository.
type DependencyCacheMode string

const (
	PerRepository    DependencyCacheMode = "per-repository"
	SharedHostCaches DependencyCacheMode = "host-shared"
)

// DependencyCache is one ecosystem's per-repository cache, mounted read-only.
type DependencyCache struct{ Ecosystem, EnvKey, Dir string }

// Dependencies is one run's view of dependency caches.
type Dependencies struct {
	Mode   DependencyCacheMode
	Caches []DependencyCache
}

// toolchains returns the host toolchain binds this run may mount.
func (d Dependencies) toolchains() []ToolchainBind {
	return HostToolchainsFor(d.Mode)
}

// HostToolchainsFor is HostToolchains without the host-wide caches of
// adapter-backed ecosystems, unless mode shares them.
func HostToolchainsFor(mode DependencyCacheMode) []ToolchainBind {
	return filterDependencyBinds(HostToolchains(), mode)
}

func filterDependencyBinds(binds []ToolchainBind, mode DependencyCacheMode) []ToolchainBind {
	if mode == SharedHostCaches {
		return binds
	}
	filtered := make([]ToolchainBind, 0, len(binds))
	for _, b := range binds {
		if eco, ok := dependencyEcosystemNamed(b.Ecosystem); ok && eco.Prefetch != nil {
			continue
		}
		filtered = append(filtered, b)
	}
	return filtered
}

func dependencyEcosystemNamed(name string) (dependencyEcosystem, bool) {
	for _, eco := range dependencyEcosystems {
		if name != "" && eco.Name == name {
			return eco, true
		}
	}
	return dependencyEcosystem{}, false
}

// envKey is the key that identifies an ecosystem's cache.
func (eco dependencyEcosystem) envKey() string {
	if len(eco.Env) == 0 {
		return ""
	}
	return eco.Env[0].Key
}

// env returns the cache's KEY=dir entries from the table, one per key.
func (c DependencyCache) env() []string {
	eco, ok := dependencyEcosystemNamed(c.Ecosystem)
	if !ok || eco.envKey() != c.EnvKey {
		return []string{c.EnvKey + "=" + c.Dir}
	}
	env := make([]string, 0, len(eco.Env))
	for _, k := range eco.Env {
		env = append(env, k.Key+"="+filepath.Join(c.Dir, k.Subdir))
	}
	return env
}

// DependencyCacheEnv returns KEY=dir entries for caches.
func DependencyCacheEnv(caches []DependencyCache) []string {
	env := make([]string, 0, len(caches))
	for _, c := range caches {
		env = append(env, c.env()...)
	}
	return env
}

// checkDependencyCache accepts only a table key for its ecosystem, at an
// absolute directory outside the workspace.
func checkDependencyCache(c DependencyCache, work string) error {
	eco, ok := dependencyEcosystemNamed(c.Ecosystem)
	if !ok || eco.Prefetch == nil || eco.envKey() != c.EnvKey {
		return fmt.Errorf("dependency cache %q has no table entry", c.EnvKey)
	}
	if !filepath.IsAbs(c.Dir) || filepath.Clean(c.Dir) == "/" {
		return fmt.Errorf("dependency cache %s: %q is not an absolute directory", c.Ecosystem, c.Dir)
	}
	if work != "" && (insideDir(work, c.Dir) || insideDir(c.Dir, work)) {
		return fmt.Errorf("dependency cache %s overlaps the workspace", c.Ecosystem)
	}
	return nil
}

// ParseDependencyCache reads an internal-tools --dependency-cache value,
// ecosystem=dir. The environment key always comes from the table.
func ParseDependencyCache(value, workspace string) (DependencyCache, error) {
	name, dir, ok := strings.Cut(value, "=")
	if !ok {
		return DependencyCache{}, fmt.Errorf("dependency cache %q: want ecosystem=dir", value)
	}
	eco, found := dependencyEcosystemNamed(name)
	if !found || eco.Prefetch == nil {
		return DependencyCache{}, fmt.Errorf("unknown dependency cache ecosystem %q", name)
	}
	c := DependencyCache{Ecosystem: name, EnvKey: eco.envKey(), Dir: filepath.Clean(dir)}
	if !filepath.IsAbs(dir) {
		return DependencyCache{}, fmt.Errorf("dependency cache %s: %q is not an absolute directory", name, dir)
	}
	if err := checkDependencyCache(c, workspace); err != nil {
		return DependencyCache{}, err
	}
	return c, nil
}

func dependencyCacheFlag(c DependencyCache) string { return c.Ecosystem + "=" + c.Dir }

// EcosystemMode is one line of doctor and node status output.
type EcosystemMode struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

// DependencyCacheModes reports how each classified ecosystem's dependencies
// reach sandboxes under mode.
func DependencyCacheModes(mode DependencyCacheMode) []EcosystemMode {
	modes := make([]EcosystemMode, 0, len(dependencyEcosystems))
	for _, eco := range dependencyEcosystems {
		m := "host-shared (no fetch-only prefetch: " + eco.NoPrefetch + ")"
		if eco.Prefetch != nil {
			m = "per-repository"
			if mode == SharedHostCaches {
				m = "host-shared (opt-in)"
			}
		}
		modes = append(modes, EcosystemMode{Name: eco.Name, Mode: m})
	}
	return modes
}

// PrepareDependencyCaches fills owner's per-repository cache for every
// adapter-backed ecosystem detected in baseSHA, under
// <userCache>/swarm/<owner>/<repo-key>/<ecosystem>. Unlike build caches it
// never falls back: any failure is an error, so callers stop before work
// starts. SharedHostCaches prepares nothing.
func PrepareDependencyCaches(ctx context.Context, owner BuildCacheOwner, worktree, baseSHA, userCache string, mode DependencyCacheMode) (Dependencies, error) {
	deps := Dependencies{Mode: mode}
	if mode == SharedHostCaches {
		return deps, nil
	}
	deps.Mode = PerRepository
	if owner != AgentDependencyCache && owner != ValidatorDependencyCache {
		return deps, fmt.Errorf("unknown dependency cache owner %q", owner)
	}
	if baseSHA == "" {
		return deps, errors.New("dependency caches need the approved base commit")
	}
	if _, err := gitcredential.Output(ctx, worktree, "rev-parse", "--verify", "--quiet", baseSHA+"^{commit}"); err != nil {
		return deps, fmt.Errorf("read approved base %s: %w", baseSHA, err)
	}
	var detected []dependencyEcosystem
	for _, eco := range dependencyEcosystems {
		if eco.Prefetch != nil && baseHasAny(ctx, worktree, baseSHA, eco.Detect) {
			detected = append(detected, eco)
		}
	}
	if len(detected) == 0 {
		return deps, nil
	}
	if userCache == "" || !filepath.IsAbs(userCache) {
		return deps, errors.New("dependency caches need an absolute user cache directory")
	}
	base := filepath.Clean(userCache)
	key, err := repositoryCacheKey(ctx, worktree)
	if err != nil {
		return deps, err
	}
	keyRel := filepath.Join("swarm", string(owner), key)
	keyDir := filepath.Join(base, keyRel)
	if insideDir(worktree, keyDir) {
		return deps, errors.New("dependency cache directory is inside the worktree")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return deps, fmt.Errorf("prepare dependency cache: %w", err)
	}
	root, err := safefs.Open(base)
	if err != nil {
		return deps, fmt.Errorf("prepare dependency cache: %w", err)
	}
	defer root.Close()
	for _, eco := range detected {
		if err := privateCacheDir(root, filepath.Join(keyRel, eco.Name)); err != nil {
			return deps, fmt.Errorf("prepare %s dependency cache: %w", eco.Name, err)
		}
		dir := filepath.Join(keyDir, eco.Name)
		files := map[string][]byte{}
		for _, name := range eco.Lockfiles {
			if !baseHasAny(ctx, worktree, baseSHA, []string{name}) {
				continue
			}
			data, err := gitcredential.Output(ctx, worktree, "cat-file", "blob", baseSHA+":"+name)
			if err != nil {
				return deps, fmt.Errorf("read %s from approved base: %w", name, err)
			}
			files[name] = data
		}
		digested := files
		var manifests map[string][]byte
		if eco.NeedsManifests {
			if manifests, err = baseManifests(ctx, worktree, baseSHA); err != nil {
				return deps, fmt.Errorf("read %s manifests from approved base: %w", eco.Name, err)
			}
			digested = maps.Clone(files)
			for name, data := range manifests {
				digested["manifest:"+name] = data
			}
		}
		marker := ".prefetched-" + eco.Name + "-" + lockfileDigest(digested)
		if _, err := os.Lstat(filepath.Join(keyDir, marker)); errors.Is(err, os.ErrNotExist) {
			if err := eco.Prefetch(ctx, prefetchRequest{BaseSHA: baseSHA, Dir: dir, Files: files, Manifests: manifests}); err != nil {
				return deps, fmt.Errorf("prefetch %s modules for base %s: %w", eco.Name, shortSHA(baseSHA), err)
			}
			if err := safefs.WriteFile(keyDir, marker, nil, 0o600); err != nil {
				return deps, fmt.Errorf("record %s prefetch: %w", eco.Name, err)
			}
		} else if err != nil {
			return deps, fmt.Errorf("check %s prefetch: %w", eco.Name, err)
		}
		deps.Caches = append(deps.Caches, DependencyCache{Ecosystem: eco.Name, EnvKey: eco.envKey(), Dir: dir})
	}
	return deps, nil
}

func baseHasAny(ctx context.Context, worktree, baseSHA string, names []string) bool {
	for _, name := range names {
		if _, err := gitcredential.Output(ctx, worktree, "cat-file", "-e", baseSHA+":"+name); err == nil {
			return true
		}
	}
	return false
}

func lockfileDigest(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(files[name]))
		h.Write(files[name])
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// runPrefetchCommand runs one fetch-only command on the host. Replaced in
// tests to record argv and environment.
var runPrefetchCommand = func(ctx context.Context, dir string, env, argv []string) ([]byte, []byte, error) {
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, binary, argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	process.Configure(cmd)
	cmd.Cancel = func() error { process.Stop(cmd); return nil }
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	process.Stop(cmd)
	return stdout.Bytes(), stderr.Bytes(), err
}

// goModDownloadArgv only fetches, verifies and extracts modules; it runs no
// repository or package code (`go help mod download`).
var goModDownloadArgv = []string{"go", "mod", "download", "-json"}

// goModDownload runs go mod download for the base's go.mod and go.sum in a
// temporary copy, because it writes go.sum into its working directory. It
// seeds from the host module cache before the host's GOPROXY. GOTOOLCHAIN is
// local, so no downloaded toolchain runs. When go.mod needs a newer Go than
// the host's, the matching golang.org/toolchain module is fetched too, so the
// sandboxed go command can switch to it offline.
func goModDownload(ctx context.Context, req prefetchRequest) error {
	gomod, ok := req.Files["go.mod"]
	if !ok {
		return errors.New("approved base has no go.mod")
	}
	tmp, err := os.MkdirTemp("", "swarm-prefetch-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	module, outside := filepath.Join(tmp, "module"), filepath.Join(tmp, "outside")
	for _, dir := range []string{module, outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
	}
	env := goPrefetchEnv(os.Getenv, tmp, req.Dir, hostGoModCache())
	redact := func(s string) string {
		for _, p := range []string{req.Dir, tmp, hostGoModCache()} {
			if p != "" {
				s = strings.ReplaceAll(s, p, "<"+filepath.Base(p)+">")
			}
		}
		return s
	}
	stdout, stderr, err := runPrefetchCommand(ctx, outside, env, []string{"go", "env", "GOVERSION"})
	if err != nil {
		return fmt.Errorf("find host go: %s", redact(prefetchFailure(stdout, stderr, err)))
	}
	local := ""
	if fields := strings.Fields(string(stdout)); len(fields) > 0 && strings.HasPrefix(fields[0], "go1") {
		local = strings.TrimPrefix(fields[0], "go")
	}
	// An unrecognized host version (such as a devel build) skips the
	// toolchain step; go mod download then reports any mismatch itself.
	if need := requiredGoToolchain(gomod); local != "" && need != "" && compareGoVersions(need, local) > 0 {
		toolchain := "golang.org/toolchain@v0.0.1-go" + need + "." + runtime.GOOS + "-" + runtime.GOARCH
		stdout, stderr, err = runPrefetchCommand(ctx, outside, env, append(append([]string(nil), goModDownloadArgv...), toolchain))
		if err != nil {
			return errors.New(redact(prefetchFailure(stdout, stderr, err)))
		}
		gomod = lowerGoDirective(gomod, local)
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), gomod, 0o600); err != nil {
		return err
	}
	if sum, ok := req.Files["go.sum"]; ok {
		if err := os.WriteFile(filepath.Join(module, "go.sum"), sum, 0o600); err != nil {
			return err
		}
	}
	stdout, stderr, err = runPrefetchCommand(ctx, module, env, goModDownloadArgv)
	if err != nil {
		return errors.New(redact(prefetchFailure(stdout, stderr, err)))
	}
	return nil
}

// goPrefetchEnv builds the prefetch environment from scratch. It copies only
// PATH and the host's module source settings, so no token, credential helper,
// SSH agent or GOFLAGS reaches the go command.
func goPrefetchEnv(getenv func(string) string, tmp, cache, hostCache string) []string {
	upstream := strings.TrimSpace(getenv("GOPROXY"))
	if upstream == "" {
		upstream = "https://proxy.golang.org,direct"
	}
	proxy := upstream
	if hostCache != "" {
		if info, err := os.Stat(filepath.Join(hostCache, "cache", "download")); err == nil && info.IsDir() {
			proxy = "file://" + filepath.ToSlash(filepath.Join(hostCache, "cache", "download")) + "," + upstream
		}
	}
	env := []string{
		"PATH=" + getenv("PATH"), "TMPDIR=" + tmp, "HOME=" + tmp,
		"GOPATH=" + filepath.Join(tmp, "gopath"), "GOCACHE=" + filepath.Join(tmp, "gocache"),
		"GOMODCACHE=" + cache, "GOPROXY=" + proxy,
		"GOTOOLCHAIN=local", "GOENV=off", "GOFLAGS=", "GOWORK=off",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
	}
	for _, key := range []string{"GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOSUMDB"} {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// hostGoModCache is the host's module cache, the prefetch's first source.
func hostGoModCache() string {
	for _, b := range HostToolchains() {
		if b.EnvKey == "GOMODCACHE" {
			return b.EnvVal
		}
	}
	return ""
}

// prefetchFailure names the modules go reported. go prints a missing module
// as `go: path@version: ...` on stderr; -json reports others in Error fields.
func prefetchFailure(stdout, stderr []byte, runErr error) string {
	var details []string
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if mod, _, ok := strings.Cut(strings.TrimPrefix(line, "go: "), ": "); ok && strings.HasPrefix(line, "go: ") && strings.Contains(mod, "@") {
			details = append(details, strings.TrimPrefix(line, "go: "))
		}
	}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	for {
		var m struct{ Path, Version, Error string }
		if err := dec.Decode(&m); err != nil {
			break
		}
		if m.Error != "" {
			msg := m.Error
			if m.Path != "" && !strings.HasPrefix(msg, m.Path) {
				msg = m.Path + "@" + m.Version + ": " + msg
			}
			details = append(details, msg)
		}
	}
	if len(details) == 0 {
		if s := strings.TrimSpace(string(stderr)); s != "" {
			details = append(details, s)
		} else {
			details = append(details, runErr.Error())
		}
	}
	details = dedupe(details)
	if len(details) > 10 {
		details = append(details[:10], fmt.Sprintf("and %d more", len(details)-10))
	}
	return strings.Join(details, "; ")
}

func dedupe(lines []string) []string {
	seen := map[string]bool{}
	out := lines[:0]
	for _, line := range lines {
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	return out
}

// requiredGoToolchain returns the Go version go.mod selects (its toolchain
// line, else its go line), without the "go" prefix, or "".
func requiredGoToolchain(gomod []byte) string {
	goLine, toolchain := "", ""
	scanner := bufio.NewScanner(bytes.NewReader(gomod))
	for scanner.Scan() {
		line, _, _ := strings.Cut(scanner.Text(), "//")
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "go":
			goLine = fields[1]
		case "toolchain":
			toolchain = strings.TrimPrefix(fields[1], "go")
		}
	}
	if toolchain != "" && toolchain != "default" {
		return toolchain
	}
	// Since Go 1.21 a language version such as 1.21 selects its .0 release.
	if parts := strings.Split(goLine, "."); len(parts) == 2 {
		if minor, err := strconv.Atoi(parts[1]); err == nil && minor >= 21 {
			return goLine + ".0"
		}
	}
	return goLine
}

// lowerGoDirective rewrites the temporary go.mod's go line to the host's
// version and drops its toolchain line, so the local go command reads it
// under GOTOOLCHAIN=local. The module graph it downloads is unchanged.
func lowerGoDirective(gomod []byte, local string) []byte {
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(gomod))
	for scanner.Scan() {
		line := scanner.Text()
		code, _, _ := strings.Cut(line, "//")
		fields := strings.Fields(code)
		if len(fields) == 2 && fields[0] == "toolchain" {
			continue
		}
		if len(fields) == 2 && fields[0] == "go" {
			line = "go " + local
		}
		out.WriteString(line + "\n")
	}
	return out.Bytes()
}

// compareGoVersions orders Go versions such as 1.26, 1.26rc1 and 1.26.10
// the way the go command does: language < beta < rc < release.
func compareGoVersions(a, b string) int {
	pa, pb := goVersionKey(a), goVersionKey(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func goVersionKey(v string) [4]int {
	v = strings.TrimPrefix(v, "go")
	kind, n := 0, 0
	for i, pre := range []string{"beta", "rc"} {
		if before, num, ok := strings.Cut(v, pre); ok {
			v, kind = before, i+1
			n, _ = strconv.Atoi(num)
		}
	}
	parts := strings.Split(v, ".")
	major, _ := strconv.Atoi(parts[0])
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	if len(parts) > 2 {
		kind = 3
		n, _ = strconv.Atoi(parts[2])
	}
	return [4]int{major, minor, kind, n}
}
