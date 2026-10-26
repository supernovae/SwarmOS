package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This probe records why automated removal stays disabled. Inspection alone
// cannot prevent ignored artifacts appearing before Git recursively removes
// the worktree, even without --force.
func TestCleanupArtifactNonForcedGitRemovalProbe(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = root
		if out, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git %v: %s %v", args, out, e)
		}
	}
	git("init")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	if e := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored\n"), 0600); e != nil {
		t.Fatal(e)
	}
	git("add", ".")
	git("commit", "-m", "fixture")
	path := filepath.Join(root, "task")
	git("worktree", "add", "-b", "task", path)
	// Deterministically insert an ignored file after the clean inspection.
	git("-C", path, "status", "--porcelain")
	if e := os.WriteFile(filepath.Join(path, "ignored"), []byte("artifact"), 0600); e != nil {
		t.Fatal(e)
	}
	git("worktree", "remove", path)
	if _, e := os.Stat(filepath.Join(path, "ignored")); !os.IsNotExist(e) {
		t.Fatalf("Git behavior changed; review removal safety: %v", e)
	}
}

func cleanupWorkspaceFixture(t *testing.T) (string, string, string, func(...string) string) {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	git("commit", "--allow-empty", "-m", "initial")
	path := WorktreePath(root, 1)
	branch := "swarm/issue-1-test"
	git("worktree", "add", "-b", branch, path)
	return root, path, git("rev-parse", "HEAD"), git
}

func TestCleanupWorktreeBoundaries(t *testing.T) {
	for _, kind := range []string{"symlink-path", "symlink-parent", "artifact-symlink", "artifact-special", "unregistered", "foreign-common", "locked", "nested", "submodule", "checked-out-elsewhere"} {
		t.Run(kind, func(t *testing.T) {
			root, path, head, git := cleanupWorkspaceFixture(t)
			ctx := context.Background()
			branch := "swarm/issue-1-test"
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(sentinel, []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "symlink-path":
				git("worktree", "remove", path)
				must(os.Symlink(outside, path))
			case "symlink-parent":
				git("worktree", "remove", path)
				must(os.Remove(filepath.Dir(path)))
				must(os.Symlink(outside, filepath.Dir(path)))
			case "artifact-symlink":
				must(os.MkdirAll(filepath.Join(path, ".swarm/active"), 0700))
				must(os.Symlink(sentinel, filepath.Join(path, ".swarm/active/PLAN.md")))
			case "artifact-special":
				must(os.MkdirAll(filepath.Join(path, ".swarm/active"), 0700))
				c := exec.Command("mkfifo", filepath.Join(path, ".swarm/active/pipe"))
				if err := c.Run(); err != nil {
					t.Skip("mkfifo unavailable")
				}
			case "unregistered":
				git("worktree", "remove", path)
				must(os.MkdirAll(path, 0700))
			case "foreign-common":
				git("worktree", "remove", path)
				must(os.MkdirAll(path, 0700))
				git("-C", path, "init")
			case "locked":
				git("worktree", "lock", path)
			case "nested", "submodule":
				name := "nested"
				if kind == "submodule" {
					name = "module"
				}
				must(os.MkdirAll(filepath.Join(path, name), 0700))
				git("-C", filepath.Join(path, name), "init")
			case "checked-out-elsewhere":
				git("worktree", "remove", path)
				git("worktree", "add", filepath.Join(root, "other"), branch)
			}
			if kind == "checked-out-elsewhere" {
				r := InspectCleanupBranch(ctx, root, branch, head, nil)
				if r.Eligible || !strings.Contains(r.Reason, "checked out") {
					t.Fatalf("%+v", r)
				}
				if err := DeleteCleanupLocalBranch(ctx, root, branch, head); err == nil {
					t.Fatal("checked-out branch deleted")
				}
			} else {
				r := InspectCleanupWorktree(ctx, root, path, branch, head)
				if r.Eligible || r.Reason == "" {
					t.Fatalf("unsafe eligible %+v", r)
				}
				if err := RetireCleanupWorktree(ctx, root, path, branch, head); err == nil {
					t.Fatal("unsafe removal succeeded")
				}
			}
			if got := git("rev-parse", "refs/heads/"+branch); got != head {
				t.Fatal("branch changed")
			}
			b, err := os.ReadFile(sentinel)
			if err != nil || string(b) != "outside" {
				t.Fatal("outside sentinel changed")
			}
		})
	}
}

func TestCleanupLateFileRetention(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		t.Run(fmt.Sprint(ignored), func(t *testing.T) {
			root, path, head, git := cleanupWorkspaceFixture(t)
			ctx := context.Background()
			branch := "swarm/issue-1-test"
			if r := InspectCleanupWorktree(ctx, root, path, branch, head); !r.Eligible {
				t.Fatalf("%+v", r)
			}
			if ignored {
				git("config", "core.excludesFile", filepath.Join(root, "excludes"))
				if err := os.WriteFile(filepath.Join(root, "excludes"), []byte("late\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(path, "late")
			if err := os.WriteFile(target, []byte("late bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := RetireCleanupWorktree(ctx, root, path, branch, head); err == nil {
				t.Fatal("late file discarded")
			}
			b, err := os.ReadFile(target)
			if err != nil || string(b) != "late bytes" {
				t.Fatal("late bytes lost")
			}
		})
	}
}

func TestCleanupTrackedManagedArtifact(t *testing.T) {
	root, path, _, git := cleanupWorkspaceFixture(t)
	target := filepath.Join(path, ".swarm/active/tracked")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("tracked content"), 0600); err != nil {
		t.Fatal(err)
	}
	git("-C", path, "add", "-f", ".swarm/active/tracked")
	git("-C", path, "commit", "-m", "tracked managed file")
	head := git("-C", path, "rev-parse", "HEAD")
	if err := RetireCleanupWorktree(context.Background(), root, path, "swarm/issue-1-test", head); err != nil {
		t.Fatalf("tracked managed file removed before Git: %v", err)
	}
}
