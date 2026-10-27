package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/safefs"
)

// buildCacheEcosystem names one language's compiled-output cache. Deferred
// ecosystems are listed so the table stays complete, but are not cached
// because their cache home also holds credentials.
type buildCacheEcosystem struct{ Name, EnvKey, Deferred string }

// buildCacheEcosystems is the only source of build cache environment keys.
// Adding an entry is covered by TestBuildCacheTableSafety; see "Build caches
// and who can write them" in docs/project.md.
var buildCacheEcosystems = []buildCacheEcosystem{
	{Name: "go", EnvKey: "GOCACHE"},
	{Name: "rust", EnvKey: "CARGO_TARGET_DIR"},
	{Name: "c-cpp", EnvKey: "CCACHE_DIR"},
	{Name: "dotnet", Deferred: "build cache home holds NuGet credentials (#188)"},
	{Name: "gradle", Deferred: "GRADLE_USER_HOME holds credentials (#188)"},
	{Name: "maven", Deferred: "~/.m2 holds settings.xml credentials (#188)"},
}

// BuildCacheOwner separates agent caches from validator caches. The two never
// share a directory, so agent output cannot reach validation.
type BuildCacheOwner string

const (
	AgentBuildCache     BuildCacheOwner = "agent-build"
	ValidatorBuildCache BuildCacheOwner = "validate-build"
)

// BuildCache is one ecosystem's cache directory for a run. Persistent caches
// live under <user-cache>/swarm/<owner>/<repo-key>/<ecosystem>; others are
// per-run scratch. Overlay mounts Dir read-only with a discarded write layer.
type BuildCache struct {
	Ecosystem, EnvKey, Dir string
	Persistent, Overlay    bool
}

// userCacheDir is replaced by tests so they never touch the real user cache.
var userCacheDir = os.UserCacheDir

// IsBuildCacheKey reports whether key is owned by the build cache table, so
// callers drop any host value for it.
func IsBuildCacheKey(key string) bool {
	for _, eco := range buildCacheEcosystems {
		if eco.EnvKey != "" && eco.EnvKey == key {
			return true
		}
	}
	return false
}

// BuildCacheEnv returns KEY=dir entries for caches.
func BuildCacheEnv(caches []BuildCache) []string {
	env := make([]string, 0, len(caches))
	for _, c := range caches {
		env = append(env, c.EnvKey+"="+c.Dir)
	}
	return env
}

// ParseBuildCache reads an internal-tools --build-cache value, ecosystem=dir.
// The environment key always comes from the table, never from the caller.
func ParseBuildCache(value, workspace string) (BuildCache, error) {
	name, dir, ok := strings.Cut(value, "=")
	if !ok {
		return BuildCache{}, fmt.Errorf("build cache %q: want ecosystem=dir", value)
	}
	for _, eco := range buildCacheEcosystems {
		if eco.Name != name || eco.Deferred != "" {
			continue
		}
		if !filepath.IsAbs(dir) || filepath.Clean(dir) == "/" {
			return BuildCache{}, fmt.Errorf("build cache %s: %q is not an absolute directory", name, dir)
		}
		dir = filepath.Clean(dir)
		if workspace != "" && (insideDir(workspace, dir) || insideDir(dir, workspace)) {
			return BuildCache{}, fmt.Errorf("build cache %s overlaps the workspace", name)
		}
		return BuildCache{Ecosystem: name, EnvKey: eco.EnvKey, Dir: dir}, nil
	}
	return BuildCache{}, fmt.Errorf("unknown build cache ecosystem %q", name)
}

func buildCacheFlag(c BuildCache) string { return c.Ecosystem + "=" + c.Dir }

// repositoryCacheKey hashes the canonical Git common directory, so linked
// issue and PR worktrees share a key while unrelated repositories stay apart.
func repositoryCacheKey(ctx context.Context, worktree string) (string, error) {
	cmd, _, err := gitcredential.Command(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve repository identity: %w", err)
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		return "", errors.New("repository identity is not absolute")
	}
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	sum := sha256.Sum256([]byte(filepath.Clean(common)))
	return hex.EncodeToString(sum[:16]), nil
}

// agentBuildCaches gives developer, tester and resolver runs their persistent
// agent cache. Read-only and discarded runs get scratch only, so they never
// gain a write channel to later runs.
func agentBuildCaches(ctx context.Context, worktree, scratch string, opts WorkerOptions) ([]BuildCache, error) {
	userCache := ""
	if !opts.ReadOnly && !opts.DiscardChanges {
		userCache, _ = userCacheDir()
	}
	return PrepareBuildCaches(ctx, AgentBuildCache, worktree, userCache, scratch)
}

// PrepareBuildCaches returns one cache per cached ecosystem for owner. Each is
// <userCache>/swarm/<owner>/<repo-key>/<ecosystem> when that can be prepared
// safely, and otherwise <scratch>/build-cache/<ecosystem>; it never falls back
// to the user's own cache directories. An empty userCache selects scratch.
// The error reports only a failure to create scratch.
func PrepareBuildCaches(ctx context.Context, owner BuildCacheOwner, worktree, userCache, scratch string) ([]BuildCache, error) {
	persistent := persistentBuildCaches(ctx, owner, worktree, userCache)
	var caches []BuildCache
	for _, eco := range buildCacheEcosystems {
		if eco.Deferred != "" {
			continue
		}
		if dir, ok := persistent[eco.Name]; ok {
			caches = append(caches, BuildCache{Ecosystem: eco.Name, EnvKey: eco.EnvKey, Dir: dir, Persistent: true})
			continue
		}
		dir, err := scratchBuildCache(scratch, eco.Name)
		if err != nil {
			return nil, err
		}
		caches = append(caches, BuildCache{Ecosystem: eco.Name, EnvKey: eco.EnvKey, Dir: dir})
	}
	return caches, nil
}

func scratchBuildCache(scratch, name string) (string, error) {
	if !filepath.IsAbs(scratch) {
		return "", fmt.Errorf("build cache scratch %q is not absolute", scratch)
	}
	dir := filepath.Join(scratch, "build-cache", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("prepare scratch build cache: %w", err)
	}
	return dir, nil
}

// persistentBuildCaches returns the ecosystems whose persistent directory is
// safe to use. Any failure leaves that ecosystem out, so it gets scratch.
func persistentBuildCaches(ctx context.Context, owner BuildCacheOwner, worktree, userCache string) map[string]string {
	dirs := map[string]string{}
	if userCache == "" || !filepath.IsAbs(userCache) || (owner != AgentBuildCache && owner != ValidatorBuildCache) {
		return dirs
	}
	base := filepath.Clean(userCache)
	key, err := repositoryCacheKey(ctx, worktree)
	if err != nil {
		return dirs
	}
	keyRel := filepath.Join("swarm", string(owner), key)
	if insideDir(worktree, filepath.Join(base, keyRel)) {
		return dirs
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return dirs
	}
	root, err := safefs.Open(base)
	if err != nil {
		return dirs
	}
	defer root.Close()
	for _, eco := range buildCacheEcosystems {
		if eco.Deferred != "" {
			continue
		}
		rel := filepath.Join(keyRel, eco.Name)
		if owner == ValidatorBuildCache && eco.Name == "go" {
			migrateLegacyGoCache(root, base, key, rel)
		}
		if privateCacheDir(root, rel) == nil {
			dirs[eco.Name] = filepath.Join(base, rel)
		}
	}
	return dirs
}

// migrateLegacyGoCache moves the validator's former swarm/go-build/<key> cache
// into place once. A failure leaves a fresh directory to be created instead.
func migrateLegacyGoCache(root *safefs.Root, base, key, rel string) {
	legacy := filepath.Join("swarm", "go-build", key)
	info, err := os.Lstat(filepath.Join(base, legacy))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if _, err := os.Lstat(filepath.Join(base, rel)); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if privateCacheDir(root, filepath.Dir(rel)) != nil {
		return
	}
	_ = root.Rename(legacy, rel)
}

// privateCacheDir creates rel and checks every component below the user
// cache. Sub rejects symlinked or non-directory components, and no component
// may be writable by other users, so the cache cannot be redirected.
func privateCacheDir(root *safefs.Root, rel string) error {
	prefix := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		dir, err := root.Sub(prefix, true, 0o700)
		if err != nil {
			return err
		}
		err = func() error {
			defer dir.Close()
			if runtime.GOOS == "windows" {
				return nil
			}
			f, err := dir.OpenDirectory(".")
			if err != nil {
				return err
			}
			info, err := f.Stat()
			f.Close()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o022 != 0 {
				return fmt.Errorf("build cache directory %q is writable by other users", prefix)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// insideDir reports whether path is root or below it, also comparing root's
// absolute and resolved locations.
func insideDir(root, path string) bool {
	roots := []string{root}
	if abs, err := filepath.Abs(root); err == nil {
		roots = append(roots, abs)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		roots = append(roots, resolved)
	}
	paths := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		paths = append(paths, resolved)
	}
	for _, r := range roots {
		for _, p := range paths {
			if rel, err := filepath.Rel(r, p); err == nil && (rel == "." || filepath.IsLocal(rel)) {
				return true
			}
		}
	}
	return false
}

// ValidationBackend names where a validation command runs, for
// ValidatorCacheMounts.
type ValidationBackend string

const (
	ValidationOnHost     ValidationBackend = "host"
	ValidationCodex      ValidationBackend = "codex"
	ValidationBubblewrap ValidationBackend = "bubblewrap"
)

// ValidatorCacheMounts is the single policy point for how a validation run
// sees the validator's caches ("Build caches and who can write them" in
// docs/project.md). Candidate code is untrusted and build caches are not
// verified on read, so only warm-up of the approved base writes a persistent
// cache. A candidate on Bubblewrap with overlay support reads it through a
// discarded write layer; every other candidate gets <scratch>/build-cache.
func ValidatorCacheMounts(caches []BuildCache, backend ValidationBackend, warmup, overlay bool, scratch string) ([]BuildCache, error) {
	mounts := make([]BuildCache, 0, len(caches))
	for _, c := range caches {
		c.Overlay = false
		switch {
		case !c.Persistent:
		case warmup:
		case backend == ValidationBubblewrap && overlay:
			c.Overlay = true
		default:
			dir, err := scratchBuildCache(scratch, c.Ecosystem)
			if err != nil {
				return nil, err
			}
			c.Dir, c.Persistent = dir, false
		}
		mounts = append(mounts, c)
	}
	return mounts, nil
}

// ValidatorCacheReusable reports whether a candidate on backend reads the
// persistent validator caches through ValidatorCacheMounts. Warm-up runs only
// then, since elsewhere the caches it writes would have no reader.
func ValidatorCacheReusable(backend ValidationBackend, overlay bool) bool {
	return backend == ValidationBubblewrap && overlay
}
