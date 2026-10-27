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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	TreeFiles  string   // basename also read at every path of the base tree
	EnvKey     string
	Prefetch   prefetchFunc
	NoPrefetch string // why there is no fetch-only prefetch
}

// prefetchRequest is one host prefetch. Dir is the only directory it may
// write; Files holds the lockfiles and tree files present in the approved
// base, keyed by their path relative to the repository root.
type prefetchRequest struct {
	BaseSHA string
	Dir     string
	Files   map[string][]byte
}

type prefetchFunc func(context.Context, prefetchRequest) error

// dependencyEcosystems is the only source of dependency cache environment
// keys and classifies every ecosystem resolveHostToolchains mounts. Adding an
// entry is covered by TestDependencyTableSafety and
// TestEveryMountedEcosystemIsClassified; see "Dependency caches" in
// docs/project.md.
var dependencyEcosystems = []dependencyEcosystem{
	{Name: "go", Detect: []string{"go.mod"}, Lockfiles: []string{"go.mod", "go.sum"}, EnvKey: "GOMODCACHE", Prefetch: goModDownload},
	{Name: "rust", Detect: []string{"Cargo.lock"}, Lockfiles: []string{"Cargo.lock"}, TreeFiles: "Cargo.toml", EnvKey: "CARGO_HOME", Prefetch: cargoFetch},
	{Name: "python", NoPrefetch: "adapter is a follow-up"},
	{Name: "node", NoPrefetch: "adapter is a follow-up"},
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

// DependencyCacheEnv returns KEY=dir entries for caches.
func DependencyCacheEnv(caches []DependencyCache) []string {
	env := make([]string, 0, len(caches))
	for _, c := range caches {
		env = append(env, c.EnvKey+"="+c.Dir)
	}
	return env
}

// checkDependencyCache accepts only a table key for its ecosystem, at an
// absolute directory outside the workspace.
func checkDependencyCache(c DependencyCache, work string) error {
	eco, ok := dependencyEcosystemNamed(c.Ecosystem)
	if !ok || eco.Prefetch == nil || eco.EnvKey != c.EnvKey {
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
	c := DependencyCache{Ecosystem: name, EnvKey: eco.EnvKey, Dir: filepath.Clean(dir)}
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
		if err := readBaseTreeFiles(ctx, worktree, baseSHA, eco.TreeFiles, files); err != nil {
			return deps, err
		}
		// v3 re-prepares caches filled before downloaded Go toolchains were finished.
		marker := ".prefetched-v3-" + eco.Name + "-" + lockfileDigest(files)
		if _, err := os.Lstat(filepath.Join(keyDir, marker)); errors.Is(err, os.ErrNotExist) {
			if err := eco.Prefetch(ctx, prefetchRequest{BaseSHA: baseSHA, Dir: dir, Files: files}); err != nil {
				return deps, fmt.Errorf("prefetch %s modules for base %s: %w", eco.Name, shortSHA(baseSHA), err)
			}
			if err := safefs.WriteFile(keyDir, marker, nil, 0o600); err != nil {
				return deps, fmt.Errorf("record %s prefetch: %w", eco.Name, err)
			}
		} else if err != nil {
			return deps, fmt.Errorf("check %s prefetch: %w", eco.Name, err)
		}
		deps.Caches = append(deps.Caches, DependencyCache{Ecosystem: eco.Name, EnvKey: eco.EnvKey, Dir: dir})
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

// readBaseTreeFiles adds every regular file named basename in the base tree
// to files. Symlinks and submodules are skipped.
func readBaseTreeFiles(ctx context.Context, worktree, baseSHA, basename string, files map[string][]byte) error {
	if basename == "" {
		return nil
	}
	list, err := gitcredential.Output(ctx, worktree, "ls-tree", "-r", "-z", baseSHA)
	if err != nil {
		return fmt.Errorf("list approved base %s: %w", shortSHA(baseSHA), err)
	}
	for _, entry := range strings.Split(string(list), "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			continue
		}
		if filepath.Base(filepath.FromSlash(path)) != basename || !filepath.IsLocal(filepath.FromSlash(path)) {
			continue
		}
		data, err := gitcredential.Output(ctx, worktree, "cat-file", "blob", fields[2])
		if err != nil {
			return fmt.Errorf("read %s from approved base: %w", path, err)
		}
		files[path] = data
	}
	return nil
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
		// The sandbox mounts this cache read-only, but the go command finishes
		// a downloaded toolchain on its first switch. Do that here, without
		// running the toolchain, so the prefetch stays fetch-only.
		if err := finishGoToolchain(filepath.Join(req.Dir, "golang.org", strings.TrimPrefix(toolchain, "golang.org/"))); err != nil {
			return fmt.Errorf("prepare Go toolchain %s: %s", need, redact(err.Error()))
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
// finishGoToolchain repeats what cmd/go's toolchain switch
// (cmd/go/internal/toolchain, Go 1.21+) does to a freshly extracted
// golang.org/toolchain module on first use: add execute bits under pkg/tool,
// lib, and bin, then copy each _go.mod to go.mod, src/go.mod last. Both steps
// are skipped when already done, exactly as the go command checks.
func finishGoToolchain(dir string) error {
	if info, err := os.Stat(filepath.Join(dir, "bin", "go")); err == nil && info.Mode()&0o111 == 0 {
		for _, step := range []struct{ dir, pattern string }{{"pkg/tool", ""}, {"lib", "go_?*_?*_exec"}, {"bin/gofmt", ""}, {"bin", ""}} {
			if err := allowToolchainExec(filepath.Join(dir, filepath.FromSlash(step.dir)), step.pattern); err != nil {
				return err
			}
		}
	}
	srcUGoMod, srcGoMod := filepath.Join(dir, "src", "_go.mod"), filepath.Join(dir, "src", "go.mod")
	if fileSize(srcGoMod) == fileSize(srcUGoMod) {
		return nil
	}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == srcUGoMod || d.Name() != "_go.mod" {
			return err
		}
		return copyToolchainGoMod(path, filepath.Join(filepath.Dir(path), "go.mod"))
	}); err != nil {
		return err
	}
	return copyToolchainGoMod(srcUGoMod, srcGoMod)
}

func allowToolchainExec(dir, pattern string) error {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if pattern != "" {
			if ok, _ := filepath.Match(pattern, d.Name()); !ok {
				return nil
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode()&0o777|0o111)
	})
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return info.Size()
}

// copyToolchainGoMod writes new from old inside a read-only module cache
// directory, restoring the directory's mode afterwards.
func copyToolchainGoMod(old, new string) error {
	oldInfo, err := os.Stat(old)
	if err != nil {
		return err
	}
	if fileSize(new) == oldInfo.Size() {
		return nil
	}
	data, err := os.ReadFile(old)
	if err != nil {
		return err
	}
	dir := filepath.Dir(old)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if err := os.Chmod(dir, dirInfo.Mode()|0o200); err != nil {
		return err
	}
	defer os.Chmod(dir, dirInfo.Mode())
	if err := os.WriteFile(new, data, 0o644); err != nil {
		return err
	}
	return os.Chmod(new, oldInfo.Mode())
}

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

// cargoFetchArgv only downloads the locked dependencies' sources into
// CARGO_HOME; it compiles nothing and runs no build script
// (`cargo help fetch`).
var cargoFetchArgv = []string{"cargo", "fetch", "--locked"}

// cargoHomeAllowed is everything cargo fetch may leave in CARGO_HOME. Any
// other entry, such as config.toml or credentials.toml, fails the prefetch so
// it is never mounted into a sandbox.
var cargoHomeAllowed = map[string]bool{
	"registry": true, "git": true,
	".package-cache": true, ".package-cache-mutate": true, ".global-cache": true,
}

// cargoFetch runs cargo fetch for the base's Cargo.lock and manifests in a
// temporary copy beside the cache. Only manifests and empty src/lib.rs stubs
// are written: no build.rs, .cargo/config.toml or rust-toolchain file, so no
// repository code or configuration reaches cargo. The copy lives under the
// private cache key directory rather than $TMPDIR, because cargo also reads
// .cargo/config.toml from every ancestor directory.
func cargoFetch(ctx context.Context, req prefetchRequest) error {
	if _, ok := req.Files["Cargo.lock"]; !ok {
		return errors.New("approved base has no Cargo.lock")
	}
	if _, ok := req.Files["Cargo.toml"]; !ok {
		return errors.New("approved base has no root Cargo.toml")
	}
	if err := checkCargoHome(req.Dir); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(req.Dir), ".prefetch-rust-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	work := filepath.Join(tmp, "work")
	for rel, data := range req.Files {
		path := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
	}
	for rel := range req.Files {
		if filepath.Base(filepath.FromSlash(rel)) != "Cargo.toml" {
			continue
		}
		stub := filepath.Join(work, filepath.Dir(filepath.FromSlash(rel)), "src", "lib.rs")
		if _, err := os.Lstat(stub); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(stub), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(stub, nil, 0o600); err != nil {
			return err
		}
	}
	env := cargoPrefetchEnv(os.Getenv, tmp, req.Dir, hostRustupHome())
	_, stderr, err := runPrefetchCommand(ctx, work, env, cargoFetchArgv)
	if err != nil {
		failure := cargoFetchFailure(stderr, err)
		for _, p := range []string{req.Dir, tmp} {
			failure = strings.ReplaceAll(failure, p, "<"+filepath.Base(p)+">")
		}
		return errors.New(failure)
	}
	return checkCargoHome(req.Dir)
}

// checkCargoHome rejects anything in the cache cargo fetch would not write.
func checkCargoHome(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		wantDir := e.Name() == "registry" || e.Name() == "git"
		if !cargoHomeAllowed[e.Name()] || e.Type()&os.ModeSymlink != 0 || e.IsDir() != wantDir {
			return fmt.Errorf("cargo cache holds unexpected %q", e.Name())
		}
	}
	return nil
}

// cargoPrefetchEnv builds the prefetch environment from scratch. It copies
// only PATH and the host's rustup selection, so no token, credential helper,
// SSH agent, registry credential, RUSTFLAGS or rustc wrapper reaches cargo,
// and rustup never installs a toolchain.
func cargoPrefetchEnv(getenv func(string) string, tmp, cache, rustupHome string) []string {
	env := []string{
		"PATH=" + getenv("PATH"), "TMPDIR=" + tmp, "HOME=" + tmp,
		"CARGO_HOME=" + cache, "CARGO_TARGET_DIR=" + filepath.Join(tmp, "target"),
		"CARGO_BUILD_RUSTC_WRAPPER=", "CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER=", "RUSTC_WRAPPER=", "RUSTC_WORKSPACE_WRAPPER=",
		"CARGO_NET_GIT_FETCH_WITH_CLI=false", "CARGO_TERM_COLOR=never", "RUSTUP_AUTO_INSTALL=0",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
	}
	if rustupHome != "" {
		env = append(env, "RUSTUP_HOME="+rustupHome)
	}
	if toolchain := strings.TrimSpace(getenv("RUSTUP_TOOLCHAIN")); toolchain != "" {
		env = append(env, "RUSTUP_TOOLCHAIN="+toolchain)
	}
	return env
}

// hostRustupHome is the host's rustup home, so a rustup cargo proxy finds the
// installed toolchains under the prefetch's temporary HOME.
func hostRustupHome() string {
	for _, b := range HostToolchains() {
		if b.EnvKey == "RUSTUP_HOME" {
			return b.EnvVal
		}
	}
	return ""
}

// cargoFetchFailure names the dependencies cargo reported, followed by its
// root cause (the last error line), else returns its stderr.
func cargoFetchFailure(stderr []byte, runErr error) string {
	var names []string
	cause := ""
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"failed to load source for dependency `", "failed to download `", "failed to get `"} {
			if _, rest, ok := strings.Cut(line, prefix); ok {
				if name, _, ok := strings.Cut(rest, "`"); ok {
					name, _, _ = strings.Cut(name, " ")
					names = append(names, name)
				}
			}
		}
		if line != "" && !strings.HasPrefix(line, "warning:") && !strings.HasPrefix(line, "Caused by:") && !strings.HasPrefix(line, "Updating ") && !strings.HasPrefix(line, "Downloading ") && !strings.HasPrefix(line, "Downloaded ") {
			cause = strings.TrimPrefix(line, "error: ")
		}
	}
	names = dedupe(names)
	switch {
	case len(names) > 0 && cause != "":
		return "dependency " + strings.Join(names, ", ") + ": " + cause
	case strings.TrimSpace(string(stderr)) != "":
		return strings.TrimSpace(string(stderr))
	default:
		return runErr.Error()
	}
}
