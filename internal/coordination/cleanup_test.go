package coordination

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupAtomicBranchAndStateFences(t *testing.T) {
	for _, scenario := range []string{"success", "advanced-branch", "changed-state", "unsupported-atomic", "protected-state", "protected-contributors", "protected-checkpoints", "wrong-push-remote"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			git := func(dir string, args ...string) string {
				t.Helper()
				out, e := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
				if e != nil {
					t.Fatalf("git %v: %s %v", args, out, e)
				}
				return strings.TrimSpace(string(out))
			}
			remote := filepath.Join(root, "remote.git")
			git(root, "init", "--bare", "--initial-branch=main", remote)
			local := filepath.Join(root, "local")
			git(root, "clone", remote, local)
			git(local, "config", "user.name", "Test")
			git(local, "config", "user.email", "test@example.invalid")
			git(local, "commit", "--allow-empty", "-m", "base")
			git(local, "push", "origin", "main")
			st := NewGitStore(local)
			rev, e := st.CompareAndSwap(ctx, 1, "", []byte(`{"reserved":true}`))
			if e != nil {
				t.Fatal(e)
			}
			head := git(local, "rev-parse", "HEAD")
			branch := "swarm/issue-1-cleanup"
			git(local, "push", "origin", head+":refs/heads/"+branch)
			switch scenario {
			case "advanced-branch":
				git(local, "commit", "--allow-empty", "-m", "advance")
				git(local, "push", "origin", "HEAD:refs/heads/"+branch)
			case "changed-state":
				if _, e = st.CompareAndSwap(ctx, 1, rev, []byte(`{"successor":true}`)); e != nil {
					t.Fatal(e)
				}
			case "wrong-push-remote":
				git(local, "remote", "set-url", "--push", "origin", filepath.Join(root, "wrong.git"))
			case "unsupported-atomic":
				git(remote, "config", "receive.advertiseAtomic", "false")
			case "protected-state":
				branch = "swarm-state/issue-1"
			case "protected-checkpoints":
				branch = "swarm-checkpoints/issue-1/" + head
				git(local, "push", "origin", head+":refs/heads/"+branch)
			case "protected-contributors":
				branch = "swarm-contributors/alice"
			}
			beforeState := git(remote, "rev-parse", "refs/heads/swarm-state/issue-1")
			beforeBranch := git(remote, "rev-parse", "refs/heads/swarm/issue-1-cleanup")
			_, e = st.CompareAndSwapAndDeleteBranch(ctx, 1, rev, []byte(`{"remote_deleted":true}`), branch, head)
			if scenario == "success" {
				if e != nil {
					t.Fatal(e)
				}
				if got := git(remote, "rev-parse", CheckpointRef(1, head)); got != head {
					t.Fatal("cleanup lost retained checkpoint")
				}
				if out := git(remote, "for-each-ref", "refs/heads/swarm/issue-1-cleanup"); out != "" {
					t.Fatal("branch survived")
				}
				raw, _, e := st.Load(ctx, 1)
				if e != nil || !strings.Contains(string(raw), "remote_deleted") {
					t.Fatalf("progress missing: %s %v", raw, e)
				}
			} else {
				if e == nil {
					t.Fatal("unsafe cleanup succeeded")
				}
				if scenario == "advanced-branch" || scenario == "changed-state" {
					if !errors.Is(e, ErrConflict) {
						t.Fatalf("lost conflict sentinel: %v", e)
					}
				}
				if got := git(remote, "rev-parse", "refs/heads/swarm-state/issue-1"); got != beforeState {
					t.Fatal("failed deletion changed state")
				}
				if got := git(remote, "rev-parse", "refs/heads/swarm/issue-1-cleanup"); got != beforeBranch {
					t.Fatal("failed deletion changed branch")
				}
			}
		})
	}
}
