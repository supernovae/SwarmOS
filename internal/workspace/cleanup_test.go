package workspace

import (
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
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
	t.Parallel()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := gitfixture.Command(t, "", args...)
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
		c := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...)
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
	t.Parallel()
	for _, kind := range []string{"symlink-path", "symlink-parent", "artifact-symlink", "artifact-special", "unregistered", "foreign-common", "locked", "nested", "submodule", "checked-out-elsewhere"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
	for _, ignored := range []bool{false, true} {
		t.Run(fmt.Sprint(ignored), func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
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

func TestCleanupWorktreeRelativePaths(t *testing.T) {
	t.Parallel()
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprint(alias), func(t *testing.T) {
			t.Parallel()
			root, path, head, _ := cleanupWorkspaceFixture(t)
			if alias {
				link := filepath.Join(t.TempDir(), "repo")
				if err := os.Symlink(root, link); err != nil {
					t.Skip(err)
				}
				root = link
				path = WorktreePath(root, 1)
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			relativeRoot, err := filepath.Rel(cwd, root)
			if err != nil {
				t.Fatal(err)
			}
			relativePath, err := filepath.Rel(cwd, path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			r := InspectCleanupWorktree(ctx, relativeRoot, relativePath, "swarm/issue-1-test", head)
			if !r.Eligible {
				t.Fatalf("relative inspection: %+v", r)
			}
			if err := RetireCleanupWorktree(ctx, relativeRoot, relativePath, "swarm/issue-1-test", head); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("worktree remains: %v", err)
			}
		})
	}
}

func TestCleanupIgnoredBuildDirectories(t *testing.T) {
	for _, dir := range []string{"node_modules", "dist", "packages/a/node_modules", ".venv", "a/.next"} {
		t.Run(dir, func(t *testing.T) {
			root, path, _, git := cleanupWorkspaceFixture(t)
			external := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(external, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte(dir+"/\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("-C", path, "add", ".gitignore")
			git("-C", path, "commit", "-m", "ignore builds")
			head := git("-C", path, "rev-parse", "HEAD")
			build := filepath.Join(path, dir)
			if err := os.MkdirAll(build, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(build, "output"), []byte("build"), 0600); err != nil {
				t.Fatal(err)
			}
			for name, target := range map[string]string{"external": external, "internal": "output"} {
				if err := os.Symlink(target, filepath.Join(build, name)); err != nil {
					t.Fatal(err)
				}
			}
			branch := "swarm/issue-1-test"
			if r := InspectCleanupWorktree(context.Background(), root, path, branch, head); !r.Eligible {
				t.Fatal(r.Reason)
			}
			if _, err := os.Stat(filepath.Join(build, "output")); err != nil {
				t.Fatal("preview mutated output", err)
			}
			if err := RetireCleanupWorktree(context.Background(), root, path, branch, head); err != nil {
				t.Fatal(err)
			}
			if err := DeleteCleanupLocalBranch(context.Background(), root, branch, head); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("worktree survived", err)
			}
			if raw, err := os.ReadFile(external); err != nil || string(raw) != "keep" {
				t.Fatal("external target changed", err)
			}
		})
	}
}

func TestCleanupBuildRetention(t *testing.T) {
	for _, tc := range []struct{ name, file, ignore, tracked, marker string }{
		{name: "env", file: ".env", ignore: ".env"},
		{name: "other", file: "other/output", ignore: "other/"},
		{name: "untracked node", file: "node_modules/file"},
		{name: "untracked dist", file: "dist/file"},
		{name: "near match", file: "my_node_modules/file", ignore: "my_node_modules/"},
		{name: "regular dist", file: "dist", ignore: "dist"},
		{name: "tracked mix", file: "dist/o.js", ignore: "dist/o.js", tracked: "dist/keep.txt"},
		{name: "nested tracked mix", file: "a/dist/o.js", ignore: "a/dist/o.js", tracked: "a/dist/keep.txt"},
		{name: "ancestor bypass", file: "dist/node_modules/o.js", ignore: "dist/node_modules/", tracked: "dist/keep.txt"},
		{name: "nested repository directory", file: "node_modules/file", ignore: "node_modules/", marker: "dir"},
		{name: "nested repository file", file: "node_modules/file", ignore: "node_modules/", marker: "file"},
		{name: "recognized symlink", file: "node_modules", ignore: "node_modules", marker: "link"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, path, _, git := cleanupWorkspaceFixture(t)
			write := func(name, body string) {
				t.Helper()
				full := filepath.Join(path, name)
				if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(".gitignore", tc.ignore+"\n")
			if tc.tracked != "" {
				write(tc.tracked, "tracked")
			}
			git("-C", path, "add", ".")
			git("-C", path, "commit", "-m", "fixture")
			head := git("-C", path, "rev-parse", "HEAD")
			external := t.TempDir()
			if tc.marker == "link" {
				if err := os.Symlink(external, filepath.Join(path, tc.file)); err != nil {
					t.Fatal(err)
				}
			} else {
				write(tc.file, "payload")
			}
			if tc.marker == "dir" {
				if err := os.MkdirAll(filepath.Join(path, "node_modules/.git"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.marker == "file" {
				write("node_modules/.git", "gitdir: elsewhere")
			}
			branch := "swarm/issue-1-test"
			r := InspectCleanupWorktree(context.Background(), root, path, branch, head)
			if r.Eligible || r.Reason == "" {
				t.Fatalf("unexpected eligibility: %+v", r)
			}
			if tc.tracked != "" && (!strings.Contains(r.Reason, tc.tracked) || !strings.Contains(r.Reason, tc.file)) {
				t.Fatalf("missing blocking paths: %s", r.Reason)
			}
			if err := RetireCleanupWorktree(context.Background(), root, path, branch, head); err == nil {
				t.Fatal("retired blocked tree")
			}
			if _, err := os.Lstat(filepath.Join(path, tc.file)); err != nil {
				t.Fatal("payload lost", err)
			}
			if got := git("rev-parse", "refs/heads/"+branch); got != head {
				t.Fatal("branch changed")
			}
		})
	}
}

func TestCleanupCleanTrackedBuildDirectory(t *testing.T) {
	root, path, _, git := cleanupWorkspaceFixture(t)
	if err := os.MkdirAll(filepath.Join(path, "dist"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "dist/keep.txt"), []byte("tracked"), 0600); err != nil {
		t.Fatal(err)
	}
	git("-C", path, "add", ".")
	git("-C", path, "commit", "-m", "tracked dist")
	head := git("-C", path, "rev-parse", "HEAD")
	if err := RetireCleanupWorktree(context.Background(), root, path, "swarm/issue-1-test", head); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupTrackedBuildChanges(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprint(staged), func(t *testing.T) {
			root, path, _, git := cleanupWorkspaceFixture(t)
			if err := os.MkdirAll(filepath.Join(path, "dist"), 0755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(path, "dist/file")
			if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			git("-C", path, "add", ".")
			git("-C", path, "commit", "-m", "tracked build")
			head := git("-C", path, "rev-parse", "HEAD")
			if err := os.WriteFile(file, []byte("modified"), 0600); err != nil {
				t.Fatal(err)
			}
			if staged {
				git("-C", path, "add", ".")
			}
			if r := InspectCleanupWorktree(context.Background(), root, path, "swarm/issue-1-test", head); r.Eligible || !strings.Contains(r.Reason, "dist/file") {
				t.Fatalf("unexpected inspection: %+v", r)
			}
			if err := RetireCleanupWorktree(context.Background(), root, path, "swarm/issue-1-test", head); err == nil {
				t.Fatal("removed tracked changes")
			}
			if raw, err := os.ReadFile(file); err != nil || string(raw) != "modified" {
				t.Fatal("lost changes", err)
			}
		})
	}
}

func TestCleanupBuildComponentInspection(t *testing.T) {
	root := t.TempDir()
	if ok, err := cleanupIgnoredBuildPath(root, "dist/missing", nil); err == nil || ok {
		t.Fatal("accepted failed component inspection")
	}
	if err := os.Mkdir(filepath.Join(root, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "node_modules/repo"), 0755); err != nil {
		t.Fatal(err)
	}
	if ok, err := cleanupIgnoredBuildPath(root, "node_modules/repo/", nil); err != nil || !ok {
		t.Fatal("trailing slash rejected", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "parent")); err != nil {
		t.Fatal(err)
	}
	if ok, err := cleanupIgnoredBuildPath(root, "parent/dist/output", nil); err == nil || ok {
		t.Fatal("accepted symlink parent")
	}
}

func TestCleanupIndexInspectionFailure(t *testing.T) {
	root, path, head, git := cleanupWorkspaceFixture(t)
	index := git("-C", path, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(index) {
		index = filepath.Join(path, index)
	}
	if err := os.WriteFile(index, []byte("invalid index"), 0600); err != nil {
		t.Fatal(err)
	}
	branch := "swarm/issue-1-test"
	r := InspectCleanupWorktree(context.Background(), root, path, branch, head)
	if r.Eligible || !strings.Contains(r.Reason, "index inspection failed") {
		t.Fatalf("unexpected inspection: %+v", r)
	}
	if err := RetireCleanupWorktree(context.Background(), root, path, branch, head); err == nil {
		t.Fatal("removed uninspectable tree")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("worktree lost", err)
	}
	if got := git("rev-parse", "refs/heads/"+branch); got != head {
		t.Fatal("branch changed")
	}
}
