package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func TestReviewerReadOnlyWorktreeGuardAndReviewPhaseHandoff(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}
	const originalREADME = "SwarmOS test repository\n"
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte(originalREADME), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", tmp, "add", "README.md").CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v (%s)", err, string(out))
	}
	if out, err := exec.Command("git", "-C", tmp, "-c", "user.name=SwarmOS", "-c", "user.email=swarm@example.test", "commit", "-m", "initial README").CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v (%s)", err, string(out))
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatal(err)
	}

	lt := tracker.NewLocalTracker(tmp)
	eng, err := NewEngine(tmp, lt)
	if err != nil {
		t.Fatal(err)
	}
	wu := &contract.WorkUnit{
		Objective:   "Review a planned change",
		Constraints: []string{"Do not modify production files during review"},
		Acceptance:  []string{"Plan receives an approved review"},
		Workflow:    "feature",
		Gate:        contract.GateAgent,
	}
	iss, err := lt.CreateIssue(ctx, "Review handoff", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}
	wu.IssueNumber = iss.Number

	t.Setenv("SWARM_MOCK_VERDICT", "APPROVED")
	planResult, err := eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:     tmp,
		IssueNumber:  iss.Number,
		Owner:        "planner",
		PlannerAgent: "mock",
		Phase:        "plan",
		Gate:         contract.GateAgent,
		Handoff:      true,
		Headless:     true,
	})
	if err != nil {
		t.Fatalf("plan handoff failed: %v", err)
	}
	if planResult.HandedOffToPhase != "review" {
		t.Fatalf("expected plan handoff to review, got %q", planResult.HandedOffToPhase)
	}

	reviewResult, err := eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:      tmp,
		IssueNumber:   iss.Number,
		Owner:         "grok-bot",
		ReviewerAgent: "mock",
		Phase:         "review",
		Gate:          contract.GateAgent,
		Handoff:       true,
		Headless:      true,
	})
	if err != nil {
		t.Fatalf("review handoff failed: %v", err)
	}
	if reviewResult.ReviewVerdict != "APPROVED" || reviewResult.HandedOffToPhase != "implement" {
		t.Fatalf("expected approved review handoff to implement, got %+v", reviewResult)
	}
	if !hasApprovedReviewArtifact(reviewResult.State) {
		t.Fatalf("review handoff did not publish an APPROVED review artifact: %+v", reviewResult.State.Artifacts)
	}

	rogueWU := &contract.WorkUnit{
		Objective:   "Guard reviewer write access",
		Constraints: []string{"Reviewer writes only .swarm/active/REVIEW.md"},
		Acceptance:  []string{"Unauthorized reviewer edits are reverted"},
		Workflow:    "feature",
		Gate:        contract.GateNone,
	}
	rogueIssue, err := lt.CreateIssue(ctx, "Rogue reviewer", contract.FormatIssueMarkdown(rogueWU), []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}
	rogueWU.IssueNumber = rogueIssue.Number
	roguePlan, err := eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:     tmp,
		IssueNumber:  rogueIssue.Number,
		Owner:        "planner",
		PlannerAgent: "mock",
		Phase:        "plan",
		Headless:     true,
	})
	if err != nil {
		t.Fatalf("rogue-review plan failed: %v", err)
	}

	scriptPath := filepath.Join(t.TempDir(), "rogue-reviewer.sh")
	script := "#!/bin/sh\nmkdir -p .swarm/active\nprintf 'VERDICT: APPROVED\\n' > .swarm/active/REVIEW.md\nprintf 'reviewer modification\\n' > README.md\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWARM_CUSTOM_AGENT_BIN", scriptPath)
	wf, err := harness.LoadWorkflow(tmp, rogueWU.Workflow)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = eng.RunAgentReview(ctx, roguePlan.WorktreePath, rogueWU, wf, roguePlan.State, "planner", "custom", "headless")
	if !errors.Is(err, ErrReviewerModifiedCode) {
		t.Fatalf("expected ErrReviewerModifiedCode, got %v", err)
	}
	readme, err := os.ReadFile(filepath.Join(roguePlan.WorktreePath, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(readme) != originalREADME {
		t.Fatalf("reviewer edit was not reverted: got %q, want %q", readme, originalREADME)
	}
}
