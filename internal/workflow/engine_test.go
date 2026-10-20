package workflow

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
)

func TestEnginePlanGateLifecycle(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatal(err)
	}

	lt := tracker.NewLocalTracker(tmp)
	wu := &contract.WorkUnit{
		Objective:   "Add version command",
		Constraints: []string{"Stdlib only"},
		Acceptance:  []string{"Outputs JSON"},
		Workflow:    "feature",
		Gate:        contract.GateAgent,
	}
	iss, err := lt.CreateIssue(ctx, "Add version command", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}

	eng, err := NewEngine(tmp, lt)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Test GateAgent rejection when SWARM_MOCK_VERDICT=CHANGES_REQUESTED
	t.Setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	_, err = eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:      tmp,
		IssueNumber:   iss.Number,
		AgentName:     "mock",
		ReviewerAgent: "mock",
		Phase:         "all",
		Gate:          contract.GateAgent,
		Headless:      true,
	})
	if !errors.Is(err, ErrPlanReviewRejected) {
		t.Fatalf("expected ErrPlanReviewRejected, got: %v", err)
	}

	// 2. Test GateAgent approval and full execution
	t.Setenv("SWARM_MOCK_VERDICT", "APPROVED")
	res, err := eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:      tmp,
		IssueNumber:   iss.Number,
		AgentName:     "mock",
		ReviewerAgent: "mock",
		Phase:         "all",
		Gate:          contract.GateAgent,
		Headless:      true,
	})
	if err != nil {
		t.Fatalf("expected GateAgent full workflow to succeed, got: %v", err)
	}
	if !res.ValidationPassed || res.ReviewVerdict != "APPROVED" {
		t.Fatalf("unexpected result: %+v", res)
	}
}
