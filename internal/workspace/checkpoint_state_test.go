package workspace

import (
	"context"
	"encoding/json"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectCheckpointFourHeadsAndPreservesWork(t *testing.T) {
	for _, scenario := range []string{"equal", "local rewrite", "remote rewrite", "divergent remote", "missing branch", "missing worktree", "detached", "transport error"} {
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
			git(root, "init", "-b", "main")
			git(root, "config", "user.name", "Test")
			git(root, "config", "user.email", "test@example.invalid")
			if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("base"), 0644); err != nil {
				t.Fatal(err)
			}
			git(root, "add", "tracked")
			git(root, "commit", "-m", "base")
			a := git(root, "rev-parse", "HEAD")
			remote := filepath.Join(t.TempDir(), "remote.git")
			git(root, "init", "--bare", "--initial-branch=main", remote)
			git(root, "remote", "add", "origin", remote)
			git(root, "push", "origin", "main")
			path, branch, err := ProvisionAt(root, 1, "test", a, "", "")
			if err != nil {
				t.Fatal(err)
			}
			git(root, "push", "origin", a+":refs/heads/"+branch)
			git(root, "commit", "--allow-empty", "-m", "B")
			b := git(root, "rev-parse", "HEAD")
			git(root, "commit", "--allow-empty", "-m", "C")
			c := git(root, "rev-parse", "HEAD")
			switch scenario {
			case "local rewrite":
				git(path, "reset", "--hard", b)
			case "remote rewrite":
				git(root, "push", "origin", b+":refs/heads/"+branch)
			case "divergent remote":
				git(path, "reset", "--hard", b)
				git(root, "push", "origin", c+":refs/heads/"+branch)
			case "missing branch":
				git(root, "worktree", "remove", path)
				git(root, "branch", "-D", branch)
			case "missing worktree":
				git(root, "worktree", "remove", path)
			case "detached":
				git(path, "checkout", "--detach")
			case "transport error":
				git(root, "remote", "set-url", "origin", filepath.Join(root, "absent"))
			}
			exists := scenario != "missing branch" && scenario != "missing worktree"
			if exists {
				if err := os.WriteFile(filepath.Join(path, "tracked"), []byte("staged bytes"), 0644); err != nil {
					t.Fatal(err)
				}
				git(path, "add", "tracked")
				if err := os.WriteFile(filepath.Join(path, "tracked"), []byte("working bytes"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "untracked"), []byte("untracked bytes"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			refs := git(root, "show-ref")
			var status, index string
			if exists {
				status = git(path, "status", "--porcelain")
				index = git(path, "show", ":tracked")
			}
			s := InspectCheckpoint(context.Background(), root, "origin", 1, branch, a)
			if s.Recorded.SHA != a {
				t.Fatal("lost authoritative observation")
			}
			if scenario == "transport error" {
				if s.RemoteBranch.Error == "" || s.RemoteBranch.Missing {
					t.Fatal("transport error hidden as missing")
				}
			} else if s.RemoteBranch.SHA == "" {
				t.Fatal("remote observation missing")
			}
			if scenario == "missing branch" && !s.LocalBranch.Missing {
				t.Fatalf("missing branch is an error: %+v", s.LocalBranch)
			}
			if !exists && !s.Worktree.Missing {
				t.Fatal("worktree missing not reported")
			}
			if exists && !s.Dirty {
				t.Fatal("dirty files not detected")
			}
			if scenario == "local rewrite" || scenario == "divergent remote" {
				if s.Worktree.SHA != b || s.LocalBranch.SHA != b {
					t.Fatal("local rewrite not detected")
				}
			}
			if scenario == "divergent remote" && s.RemoteBranch.SHA != c {
				t.Fatal("live remote omitted")
			}
			if scenario == "detached" && s.AttachedBranch != "" {
				t.Fatal("detached branch not detected")
			}
			if scenario != "equal" && exists {
				if _, _, err := ProvisionAt(root, 1, "test", a, branch, a); err == nil && exists {
					t.Fatal("divergent resume accepted")
				}
			}
			if git(root, "show-ref") != refs {
				t.Fatal("inspection/resume changed refs")
			}
			if exists {
				if git(path, "status", "--porcelain") != status || git(path, "show", ":tracked") != index {
					t.Fatal("index changed")
				}
				for name, want := range map[string]string{"tracked": "working bytes", "untracked": "untracked bytes"} {
					got, err := os.ReadFile(filepath.Join(path, name))
					if err != nil || string(got) != want {
						t.Fatal("worktree bytes changed")
					}
				}
			}
			raw, err := json.Marshal(s)
			if err != nil || !strings.Contains(string(raw), a) {
				t.Fatal("JSON lost full SHA")
			}
		})
	}
}
