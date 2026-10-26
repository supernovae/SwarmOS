package validation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

// ErrGoCacheUnavailable reports that the SwarmOS-owned host build cache could
// not be prepared. Host checks never fall back to the user's own GOCACHE.
var ErrGoCacheUnavailable = errors.New("swarm Go build cache unavailable")

// userCacheDir is replaced by tests so they never touch the real user cache.
var userCacheDir = os.UserCacheDir

// hostGoCache returns <user-cache>/swarm/go-build/<key>, a private Go build
// cache shared by every worktree of one repository. The key hashes the
// canonical Git common directory, so linked issue and PR worktrees reuse
// compiled packages while unrelated repositories stay apart. It is used only
// for checks that already run on the host and is never mounted into sandboxes.
func hostGoCache(worktreePath string) (string, error) {
	out, err := gitOutput(worktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%w: resolve repository identity: %v", ErrGoCacheUnavailable, err)
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		return "", fmt.Errorf("%w: repository identity is not absolute", ErrGoCacheUnavailable)
	}
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	sum := sha256.Sum256([]byte(filepath.Clean(common)))
	key := hex.EncodeToString(sum[:16])

	base, err := userCacheDir()
	if err != nil || !filepath.IsAbs(base) {
		return "", fmt.Errorf("%w: no absolute user cache directory", ErrGoCacheUnavailable)
	}
	base = filepath.Clean(base)
	rel := filepath.Join("swarm", "go-build", key)
	dir := filepath.Join(base, rel)
	if insideDir(worktreePath, dir) {
		return "", fmt.Errorf("%w: user cache directory is inside the worktree", ErrGoCacheUnavailable)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("%w: %v", ErrGoCacheUnavailable, err)
	}
	root, err := safefs.Open(base)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGoCacheUnavailable, err)
	}
	defer root.Close()
	// Sub creates missing components at 0700 and rejects symlinked or
	// non-directory components, so the cache cannot be redirected.
	cache, err := root.Sub(rel, true, 0o700)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGoCacheUnavailable, err)
	}
	defer cache.Close()
	if runtime.GOOS != "windows" {
		f, err := cache.OpenDirectory(".")
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrGoCacheUnavailable, err)
		}
		info, err := f.Stat()
		f.Close()
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrGoCacheUnavailable, err)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return "", fmt.Errorf("%w: cache directory is writable by other users", ErrGoCacheUnavailable)
		}
	}
	return dir, nil
}

// insideDir reports whether path is root or below it, also comparing root's
// resolved location.
func insideDir(root, path string) bool {
	roots := []string{root}
	if abs, err := filepath.Abs(root); err == nil {
		roots = append(roots, abs)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		roots = append(roots, resolved)
	}
	for _, r := range roots {
		if rel, err := filepath.Rel(r, path); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return true
		}
	}
	return false
}
