package workspace

import (
	"path/filepath"
	"testing"
)

func TestRebindBudgetCheckpointPreservesOldBranchAndRefusesDirtyWork(t *testing.T) {
	wt, oldBranch, head := checkpointFixture(t)
	common := workspaceGitOutput(t, wt, "rev-parse", "--git-common-dir")
	root := filepath.Dir(common)
	newBranch := "swarm/issue-47-newclaim"
	if err := RebindBudgetCheckpoint(t.Context(), root, 47, oldBranch, newBranch, head); err != nil {
		t.Fatal(err)
	}
	if got := workspaceGitOutput(t, root, "rev-parse", oldBranch); got != head {
		t.Fatalf("old claim branch changed: %s", got)
	}
	if got := workspaceGitOutput(t, wt, "branch", "--show-current"); got != newBranch {
		t.Fatalf("new claim branch: %s", got)
	}
	checkpointWrite(t, wt, "partial.txt", "keep my work")
	if err := RebindBudgetCheckpoint(t.Context(), root, 47, newBranch, "swarm/issue-47-nextclaim", head); err == nil {
		t.Fatal("dirty worktree rebound")
	}
	if got := workspaceGitOutput(t, wt, "branch", "--show-current"); got != newBranch {
		t.Fatal("rejected rebind changed branch")
	}
}
