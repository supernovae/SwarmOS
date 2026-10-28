package workspace

import (
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// discardFixture is a repository whose issue 7 worktree holds an abandoned
// checkpoint C on its own branch, while main advanced to an unrelated base B.
type discardFixture struct {
	root, path, base, head string
	fresh                  *FreshStart
}

func newDiscardFixture(t *testing.T) discardFixture {
	t.Helper()
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.test"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "init", "-b", "main")
	git(root, "commit", "--allow-empty", "-m", "A")
	a := git(root, "rev-parse", "HEAD")
	path, branch, err := ProvisionAt(root, 7, "task", a, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "abandoned.txt"), []byte("old attempt"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(path, "add", "abandoned.txt")
	git(path, "commit", "-m", "C")
	head := git(path, "rev-parse", "HEAD")
	git(root, "commit", "--allow-empty", "-m", "B")
	base := git(root, "rev-parse", "HEAD")
	return discardFixture{root: root, path: path, base: base, head: head, fresh: &FreshStart{Branch: FreshStartBranch(7, head), AbandonedBranch: branch, AbandonedHeadSHA: head}}
}

func (f discardFixture) provision() (string, string, error) {
	return ProvisionWithContext(f.root, "", 7, "task", f.base, f.fresh.Branch, "", ProvisionContext{FreshStart: f.fresh})
}

func (f discardFixture) archive(suffix string) string {
	return filepath.Join(filepath.Dir(f.path), "issue-7-discarded-"+f.head[:12]+suffix)
}

func TestDiscardCheckpointProvisionsFreshBranch(t *testing.T) {
	for _, scenario := range []string{"clean", "dirty", "absent", "archive collision", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDiscardFixture(t)
			archive := f.archive("")
			switch scenario {
			case "dirty":
				if err := os.WriteFile(filepath.Join(f.path, "untracked.txt"), []byte("keep me"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.path, "abandoned.txt"), []byte("edited"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "absent":
				runWorkspaceGit(t, f.root, "worktree", "remove", f.path)
			case "archive collision":
				if err := os.MkdirAll(archive, 0o755); err != nil {
					t.Fatal(err)
				}
				archive = f.archive("-2")
			}
			path, branch, err := f.provision()
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "retry" {
				if path, branch, err = f.provision(); err != nil {
					t.Fatalf("retry refused: %v", err)
				}
			}
			if path != f.path || branch != f.fresh.Branch {
				t.Fatalf("got %s %s", path, branch)
			}
			if got := workspaceGitOutput(t, path, "rev-parse", "HEAD"); got != f.base {
				t.Fatalf("fresh attempt at %s, want base %s", got, f.base)
			}
			if _, err := os.Stat(filepath.Join(path, "abandoned.txt")); !os.IsNotExist(err) {
				t.Fatal("fresh attempt carries abandoned changes")
			}
			if got := workspaceGitOutput(t, f.root, "rev-parse", f.fresh.AbandonedBranch); got != f.head {
				t.Fatal("abandoned branch moved")
			}
			if scenario == "absent" {
				if _, err := os.Lstat(archive); !os.IsNotExist(err) {
					t.Fatal("archived a missing worktree")
				}
				return
			}
			if got := workspaceGitOutput(t, archive, "rev-parse", "HEAD"); got != f.head {
				t.Fatalf("archive at %s, want %s", got, f.head)
			}
			if b := workspaceGitOutput(t, archive, "symbolic-ref", "--short", "HEAD"); b != f.fresh.AbandonedBranch {
				t.Fatalf("archive on %s", b)
			}
			if scenario == "dirty" {
				for name, want := range map[string]string{"untracked.txt": "keep me", "abandoned.txt": "edited"} {
					got, err := os.ReadFile(filepath.Join(archive, name))
					if err != nil || string(got) != want {
						t.Fatalf("lost %s: %q %v", name, got, err)
					}
				}
			}
			if scenario == "archive collision" {
				if entries, _ := os.ReadDir(f.archive("")); len(entries) != 0 {
					t.Fatal("wrote into an existing archive")
				}
			}
		})
	}
}

func TestDiscardCheckpointSetAsideRefusals(t *testing.T) {
	for _, scenario := range []string{"other branch", "rebase in progress", "symlink", "foreign repository", "unrelated head"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDiscardFixture(t)
			switch scenario {
			case "other branch":
				runWorkspaceGit(t, f.path, "checkout", "-b", "swarm/issue-7-other")
			case "rebase in progress":
				marker := workspaceGitOutput(t, f.path, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge")
				if err := os.MkdirAll(marker, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				runWorkspaceGit(t, f.root, "worktree", "move", f.path, f.path+"-real")
				if err := os.Symlink(f.path+"-real", f.path); err != nil {
					t.Skip(err)
				}
			case "foreign repository":
				runWorkspaceGit(t, f.root, "worktree", "remove", "--force", f.path)
				other := t.TempDir()
				runWorkspaceGit(t, other, "init", "-b", "main")
				runWorkspaceGit(t, other, "-c", "user.name=T", "-c", "user.email=t@example.test", "commit", "--allow-empty", "-m", "x")
				runWorkspaceGit(t, other, "worktree", "add", "-b", f.fresh.AbandonedBranch, f.path)
			case "unrelated head":
				runWorkspaceGit(t, f.path, "reset", "--hard", f.base)
			}
			before := workspaceGitOutput(t, f.root, "worktree", "list", "--porcelain")
			if _, _, err := f.provision(); err == nil || !strings.Contains(err.Error(), "cannot set aside the abandoned worktree") {
				t.Fatalf("not refused: %v", err)
			}
			if after := workspaceGitOutput(t, f.root, "worktree", "list", "--porcelain"); after != before {
				t.Fatal("refusal changed worktrees")
			}
			if _, err := os.Lstat(f.archive("")); !os.IsNotExist(err) {
				t.Fatal("refusal created an archive")
			}
		})
	}
}

func TestDiscardCheckpointProvisionErrorNamesCommand(t *testing.T) {
	f := newDiscardFixture(t)
	_, _, err := ProvisionWithContext(f.root, "", 7, "task", f.base, f.fresh.AbandonedBranch, f.head, ProvisionContext{})
	if err == nil {
		t.Fatal("divergent checkpoint provisioned")
	}
	for _, want := range []string{"reconcile the branch without resetting it", "swarm issue discard-checkpoint 7 --expected-head " + f.head, "maintainer", "abandoned"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}
