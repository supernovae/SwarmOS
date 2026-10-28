package workspace

import (
	"context"
	"github.com/supernovae/swarmos/internal/contract"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointExcludesTimeoutOutput(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"untracked", "staged", "tracked"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			wt, branch, _ := checkpointFixture(t)
			dir := filepath.Join(wt, ".swarm", "active-output")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			checkpointWrite(t, dir, "developer-timeout.patch", "recovery")
			if state != "untracked" {
				runWorkspaceGit(t, wt, "add", "-f", ".swarm/active-output")
			}
			if state == "tracked" {
				runWorkspaceGit(t, wt, "commit", "-m", "manual recovery commit")
			}
			checkpointWrite(t, wt, "staged.txt", "real implementation")
			_, err := Checkpoint(context.Background(), wt, branch, CheckpointOptions{WorkUnit: &contract.WorkUnit{IssueNumber: 47, Title: "Timeout"}, Purpose: "implement"})
			if state != "untracked" {
				if err == nil {
					t.Fatal("accepted tracked/staged recovery output")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := workspaceGitOutput(t, wt, "ls-tree", "-r", "--name-only", "HEAD", "--", ".swarm/active-output"); got != "" {
				t.Fatal(got)
			}
			if _, err := os.Stat(filepath.Join(dir, "developer-timeout.patch")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
