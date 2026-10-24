package workflow

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
)

func TestPreflightRejectsReviewerOutsideReviewerAgents(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatal(err)
	}
	configureValidation(t, tmp)
	cfgPath := filepath.Join(tmp, ".swarm", "config.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	pinned := strings.Replace(string(data), `ready_label: "swarm:ready"`, "ready_label: \"swarm:ready\"\n  reviewer_agents: [codex]", 1)
	if err := os.WriteFile(cfgPath, []byte(pinned), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", tmp, "add", "."}, {"-C", tmp, "commit", "-m", "pin reviewer"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	lt := tracker.NewLocalTracker(tmp).WithUser("local-dev", "OWNER")
	wu := &contract.WorkUnit{Objective: "Pin reviewers", Constraints: []string{"Tests"}, Acceptance: []string{"Rejected early"}, Workflow: "feature", Gate: contract.GateAgent}
	iss, err := lt.CreateIssue(ctx, "Pin reviewers", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
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
	_, err = eng.ExecuteWork(ctx, WorkOptions{RepoRoot: tmp, IssueNumber: iss.Number, AgentName: "mock", Phase: "all", Gate: contract.GateAgent, Headless: true})
	if err == nil || !strings.Contains(err.Error(), "security.reviewer_agents") {
		t.Fatalf("expected reviewer_agents preflight failure, got %v", err)
	}
	st, err := eng.LeaseManager.GetCurrentState(ctx, iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	if st.Lease.Status == "active" {
		t.Fatal("preflight failure still claimed the issue")
	}
}
