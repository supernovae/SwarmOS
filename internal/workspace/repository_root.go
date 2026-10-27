package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrMainCheckoutUnknown reports a SwarmOS task or PR worktree whose main
// checkout cannot be verified, so coordinator state has no safe home.
var ErrMainCheckoutUnknown = errors.New("cannot determine the main checkout")

// ResolveCoordinatorRoot maps dir to the main checkout when dir lies inside a
// linked worktree directly under that checkout's .swarm/worktrees. Any other
// path, including non-Git directories and unrelated worktrees, is returned
// unchanged. It never writes to the repository.
func ResolveCoordinatorRoot(ctx context.Context, dir string) (string, bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir, false, nil
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return dir, false, nil
	}
	top, err := gitOutputContext(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		// Git cannot open a task worktree whose main metadata moved; its
		// worktree .git file still marks it as one.
		if candidate := enclosingGitFile(abs); candidate != "" && managedLayout(candidate) != "" {
			return "", false, mainCheckoutUnknown(candidate, "its Git metadata is unavailable")
		}
		return dir, false, nil
	}
	top, err = canonicalPath(strings.TrimSpace(top))
	if err != nil {
		return dir, false, nil
	}
	gitDir, err1 := gitOutputContext(ctx, top, "rev-parse", "--path-format=absolute", "--git-dir")
	common, err2 := gitOutputContext(ctx, top, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err1 != nil || err2 != nil {
		return dir, false, nil
	}
	gitDir, common = strings.TrimSpace(gitDir), strings.TrimSpace(common)
	if samePath(gitDir, common) {
		return dir, false, nil // A main checkout or an independent repository.
	}
	main := managedLayout(top)
	if main == "" {
		return dir, false, nil // A linked worktree SwarmOS did not create.
	}
	if reason := verifyMainCheckout(ctx, main, top, common); reason != "" {
		return "", false, mainCheckoutUnknown(top, reason)
	}
	return main, true, nil
}

func mainCheckoutUnknown(worktree, reason string) error {
	return fmt.Errorf("%w for task worktree %s: %s; cd to the main checkout or pass --root <main-checkout>", ErrMainCheckoutUnknown, worktree, reason)
}

// managedLayout returns the checkout that would own top as a managed worktree
// (<main>/.swarm/worktrees/<name>), or "" when top is not at such a path.
func managedLayout(top string) string {
	parent := filepath.Dir(top)
	swarm := filepath.Dir(parent)
	if filepath.Base(parent) != "worktrees" || filepath.Base(swarm) != ".swarm" || top == parent {
		return ""
	}
	return filepath.Dir(swarm)
}

// verifyMainCheckout explains why main is not the primary checkout sharing
// common with the registered linked worktree top, or returns "".
func verifyMainCheckout(ctx context.Context, main, top, common string) string {
	if info, err := os.Stat(main); err != nil || !info.IsDir() {
		return "the enclosing checkout " + main + " is missing"
	}
	mainTop, err := gitOutputContext(ctx, main, "rev-parse", "--show-toplevel")
	if err != nil || !samePath(strings.TrimSpace(mainTop), main) {
		return main + " is not a Git checkout"
	}
	mainGitDir, err1 := gitOutputContext(ctx, main, "rev-parse", "--path-format=absolute", "--git-dir")
	mainCommon, err2 := gitOutputContext(ctx, main, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err1 != nil || err2 != nil {
		return "the Git metadata of " + main + " is unreadable"
	}
	if !samePath(strings.TrimSpace(mainCommon), common) {
		return main + " belongs to a different repository"
	}
	if !samePath(strings.TrimSpace(mainGitDir), common) {
		return main + " is itself a linked worktree"
	}
	registrations, err := gitOutputContext(ctx, main, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "worktree registrations are unreadable"
	}
	for _, record := range strings.Split(registrations, "\x00\x00") {
		fields := strings.Split(record, "\x00")
		if len(fields) > 0 && strings.HasPrefix(fields[0], "worktree ") && samePath(strings.TrimPrefix(fields[0], "worktree "), top) {
			return ""
		}
	}
	return "the worktree is not registered with " + main
}

// enclosingGitFile returns the nearest directory at or above dir whose .git
// entry is a regular file (a linked worktree), stopping at the first .git.
func enclosingGitFile(dir string) string {
	dir, err := canonicalPath(dir)
	if err != nil {
		return ""
	}
	for {
		if info, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			if info.Mode().IsRegular() {
				return dir
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}
