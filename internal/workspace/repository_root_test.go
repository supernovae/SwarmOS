package workspace

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rootGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
}

func rootRepo(t *testing.T, dir string, initArgs ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootGit(t, dir, append([]string{"init", "-b", "main"}, initArgs...)...)
	rootGit(t, dir, "config", "user.name", "Test")
	rootGit(t, dir, "config", "user.email", "test@example.invalid")
	rootGit(t, dir, "commit", "--allow-empty", "-m", "fixture")
	return dir
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveCoordinatorRoot(t *testing.T) {
	base := canonicalTemp(t)
	main := rootRepo(t, filepath.Join(base, "main checkout"))
	issue := filepath.Join(main, ".swarm", "worktrees", "issue-78")
	pr := filepath.Join(main, ".swarm", "worktrees", "pr-9")
	rootGit(t, main, "worktree", "add", "-b", "swarm/issue-78-x", issue)
	rootGit(t, main, "worktree", "add", "-b", "swarm/pr-9", pr)
	nested := filepath.Join(issue, "internal", "cli")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(issue, alias); err != nil {
		t.Fatal(err)
	}
	// A primary checkout whose Git directory lives elsewhere.
	sep := rootRepo(t, filepath.Join(base, "sep"), "--separate-git-dir", filepath.Join(base, "sep.git"))
	sepTask := filepath.Join(sep, ".swarm", "worktrees", "issue-2")
	rootGit(t, sep, "worktree", "add", "-b", "swarm/issue-2-x", sepTask)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, nested)
	if err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"issue worktree": issue,
		"PR worktree":    pr,
		"descendant":     nested,
		"relative path":  relative,
		"symlink alias":  alias,
	} {
		t.Run(name, func(t *testing.T) {
			got, redirected, err := ResolveCoordinatorRoot(context.Background(), dir)
			if err != nil || !redirected || got != main {
				t.Fatalf("ResolveCoordinatorRoot(%q) = %q, %t, %v; want %q", dir, got, redirected, err, main)
			}
		})
	}
	t.Run("separate git dir", func(t *testing.T) {
		got, redirected, err := ResolveCoordinatorRoot(context.Background(), sepTask)
		if err != nil || !redirected || got != sep {
			t.Fatalf("got %q, %t, %v; want %q", got, redirected, err, sep)
		}
	})
}

func TestResolveCoordinatorRootPreservesOtherPaths(t *testing.T) {
	base := canonicalTemp(t)
	main := rootRepo(t, filepath.Join(base, "main"))
	unrelated := filepath.Join(base, "elsewhere", "issue-78")
	rootGit(t, main, "worktree", "add", "-b", "unrelated", unrelated)
	lookalike := filepath.Join(main, ".swarm", "worktrees-other", "issue-5")
	rootGit(t, main, "worktree", "add", "-b", "lookalike", lookalike)
	independent := rootRepo(t, filepath.Join(main, ".swarm", "worktrees", "issue-6"))
	plainDir := filepath.Join(main, ".swarm", "worktrees", "pr-78")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	notGit := filepath.Join(canonicalTemp(t), "issue-78")
	if err := os.MkdirAll(notGit, 0o755); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(base, "bare.git")
	rootGit(t, base, "clone", "--bare", main, bare)
	for name, dir := range map[string]string{
		"main checkout":                   main,
		"main subdirectory":               filepath.Join(main, ".swarm"),
		"linked worktree outside layout":  unrelated,
		"prefix lookalike":                lookalike,
		"independent repository":          independent,
		"plain directory named like task": plainDir,
		"non-Git directory":               notGit,
		"bare repository":                 bare,
		"missing directory":               filepath.Join(base, "missing"),
	} {
		t.Run(name, func(t *testing.T) {
			got, redirected, err := ResolveCoordinatorRoot(context.Background(), dir)
			if err != nil || redirected || got != dir {
				t.Fatalf("ResolveCoordinatorRoot(%q) = %q, %t, %v; want unchanged", dir, got, redirected, err)
			}
		})
	}
}

func TestResolveCoordinatorRootFailsClosed(t *testing.T) {
	base := canonicalTemp(t)
	assertRefused := func(t *testing.T, dir string) {
		t.Helper()
		got, redirected, err := ResolveCoordinatorRoot(context.Background(), dir)
		if !errors.Is(err, ErrMainCheckoutUnknown) || redirected || got != "" {
			t.Fatalf("ResolveCoordinatorRoot(%q) = %q, %t, %v; want refusal", dir, got, redirected, err)
		}
		for _, want := range []string{"cd to the main checkout", "--root"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %q", err, want)
			}
		}
	}

	t.Run("primary checkout missing", func(t *testing.T) {
		gitDir := filepath.Join(base, "missing.git")
		main := rootRepo(t, filepath.Join(base, "missing"), "--separate-git-dir", gitDir)
		task := filepath.Join(base, "tasks", ".swarm", "worktrees", "issue-1")
		rootGit(t, main, "worktree", "add", "-b", "t1", task)
		// The task's enclosing checkout never existed; Git metadata is intact.
		assertRefused(t, task)
	})
	t.Run("bare common repository", func(t *testing.T) {
		seed := rootRepo(t, filepath.Join(base, "seed"))
		bare := filepath.Join(base, "seed.git")
		rootGit(t, base, "clone", "--bare", seed, bare)
		owner := filepath.Join(base, "owner")
		if err := os.MkdirAll(owner, 0o755); err != nil {
			t.Fatal(err)
		}
		task := filepath.Join(owner, ".swarm", "worktrees", "issue-3")
		rootGit(t, bare, "worktree", "add", "-b", "t3", task)
		assertRefused(t, task)
	})
	t.Run("enclosing checkout is another repository", func(t *testing.T) {
		other := rootRepo(t, filepath.Join(base, "other"))
		source := rootRepo(t, filepath.Join(base, "source"))
		task := filepath.Join(other, ".swarm", "worktrees", "issue-4")
		rootGit(t, source, "worktree", "add", "-b", "t4", task)
		assertRefused(t, task)
	})
	t.Run("main metadata moved", func(t *testing.T) {
		main := rootRepo(t, filepath.Join(base, "moving"))
		task := filepath.Join(main, ".swarm", "worktrees", "issue-5")
		rootGit(t, main, "worktree", "add", "-b", "t5", task)
		if err := os.Rename(filepath.Join(main, ".git"), filepath.Join(base, "moved.git")); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, filepath.Join(task))
	})
}
