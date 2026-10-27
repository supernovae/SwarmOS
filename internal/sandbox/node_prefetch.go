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
)

// nodeLockfiles enable the node adapter; each one present gets its own
// fetch-only command.
var nodeLockfiles = []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"}

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
	case "package-lock.json":
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

// nodePrefetch runs one fetch-only command per lockfile in the approved
// base, each in a temporary copy holding only that lockfile, the workspace
// file and the base's package.json files. Repository .npmrc, .yarnrc,
// .yarn/ and .pnpmfile.cjs never reach the copy, and the packageManager
// field is dropped, so no repository code or configured registry runs.
// Git-hosted dependencies are refused first, because fetching them can run
// their prepare script.
func nodePrefetch(ctx context.Context, req prefetchRequest) error {
	var present []string
	for _, name := range nodeLockfiles {
		if _, ok := req.Files[name]; ok {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return errors.New("approved base has no Node lockfile")
	}
	if refused := nodeGitDependencies(req.Files); len(refused) > 0 {
		return fmt.Errorf("git-hosted dependencies cannot be fetched without running package code: %s", strings.Join(refused, "; "))
	}
	for _, k := range nodeCacheEnv {
		if err := os.MkdirAll(filepath.Join(req.Dir, k.Subdir), 0o700); err != nil {
			return err
		}
	}
	for _, lockfile := range present {
		if err := nodeFetch(ctx, req, lockfile); err != nil {
			return err
		}
	}
	return nil
}

func nodeFetch(ctx context.Context, req prefetchRequest, lockfile string) error {
	tmp, err := os.MkdirTemp("", "swarm-prefetch-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	project := filepath.Join(tmp, "project")
	files := map[string][]byte{lockfile: req.Files[lockfile]}
	for name, data := range req.Manifests {
		files[name] = data
	}
	for _, name := range []string{"package.json", "pnpm-workspace.yaml"} {
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
	stdout, stderr, err := runPrefetchCommand(ctx, project, env, nodeFetchArgv(lockfile, req.Files[lockfile]))
	if err != nil {
		detail := nodePrefetchFailure(stdout, stderr, err)
		for _, p := range []string{req.Dir, tmp} {
			detail = strings.ReplaceAll(detail, p, "<"+filepath.Base(p)+">")
		}
		return fmt.Errorf("%s: %s", lockfile, detail)
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
	if _, ok := fields["packageManager"]; !ok {
		return data, nil
	}
	delete(fields, "packageManager")
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
