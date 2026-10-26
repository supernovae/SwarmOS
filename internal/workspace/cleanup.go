package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/safefs"
)

// CleanupInspection describes one resource without updating the index or refs.
type CleanupInspection struct {
	Head     string
	Branch   string
	Absent   bool
	Eligible bool
	Reason   string
}

func cleanupOutput(ctx context.Context, root string, args ...string) (string, error) {
	cmd, redact, err := gitcredential.Command(ctx, root, args...)
	if err != nil {
		return "", err
	}
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSuffix(redact(string(out)), "\n"), nil
}

func cleanupManaged(name string) bool {
	return strings.HasPrefix(name, ".swarm/active/") || strings.HasPrefix(name, ".swarm/active-output/")
}

// Normalize the repository anchor only: resolving the worktree itself would
// conceal symlink traversal from the component-by-component safety checks.
func cleanupPaths(root, path string) (string, string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", "", err
	}
	if rel, err := filepath.Rel(absoluteRoot, absolutePath); err == nil && filepath.IsLocal(rel) {
		absolutePath = filepath.Join(canonicalRoot, rel)
	}
	return filepath.Clean(canonicalRoot), filepath.Clean(absolutePath), nil
}

// InspectCleanupWorktree accepts only registered task worktrees in this repository.
func InspectCleanupWorktree(ctx context.Context, root, path, branch, expected string) CleanupInspection {
	r := CleanupInspection{Branch: branch}
	fail := func(reason string) CleanupInspection { r.Reason = reason; return r }
	root, path, err := cleanupPaths(root, path)
	if err != nil {
		return fail("resolve cleanup paths: " + err.Error())
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) || !strings.HasPrefix(filepath.ToSlash(rel), ".swarm/worktrees/") {
		return fail("outside managed worktree paths")
	}
	// Inspect every parent before treating an absent leaf as absent.
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, e := os.Lstat(current)
		if os.IsNotExist(e) {
			r.Absent = true
			return fail("already absent")
		}
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fail("unsafe worktree path: " + current)
		}
	}
	gitFile, e := os.Lstat(filepath.Join(path, ".git"))
	if e != nil || !gitFile.Mode().IsRegular() {
		return fail("unfamiliar worktree Git registration")
	}
	common, e := cleanupOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	other, e2 := cleanupOutput(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if e != nil || e2 != nil || common != other {
		return fail("different common Git directory")
	}
	invoking, _ := cleanupOutput(ctx, root, "rev-parse", "--show-toplevel")
	if filepath.Clean(invoking) == path {
		return fail("invoking checkout is retained")
	}
	registrations, e := cleanupOutput(ctx, root, "worktree", "list", "--porcelain", "-z")
	if e != nil {
		return fail("worktree registration inspection failed")
	}
	found := false
	for _, record := range strings.Split(registrations, "\x00\x00") {
		fields := strings.Split(record, "\x00")
		if len(fields) > 0 && strings.HasPrefix(fields[0], "worktree ") && filepath.Clean(strings.TrimPrefix(fields[0], "worktree ")) == path {
			found = true
			for _, field := range fields {
				if strings.HasPrefix(field, "locked") {
					return fail("locked worktree")
				}
			}
		}
	}
	if !found {
		return fail("unregistered worktree")
	}
	r.Head, e = cleanupOutput(ctx, path, "rev-parse", "HEAD")
	if e != nil || r.Head != expected || expected == "" {
		return fail(fmt.Sprintf("HEAD %s differs from submitted tip %s", r.Head, expected))
	}
	actual, _ := cleanupOutput(ctx, path, "symbolic-ref", "-q", "HEAD")
	if actual != "" && actual != "refs/heads/"+branch {
		return fail("unexpected checked-out branch: " + actual)
	}
	tip, _ := cleanupOutput(ctx, root, "rev-parse", "--verify", "refs/heads/"+branch)
	if tip != "" && tip != expected {
		return fail("local branch advanced since submission: " + tip)
	}
	// Enumerate tracked changes separately so managed paths never excuse them.
	for _, args := range [][]string{{"diff", "--cached", "--name-only", "-z", "HEAD", "--"}, {"diff", "--name-only", "-z", "HEAD", "--"}, {"ls-files", "--others", "--exclude-standard", "-z"}, {"ls-files", "--others", "--ignored", "--exclude-standard", "-z"}} {
		out, err := cleanupOutput(ctx, path, args...)
		if err != nil {
			return fail("cleanliness inspection failed")
		}
		for _, name := range strings.Split(out, "\x00") {
			if name != "" && (args[0] == "diff" || !cleanupManaged(name)) {
				return fail(fmt.Sprintf("blocking path %q", name))
			}
		}
	}
	staged, err := cleanupOutput(ctx, path, "ls-files", "--stage", "-z")
	if err != nil {
		return fail("index inspection failed")
	}
	for _, entry := range strings.Split(staged, "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			return fail("submodule is retained: " + entry)
		}
	}
	// Walk also catches nested repositories, symlinks and special managed artifacts.
	err = filepath.WalkDir(path, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		name, _ := filepath.Rel(path, p)
		name = filepath.ToSlash(name)
		if name == ".git" {
			return nil
		}
		if d.IsDir() && name != "." {
			if _, e := os.Lstat(filepath.Join(p, ".git")); e == nil {
				return fmt.Errorf("nested repository: %s", name)
			}
		}
		if name == ".swarm" || name == ".swarm/active" || name == ".swarm/active-output" || cleanupManaged(name) {
			if d.Type()&os.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
				return fmt.Errorf("unsafe managed artifact: %s", name)
			}
		}
		return nil
	})
	if err != nil {
		return fail(err.Error())
	}
	r.Eligible = true
	r.Reason = "verified merged submitted tip"
	return r
}

// RetireCleanupWorktree reinspects before removing disposable artifacts and invoking Git.
// Writers must be stopped: inspection and Git removal are not an atomic operation.
func RetireCleanupWorktree(ctx context.Context, root, path, branch, expected string) error {
	root, path, err := cleanupPaths(root, path)
	if err != nil {
		return fmt.Errorf("resolve cleanup paths: %w", err)
	}
	r := InspectCleanupWorktree(ctx, root, path, branch, expected)
	if r.Absent {
		return nil
	}
	if !r.Eligible {
		return fmt.Errorf("%s", r.Reason)
	}
	pinned, err := safefs.Open(path)
	if err != nil {
		return err
	}
	defer pinned.Close()
	// Remove only untracked managed leaves, including ignored leaves. Tracked
	// managed files belong to the checkout and must be left for Git to remove.
	for _, args := range [][]string{{"ls-files", "--others", "--exclude-standard", "-z"}, {"ls-files", "--others", "--ignored", "--exclude-standard", "-z"}} {
		out, e := cleanupOutput(ctx, path, args...)
		if e != nil {
			return e
		}
		for _, name := range strings.Split(out, "\x00") {
			if name == "" || !cleanupManaged(name) {
				continue
			}
			f, e := pinned.OpenRegular(name)
			if e != nil {
				return e
			}
			f.Close()
			if e = pinned.Remove(name); e != nil {
				return e
			}
		}
	}
	_, err = cleanupOutput(ctx, root, "worktree", "remove", path)
	return err
}

func InspectCleanupBranch(ctx context.Context, root, branch, expected string, retiringPaths []string) CleanupInspection {
	r := CleanupInspection{Branch: branch}
	r.Head, _ = cleanupOutput(ctx, root, "rev-parse", "--verify", "refs/heads/"+branch)
	if r.Head == "" {
		r.Absent = true
		r.Reason = "already absent"
		return r
	}
	if expected == "" || r.Head != expected {
		r.Reason = "local branch advanced since submission: " + r.Head
		return r
	}
	out, err := cleanupOutput(ctx, root, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		r.Reason = "worktree inspection failed"
		return r
	}
	for _, record := range strings.Split(out, "\x00\x00") {
		fields := strings.Split(record, "\x00")
		path := ""
		checked := false
		for _, f := range fields {
			if strings.HasPrefix(f, "worktree ") {
				path = strings.TrimPrefix(f, "worktree ")
			}
			if f == "branch refs/heads/"+branch {
				checked = true
			}
		}
		if checked {
			allowed := false
			for _, p := range retiringPaths {
				if p == path {
					allowed = true
				}
			}
			if !allowed {
				r.Reason = "branch checked out in retained worktree: " + path
				return r
			}
		}
	}
	r.Eligible = true
	r.Reason = "verified merged submitted tip"
	return r
}

func DeleteCleanupLocalBranch(ctx context.Context, root, branch, expected string) error {
	r := InspectCleanupBranch(ctx, root, branch, expected, nil)
	if r.Absent {
		return nil
	}
	if !r.Eligible {
		return fmt.Errorf("%s", r.Reason)
	}
	_, err := cleanupOutput(ctx, root, "update-ref", "-d", "refs/heads/"+branch, expected)
	if err != nil {
		current, _ := cleanupOutput(ctx, root, "rev-parse", "--verify", "refs/heads/"+branch)
		return fmt.Errorf("branch CAS failed: expected %s, current %s: %w", expected, current, err)
	}
	return nil
}
