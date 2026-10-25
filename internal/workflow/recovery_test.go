package workflow

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestEngineDroppedCheckpointRecovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.test")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	configureValidation(t, root)
	git("add", ".")
	git("commit", "-m", "harness")
	remote := filepath.Join(t.TempDir(), "remote.git")
	git("init", "--bare", "--initial-branch=main", remote)
	git("remote", "add", "origin", remote)
	git("push", "origin", "main")
	tr := tracker.NewLocalTracker(root).WithUser("local-dev", "OWNER")
	wu := &contract.WorkUnit{Objective: "Recover checkpoint", Constraints: []string{"Keep commits"}, Acceptance: []string{"Resume at base"}, Workflow: "feature", Gate: contract.GateNone}
	issue, err := tr.CreateIssue(ctx, "Recover checkpoint", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(root, tr)
	if err != nil {
		t.Fatal(err)
	}
	m := eng.LeaseManager
	m.SetStore(root, coordination.NewGitStore(root))
	approved, err := m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	path, branch, err := workspace.ProvisionAt(root, issue.Number, issue.Title, approved.BaseSHA, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RecordCheckpoint(ctx, issue.Number, "local-dev", branch, approved.BaseSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, issue.Number, "local-dev", "test"); err != nil {
		t.Fatal(err)
	}
	git("commit", "--allow-empty", "-m", "new base")
	base := git("rev-parse", "HEAD")
	git("push", "origin", "main")
	if _, err := m.ApproveTask(ctx, issue.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	result, err := eng.ExecuteWork(ctx, WorkOptions{RepoRoot: root, IssueNumber: issue.Number, PlannerAgent: "mock", ReviewerAgent: "mock", DeveloperAgent: "mock", TesterAgent: "mock", Phase: "all", Gate: contract.GateNone, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorktreePath != path || result.BranchName != branch || result.BaseSHA != base || !result.ValidationPassed {
		t.Fatalf("unexpected recovery: %+v", result)
	}
	final, err := m.GetCurrentState(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	if final.DroppedCheckpoint != nil || final.HeadSHA == "" {
		t.Fatal("engine did not publish recovered checkpoint")
	}
	if ok, err := workspace.BaseIsAncestor(ctx, root, base, final.HeadSHA); err != nil || !ok {
		t.Fatalf("checkpoint ancestry: %v %v", ok, err)
	}
	if !strings.HasPrefix(git("ls-remote", "origin", "refs/heads/"+branch), final.HeadSHA) {
		t.Fatal("engine remote publication missing")
	}
}
