package workspace

import (
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionDroppedCheckpoint(t *testing.T) {
	for _, scenario := range []string{"clean", "markers", "staged markers", "hidden staged edit", "dirty", "staged", "untracked", "instruction edit", "mode", "local diverged", "remote diverged", "remote ancestor", "branch only", "fresh", "fresh clone", "wrong branch"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			git := func(dir string, args ...string) string {
				t.Helper()
				out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			remote := filepath.Join(t.TempDir(), "remote.git")
			git(root, "init", "--bare", remote)
			git(root, "init", "-b", "main")
			git(root, "config", "user.name", "Test")
			git(root, "config", "user.email", "test@example.test")
			write := func(dir, name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			write(root, "AGENTS.md", "old instructions\n")
			write(root, "source", "old\n")
			git(root, "add", ".")
			git(root, "commit", "-m", "old")
			old := git(root, "rev-parse", "HEAD")
			path, branch, err := ProvisionAt(root, 60, "old title", old, "", "")
			if err != nil {
				t.Fatal(err)
			}
			git(root, "remote", "add", "origin", remote)
			git(root, "push", "origin", branch)
			write(root, "AGENTS.md", "new instructions\n")
			write(root, "source", "new\n")
			git(root, "add", ".")
			git(root, "commit", "-m", "new")
			base := git(root, "rev-parse", "HEAD")
			rejected := false
			switch scenario {
			case "markers", "staged markers":
				write(path, "AGENTS.md", "<!-- swarm:active:start -->\npointer\n<!-- swarm:active:end -->\n\nold instructions\n")
				write(path, "CLAUDE.md", "<!-- swarm:active:start -->\npointer\n<!-- swarm:active:end -->\n")
				if scenario == "staged markers" {
					git(path, "add", "AGENTS.md", "CLAUDE.md")
				}
			case "hidden staged edit":
				write(path, "AGENTS.md", "staged user work\n")
				git(path, "add", "AGENTS.md")
				write(path, "AGENTS.md", "old instructions\n")
				rejected = true
			case "dirty":
				write(path, "source", "dirty")
				rejected = true
			case "staged":
				write(path, "source", "dirty")
				git(path, "add", "source")
				rejected = true
			case "untracked":
				write(path, "extra", "dirty")
				rejected = true
			case "instruction edit":
				write(path, "AGENTS.md", "user edit\n")
				rejected = true
			case "mode":
				if err := os.Chmod(filepath.Join(path, "source"), 0755); err != nil {
					t.Fatal(err)
				}
				rejected = true
			case "local diverged":
				git(path, "commit", "--allow-empty", "-m", "unique")
				rejected = true
			case "wrong branch":
				git(path, "checkout", "-b", "other")
				rejected = true
			case "remote diverged":
				git(path, "commit", "--allow-empty", "-m", "remote work")
				git(path, "push", "origin", branch)
				rejected = true
			case "remote ancestor":
				git(root, "push", "origin", base+":refs/heads/"+branch)
				rejected = true
			case "fresh clone":
				git(root, "push", "origin", "main")
				clone := filepath.Join(t.TempDir(), "clone")
				git(root, "clone", "--branch", "main", remote, clone)
				root = clone
				path = WorktreePath(root, 60)
			case "branch only":
				git(root, "worktree", "remove", path)
			case "fresh":
				git(root, "worktree", "remove", path)
				git(root, "branch", "-d", branch)
			}
			snapshot := func() string {
				t.Helper()
				var result strings.Builder
				for _, name := range []string{"AGENTS.md", "CLAUDE.md", "source", "extra"} {
					data, err := os.ReadFile(filepath.Join(path, name))
					info, statErr := os.Lstat(filepath.Join(path, name))
					result.WriteString(name)
					result.Write(data)
					if err != nil {
						result.WriteString(err.Error())
					}
					if statErr == nil {
						result.WriteString(info.Mode().String())
					}
				}
				result.WriteString(git(path, "ls-files", "--stage", "-z"))
				return result.String()
			}
			var beforeFiles string
			if rejected {
				beforeFiles = snapshot()
			}
			beforeRef := git(root, "show-ref")
			beforeRemote := git(root, "ls-remote", "origin")
			var beforeStatus string
			if scenario != "branch only" && scenario != "fresh" && scenario != "fresh clone" {
				beforeStatus = git(path, "status", "--porcelain")
			}
			recovered, gotBranch, err := ProvisionFromRemoteAt(root, "origin", 60, "changed title", base, "", "", &DroppedCheckpoint{Branch: branch, HeadSHA: old})
			if rejected {
				if err == nil {
					t.Fatal("unsafe recovery succeeded")
				}
				if snapshot() != beforeFiles || git(root, "show-ref") != beforeRef || git(root, "ls-remote", "origin") != beforeRemote || git(path, "status", "--porcelain") != beforeStatus {
					t.Fatal("refusal mutated checkout or refs")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if recovered != path || gotBranch != branch || git(path, "rev-parse", "HEAD") != base {
				t.Fatal("did not recover at new base on retired branch")
			}
			if git(root, "ls-remote", "origin") != beforeRemote {
				t.Fatal("provisioning published")
			}
			content, err := os.ReadFile(filepath.Join(path, "AGENTS.md"))
			if err != nil || string(content) != "new instructions\n" {
				t.Fatalf("instructions: %q %v", content, err)
			}
			if _, _, err := ProvisionFromRemoteAt(root, "origin", 60, "changed", base, "", "", &DroppedCheckpoint{Branch: branch, HeadSHA: old}); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}
