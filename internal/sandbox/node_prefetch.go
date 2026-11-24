package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/storageuse"
)

// nodeLockfiles are the locks recognized by the fetch-only Node adapter.
var nodeLockfiles = []string{"package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock"}

// nodeCacheEnv gives each package manager its own subdirectory of the
// per-repository cache. npm_config_cache identifies the cache.
var nodeCacheEnv = []cacheEnvKey{
	{Key: "npm_config_cache", Subdir: "npm"},
	{Key: "npm_config_store_dir", Subdir: "pnpm-store"},
	{Key: "YARN_CACHE_FOLDER", Subdir: "yarn"},
}

// maxNodeManifests bounds how many package.json files a prefetch copies.
const maxNodeManifests = 1000

// nodeFetchArgv returns the fetch-only command for one lockfile. None runs
// package lifecycle or build scripts: npm and Yarn v1 take --ignore-scripts,
// pnpm fetch only fills the store, and Yarn Berry's skip-build mode never
// runs postinstall steps. nodePrefetchEnv disables scripts again.
func nodeFetchArgv(lockfile string, data []byte) []string {
	switch lockfile {
	case "package-lock.json", "npm-shrinkwrap.json":
		return []string{"npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund"}
	case "pnpm-lock.yaml":
		return []string{"pnpm", "fetch", "--frozen-lockfile"}
	case "yarn.lock":
		if bytes.Contains(data, []byte("__metadata:")) {
			return []string{"yarn", "install", "--immutable", "--mode=skip-build"}
		}
		return []string{"yarn", "install", "--frozen-lockfile", "--ignore-scripts", "--non-interactive"}
	}
	return nil
}

// readinessNodeFlow selects the same sanitized lockfile flow for preparation
// and consumption. A repository with competing locks needs an explicit manager.
func readinessNodeFlow(files map[string][]byte) (lock, manager, version string, err error) {
	var manifest struct {
		Manager string `json:"packageManager"`
	}
	if json.Unmarshal(files["package.json"], &manifest) != nil {
		return "", "", "", errors.New("supply a valid package.json")
	}
	if manifest.Manager != "" {
		manager, version, _ = strings.Cut(manifest.Manager, "@")
		version, _, _ = strings.Cut(version, "+")
		if !packageVer.MatchString(version) {
			return "", "", "", errors.New("declare an exact supported packageManager version in package.json")
		}
	}
	for _, name := range nodeLockfiles {
		data, ok := files[name]
		if !ok {
			continue
		}
		argv := nodeFetchArgv(name, data)
		if manager != "" && manager != argv[0] {
			continue
		}
		if lock != "" && name != "npm-shrinkwrap.json" {
			return "", "", "", errors.New("competing locks: declare a matching packageManager and keep one lockfile for that manager")
		}
		lock = name
	}
	if lock == "" {
		return "", "", "", errors.New("commit a supported lockfile matching packageManager in package.json")
	}
	manager = nodeFetchArgv(lock, files[lock])[0]
	if manager == "yarn" && version != "" && strings.HasPrefix(version, "1.") == bytes.Contains(files[lock], []byte("__metadata:")) {
		return "", "", "", errors.New("use a Yarn lockfile matching the declared Yarn major version")
	}
	return lock, manager, version, nil
}

func nodeManagerMatches(lock string, data []byte, observed, declared string) bool {
	if !packageVer.MatchString(observed) || (declared != "" && observed != declared) {
		return false
	}
	return lock != "yarn.lock" || strings.HasPrefix(observed, "1.") != bytes.Contains(data, []byte("__metadata:"))
}

// nodePrefetch runs only the selected fetch-only command in a sanitized copy.
func nodePrefetch(ctx context.Context, req prefetchRequest) error {
	lock, _, _, err := readinessNodeFlow(req.Files)
	if err != nil {
		return err
	}
	if refused := nodeGitDependencies(req.Files); len(refused) > 0 {
		return fmt.Errorf("git-hosted dependencies cannot be fetched without running package code: %s", strings.Join(refused, "; "))
	}
	for name, data := range req.Manifests {
		if readinessNodeProtocol(data) {
			return fmt.Errorf("%s: replace Git and executable package protocols before preparation", name)
		}
	}
	for _, name := range []string{lock, "package.json", "pnpm-workspace.yaml"} {
		if readinessNodeProtocol(req.Files[name]) {
			return fmt.Errorf("%s: replace Git and executable package protocols before preparation", name)
		}
	}
	for _, k := range nodeCacheEnv {
		if err := os.MkdirAll(filepath.Join(req.Dir, k.Subdir), 0o700); err != nil {
			return err
		}
	}
	return nodeFetch(ctx, req, lock)
}

func nodeFetch(ctx context.Context, req prefetchRequest, lockfile string) (result error) {
	ctx, owner, scopeErr := storageuse.Ensure(ctx, "")
	if scopeErr != nil {
		return scopeErr
	}
	if owner != nil {
		defer owner.Complete(&result)
	}

	tmp, err := prefetchTemp(ctx, "", "swarm-prefetch-*")
	if err != nil {
		return err
	}
	defer storageuse.FromContext(ctx).CleanupTemp(tmp, &result)
	project := filepath.Join(tmp, "project")
	files := map[string][]byte{lockfile: req.Files[lockfile]}
	for name, data := range req.Manifests {
		files[name] = data
	}
	for _, name := range []string{"package.json", "pnpm-workspace.yaml"} {
		if name == "pnpm-workspace.yaml" && lockfile != "pnpm-lock.yaml" {
			continue
		}
		if data, ok := req.Files[name]; ok {
			files[name] = data
		}
	}
	if _, ok := files["package.json"]; !ok {
		return errors.New("approved base has no package.json")
	}
	for name, data := range files {
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("unsafe manifest path %q", name)
		}
		if path.Base(name) == "package.json" {
			if data, err = withoutPackageManager(data); err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
		}
		target := filepath.Join(project, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return err
		}
	}
	env, err := nodePrefetchEnv(os.Getenv, tmp, req.Dir)
	if err != nil {
		return err
	}
	argv := nodeFetchArgv(lockfile, req.Files[lockfile])
	_, manager, version, err := readinessNodeFlow(req.Files)
	if err != nil {
		return err
	}
	if manager == "pnpm" {
		env = append(env, "npm_config_manage_package_manager_versions=false")
	}
	if version != "" || lockfile == "yarn.lock" {
		out, _, err := runPrefetchCommand(ctx, project, env, []string{manager, "--version"})
		if err != nil || !nodeManagerMatches(lockfile, req.Files[lockfile], strings.TrimSpace(string(out)), version) {
			return fmt.Errorf("package.json: install trusted %s %s matching the selected lockfile for dependency preparation (observed %q)", manager, version, strings.TrimSpace(string(out)))
		}
	}
	stdout, stderr, err := runPrefetchCommand(ctx, project, env, argv)
	if err != nil {
		detail := nodePrefetchFailure(stdout, stderr, err)
		for _, p := range []string{req.Dir, tmp} {
			detail = strings.ReplaceAll(detail, p, "<"+filepath.Base(p)+">")
		}
		return fmt.Errorf("%s (%s): install the selected manager and make the locked packages available to trusted preparation: %s", lockfile, manager, detail)
	}
	return nil
}

// withoutPackageManager drops package.json's packageManager field, which
// would make corepack download and run a package manager.
func withoutPackageManager(data []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	_, manager := fields["packageManager"]
	if !manager && fields["devEngines"] == nil {
		return data, nil
	}
	delete(fields, "packageManager")
	if fields["devEngines"] != nil {
		var engines map[string]json.RawMessage
		if err := json.Unmarshal(fields["devEngines"], &engines); err != nil {
			return nil, err
		}
		delete(engines, "packageManager")
		fields["devEngines"], _ = json.Marshal(engines)
	}
	return json.Marshal(fields)
}

// nodePrefetchEnv builds the prefetch environment from scratch. It copies
// only PATH and the host's registry settings, so no token, NODE_OPTIONS,
// SSH agent or user npmrc reaches the package manager, and every manager's
// script and plugin switches are off.
func nodePrefetchEnv(getenv func(string) string, tmp, cache string) ([]string, error) {
	userConfig, globalConfig := filepath.Join(tmp, "npmrc"), filepath.Join(tmp, "global-npmrc")
	for _, f := range []string{userConfig, globalConfig} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			return nil, err
		}
	}
	env := []string{
		"PATH=" + getenv("PATH"), "HOME=" + tmp, "TMPDIR=" + tmp,
		"XDG_CONFIG_HOME=" + filepath.Join(tmp, "config"), "XDG_CACHE_HOME=" + filepath.Join(tmp, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(tmp, "data"),
		"npm_config_userconfig=" + userConfig, "npm_config_globalconfig=" + globalConfig,
		"npm_config_logs_dir=" + filepath.Join(tmp, "logs"),
		"npm_config_ignore_scripts=true", "npm_config_ignore_pnpmfile=true",
		"npm_config_update_notifier=false", "npm_config_audit=false", "npm_config_fund=false",
		"YARN_ENABLE_SCRIPTS=0", "YARN_IGNORE_PATH=1", "YARN_ENABLE_GLOBAL_CACHE=0", "YARN_ENABLE_TELEMETRY=0",
		"COREPACK_ENABLE_NETWORK=0", "COREPACK_ENABLE_AUTO_PIN=0", "COREPACK_ENABLE_DOWNLOAD_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
	}
	for _, k := range nodeCacheEnv {
		env = append(env, k.Key+"="+filepath.Join(cache, k.Subdir))
	}
	for _, key := range []string{"npm_config_registry", "YARN_NPM_REGISTRY_SERVER"} {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env, nil
}

// nodeGitMarkers identify git-hosted dependencies in npm, pnpm and Yarn
// lockfiles.
var nodeGitMarkers = []string{"git+", "git://", "github:", "codeload.github.com", "#commit=", "type: git"}

// lockfileFields are the per-entry fields a git marker can sit in; the
// package name is the enclosing entry instead.
var lockfileFields = map[string]bool{
	"resolved": true, "version": true, "resolution": true, "specifier": true, "from": true,
	"integrity": true, "tarball": true, "repo": true, "commit": true, "type": true,
}

// nodeGitDependencies names each lockfile entry that resolves from git. It
// reads lockfiles line by line: a match names its own key, or else the
// innermost enclosing entry.
func nodeGitDependencies(files map[string][]byte) []string {
	var refused []string
	for _, lockfile := range nodeLockfiles {
		type header struct {
			indent int
			name   string
		}
		var stack []header
		seen := map[string]bool{}
		for _, line := range strings.Split(string(files[lockfile]), "\n") {
			text := strings.TrimSpace(line)
			if text == "" || strings.HasPrefix(text, "#") {
				continue
			}
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
				stack = stack[:len(stack)-1]
			}
			key, _, _ := strings.Cut(text, ": ")
			key = lockfileName(key)
			isHeader := strings.HasSuffix(text, "{") || strings.HasSuffix(text, ":")
			if isHeader {
				stack = append(stack, header{indent, lockfileName(text)})
			}
			// Integrity hashes are base64 and can contain "git+".
			if !containsAny(integrityHash.ReplaceAllString(text, ""), nodeGitMarkers) {
				continue
			}
			name := key
			if isHeader {
				name = lockfileName(text)
			} else if lockfileFields[key] || key == text {
				name = ""
				if len(stack) > 0 {
					name = stack[len(stack)-1].name
				}
			}
			if name == "" {
				name = text
			}
			if entry := lockfile + ": " + name; !seen[entry] {
				seen[entry] = true
				refused = append(refused, entry)
			}
		}
	}
	return refused
}

// lockfileName trims a lockfile key to a package name: quotes, the
// trailing ":" or "{", and an npm node_modules/ path prefix.
func lockfileName(s string) string {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "{"))
	s = strings.TrimSuffix(strings.TrimSpace(s), ":")
	s = strings.Trim(s, `"'`)
	if i := strings.LastIndex(s, "node_modules/"); i >= 0 {
		s = s[i+len("node_modules/"):]
	}
	return s
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

var (
	npmNotInRegistry = regexp.MustCompile(`'([^'\s]+@[^'\s]+)' is not in (?:this|the npm) registry`)
	npmNoVersion     = regexp.MustCompile(`No matching version found for (\S+@\S+?)\.?(?:\s|$)`)
	npmTarball       = regexp.MustCompile(`GET \S+?/(?:(@[^/\s]+)/)?([^/\s]+)/-/[^/\s]+?-(\d[^/\s]*?)\.tgz`)
	integrityHash    = regexp.MustCompile(`sha\d+-[A-Za-z0-9+/=]+`)
	yarnMissing      = regexp.MustCompile(`Couldn't find package "([^"]+)"`)
)

// nodePrefetchFailure names the dependencies a package manager reported
// missing, or else returns its trimmed error output.
func nodePrefetchFailure(stdout, stderr []byte, runErr error) string {
	var details []string
	output := string(stderr) + "\n" + string(stdout)
	for _, re := range []*regexp.Regexp{npmNotInRegistry, npmNoVersion, yarnMissing} {
		for _, m := range re.FindAllStringSubmatch(output, -1) {
			if !strings.Contains(m[1], "://") {
				details = append(details, m[1])
			}
		}
	}
	for _, m := range npmTarball.FindAllStringSubmatch(output, -1) {
		name := m[2]
		if m[1] != "" {
			name = m[1] + "/" + m[2]
		}
		details = append(details, name+"@"+m[3])
	}
	if len(details) == 0 {
		if s := strings.TrimSpace(string(stderr)); s != "" {
			details = append(details, lastLines(s, 10))
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

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " ")
}

// baseManifests reads every package.json in baseSHA outside node_modules,
// so workspace installs see their members' manifests.
func baseManifests(ctx context.Context, worktree, baseSHA string) (map[string][]byte, error) {
	out, err := gitcredential.Output(ctx, worktree, "ls-tree", "-r", "-z", "--full-tree", baseSHA)
	if err != nil {
		return nil, err
	}
	manifests := map[string][]byte{}
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			continue
		}
		if path.Base(name) != "package.json" || strings.Contains("/"+name, "/node_modules/") {
			continue
		}
		if len(manifests) == maxNodeManifests {
			return nil, fmt.Errorf("more than %d package.json files", maxNodeManifests)
		}
		data, err := gitcredential.Output(ctx, worktree, "cat-file", "blob", fields[2])
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		manifests[name] = data
	}
	return manifests, nil
}
