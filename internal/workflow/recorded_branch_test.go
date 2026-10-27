package workflow

import (
	"context"
	"os/exec"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

// Work after a title edit reuses the branch recorded by the first claim
// instead of deriving a new one and refusing the existing worktree.
func TestExecuteWorkTitleEditRecordedBranch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, out)
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatal(err)
	}
	configureValidation(t, tmp)
	for _, args := range [][]string{{"add", "."}, {"-c", "user.name=SwarmOS", "-c", "user.email=swarm@example.test", "commit", "-m", "initialize harness"}} {
		if out, err := exec.Command("git", append([]string{"-C", tmp}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	lt := tracker.NewLocalTracker(tmp).WithUser("local-dev", "OWNER")
	wu := &contract.WorkUnit{Objective: "Add version command", Constraints: []string{"Stdlib only"}, Acceptance: []string{"Outputs JSON"}, Workflow: "feature", Gate: contract.GateAgent}
	iss, err := lt.CreateIssue(ctx, "Add version command", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(tmp, lt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.LeaseManager.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWARM_MOCK_VERDICT", "APPROVED")
	planned, err := eng.ExecuteWork(ctx, WorkOptions{RepoRoot: tmp, IssueNumber: iss.Number, AgentName: "mock", Phase: "plan", Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	original := workspace.BranchName(iss.Number, "Add version command")
	if planned.BranchName != original || planned.State.Branch != original {
		t.Fatalf("first work used %q (state %q), want %q", planned.BranchName, planned.State.Branch, original)
	}
	_, _ = eng.LeaseManager.Release(ctx, iss.Number, "local-dev", "title edit")
	if _, err := lt.UpdateIssueTitle(ctx, iss.Number, "Print the version"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.LeaseManager.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	res, err := eng.ExecuteWork(ctx, WorkOptions{RepoRoot: tmp, IssueNumber: iss.Number, AgentName: "mock", Phase: "all", Gate: contract.GateAgent, Headless: true})
	if err != nil {
		t.Fatalf("work after title edit: %v", err)
	}
	if res.BranchName != original || res.State.Branch != original || res.WorktreePath != planned.WorktreePath {
		t.Fatalf("work moved to %q at %s, want %q at %s", res.BranchName, res.WorktreePath, original, planned.WorktreePath)
	}
}
