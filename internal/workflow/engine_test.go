package workflow

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
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
		RepoRoot:       tmp,
		IssueNumber:    iss.Number,
		PlannerAgent:   "mock",
		ReviewerAgent:  "mock",
		DeveloperAgent: "mock",
		TesterAgent:    "mock",
		Phase:          "all",
		Gate:           contract.GateAgent,
		Headless:       true,
	})
	if !errors.Is(err, ErrPlanReviewRejected) {
		t.Fatalf("expected ErrPlanReviewRejected, got: %v", err)
	}

	// 2. Test GateAgent approval and full multi-role execution
	t.Setenv("SWARM_MOCK_VERDICT", "APPROVED")
	res, err := eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:       tmp,
		IssueNumber:    iss.Number,
		PlannerAgent:   "mock",
		ReviewerAgent:  "mock",
		DeveloperAgent: "mock",
		TesterAgent:    "mock",
		Phase:          "all",
		Gate:           contract.GateAgent,
		Headless:       true,
	})
	if err != nil {
		t.Fatalf("expected GateAgent full workflow to succeed, got: %v", err)
	}
	if !res.ValidationPassed || res.ReviewVerdict != "APPROVED" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(res.RoleAgentsUsed) < 4 {
		t.Fatalf("expected all 4 roles (planner, reviewer, developer, tester) to be recorded, got: %v", res.RoleAgentsUsed)
	}
}

func TestCollaborativeHandoffAndDecomposition(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatal(err)
	}

	lt := tracker.NewLocalTracker(tmp)
	eng, err := NewEngine(tmp, lt)
	if err != nil {
		t.Fatal(err)
	}

	// Create parent epic issue with 2 acceptance items and decompose it sequentially
	parentWU := &contract.WorkUnit{
		Objective:   "Build authentication & session subsystem",
		Constraints: []string{"Stdlib only"},
		Acceptance: []string{
			"Token parser validates signatures",
			"Session middleware attaches principal",
		},
		Workflow: "feature",
		Gate:     contract.GateNone,
	}
	parentIss, err := lt.CreateIssue(ctx, "Auth Subsystem Epic", contract.FormatIssueMarkdown(parentWU), []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}

	children, err := eng.DecomposeIssue(ctx, tmp, parentIss.Number, nil, true)
	if err != nil || len(children) != 2 {
		t.Fatalf("expected 2 decomposed child issues, got %d (%v)", len(children), err)
	}

	// Child #2 depends on Child #1 -> attempting to execute Child #2 before Child #1 is completed must fail with ErrDependenciesUnmet!
	_, err = eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:    tmp,
		IssueNumber: children[1].Number,
		Owner:       "alice",
		AgentName:   "mock",
		Headless:    true,
	})
	if !errors.Is(err, lease.ErrDependenciesUnmet) {
		t.Fatalf("expected ErrDependenciesUnmet when running child #2 before child #1, got: %v", err)
	}

	// Collaborative Handoff on Child #1:
	// Contributor "alice" runs --phase plan --handoff
	resPlan, err := eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:    tmp,
		IssueNumber: children[0].Number,
		Owner:       "alice",
		AgentName:   "mock",
		Phase:       "plan",
		Handoff:     true,
		Headless:    true,
	})
	if err != nil {
		t.Fatalf("alice plan handoff failed: %v", err)
	}
	if resPlan.HandedOffToPhase != "implement" || resPlan.State.Lease.Status != "handoff" {
		t.Fatalf("expected handoff to implement, got: %+v", resPlan.State.Lease)
	}

	// Contributor "bob" claims Child #1 in handoff state and runs --phase implement!
	resImpl, err := eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:    tmp,
		IssueNumber: children[0].Number,
		Owner:       "bob",
		AgentName:   "mock",
		Phase:       "implement",
		Headless:    true,
	})
	if err != nil {
		t.Fatalf("bob implement after handoff failed: %v", err)
	}
	if len(resImpl.State.Lease.Contributors) < 2 {
		t.Fatalf("expected both alice and bob in contributors list, got: %v", resImpl.State.Lease.Contributors)
	}
}
