package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	configureValidation(t, tmp)
	if out, err := exec.Command("git", "-C", tmp, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add harness failed: %v (%s)", err, string(out))
	}
	if out, err := exec.Command("git", "-C", tmp, "-c", "user.name=SwarmOS", "-c", "user.email=swarm@example.test", "commit", "-m", "initialize harness").CombinedOutput(); err != nil {
		t.Fatalf("git commit harness failed: %v (%s)", err, string(out))
	}

	lt := tracker.NewLocalTracker(tmp).WithUser("local-dev", "OWNER")
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
	if _, err := eng.LeaseManager.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}

	// 1. Test GateAgent rejection when SWARM_MOCK_VERDICT=CHANGES_REQUESTED
	t.Setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	var failedEvents []ProgressEvent
	failed, err := eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:       tmp,
		IssueNumber:    iss.Number,
		PlannerAgent:   "mock",
		ReviewerAgent:  "mock",
		DeveloperAgent: "mock",
		TesterAgent:    "mock",
		Phase:          "all",
		Gate:           contract.GateAgent,
		Headless:       true,
		OnProgress:     func(e ProgressEvent) { failedEvents = append(failedEvents, e) },
	})
	if failed == nil || failed.State == nil {
		t.Fatalf("rejected execution did not retain a fenced state")
	}
	if _, releaseErr := eng.LeaseManager.Release(ctx, iss.Number, "local-dev", "retry rejected plan"); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if !errors.Is(err, ErrPlanReviewRejected) {
		t.Fatalf("expected ErrPlanReviewRejected, got: %v", err)
	}
	if last := failedEvents[len(failedEvents)-1]; last.Stage != "review" || last.Status != "failed" {
		t.Fatalf("rejection progress: %+v", failedEvents)
	}
	for _, event := range failedEvents {
		if event.Stage == "implement" {
			t.Fatal("rejected review announced implementation")
		}
	}

	// A default rerun must plan again, but the deterministic mock planner
	// returns the rejected documents unchanged and must not reach review.
	unchanged, unchangedErr := eng.ExecuteWork(ctx, WorkOptions{RepoRoot: tmp, IssueNumber: iss.Number, AgentName: "mock", Phase: "all", Headless: true})
	if unchanged.RoleAgentsUsed["planner"] != "mock" || unchanged.RoleAgentsUsed["reviewer"] != "" {
		t.Fatalf("unchanged plan was reviewed: %+v", unchanged.RoleAgentsUsed)
	}
	if unchangedErr == nil || !strings.Contains(unchangedErr.Error(), "re-plan is unchanged") {
		t.Fatalf("unchanged replan accepted: %v", unchangedErr)
	}
	// Explicit review remains available to an operator who wants to re-review
	// the current plan rather than ask for a new default planning attempt.
	t.Setenv("SWARM_MOCK_VERDICT", "APPROVED")
	reviewed, reviewErr := eng.ExecuteWork(ctx, WorkOptions{RepoRoot: tmp, IssueNumber: iss.Number, ClaimID: unchanged.State.Lease.ClaimID, AgentName: "mock", Phase: "review", Headless: true})
	if reviewErr != nil {
		t.Fatal(reviewErr)
	}
	// 2. Test GateAgent approval and full multi-role execution
	t.Setenv("SWARM_MOCK_VERDICT", "APPROVED")
	var completedEvents []ProgressEvent
	res, err := eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot:       tmp,
		ClaimID:        reviewed.State.Lease.ClaimID,
		IssueNumber:    iss.Number,
		PlannerAgent:   "mock",
		ReviewerAgent:  "mock",
		DeveloperAgent: "mock",
		TesterAgent:    "mock",
		Phase:          "all",
		Gate:           contract.GateAgent,
		Headless:       true,
		OnProgress:     func(e ProgressEvent) { completedEvents = append(completedEvents, e) },
	})
	if err != nil {
		t.Fatalf("expected GateAgent full workflow to succeed, got: %v", err)
	}
	if !res.ValidationPassed || res.ReviewVerdict != "APPROVED" {
		t.Fatalf("unexpected result: %+v", res)
	}
	var started []string
	for _, event := range completedEvents {
		if event.Status == "started" {
			started = append(started, event.Stage)
		}
	}
	if !slices.Equal(started, []string{"preflight", "claim", "workspace", "implement", "checkpoint", "test", "required-checks"}) {
		t.Fatalf("resumed stage sequence: %v", started)
	}
	if last := completedEvents[len(completedEvents)-1]; last.Stage != "required-checks" || last.Status != "completed" {
		t.Fatalf("completion progress: %+v", last)
	}

	// Resuming an approved plan must preserve the upstream review-skip behavior,
	// including its progress: neither planning nor review should be announced.
	var resumedEvents []ProgressEvent
	resumed, err := eng.ExecuteWork(ctx, WorkOptions{
		RepoRoot: tmp, IssueNumber: iss.Number, ClaimID: res.State.Lease.ClaimID,
		AgentName: "mock", Phase: "all", Headless: true,
		OnProgress: func(e ProgressEvent) { resumedEvents = append(resumedEvents, e) },
	})
	if err != nil {
		t.Fatalf("resume approved plan: %v", err)
	}
	if resumed.ReviewVerdict != "APPROVED" || !resumed.ValidationPassed {
		t.Fatalf("approved resume result: %+v", resumed)
	}
	for _, event := range resumedEvents {
		if event.Stage == "plan" || event.Stage == "review" {
			t.Fatalf("resumed execution announced a skipped stage: %+v", event)
		}
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
	configureValidation(t, tmp)
	if out, err := exec.Command("git", "-C", tmp, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add harness failed: %v (%s)", err, string(out))
	}
	if out, err := exec.Command("git", "-C", tmp, "-c", "user.name=SwarmOS", "-c", "user.email=swarm@example.test", "commit", "-m", "initialize harness").CombinedOutput(); err != nil {
		t.Fatalf("git commit harness failed: %v (%s)", err, string(out))
	}

	lt := tracker.NewLocalTracker(tmp).WithUser("local-dev", "OWNER")
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
	for _, child := range children {
		if !slices.Contains(child.Labels, "swarm:draft") || slices.Contains(child.Labels, "swarm:ready") {
			t.Fatalf("expected decomposed child #%d to start as swarm:draft, got %v", child.Number, child.Labels)
		}
		if parentNum, err := lt.ParentIssueNumber(ctx, child.Number); err != nil || parentNum != parentIss.Number {
			t.Fatalf("expected decomposed child #%d native parent #%d, got (%d, %v)", child.Number, parentIss.Number, parentNum, err)
		}
		if _, err := eng.LeaseManager.ApproveTask(ctx, child.Number, "local-dev"); err != nil {
			t.Fatal(err)
		}
	}

	epicIss, err := lt.CreateTypedIssue(ctx, "Standalone Epic", "# Epic Body", []string{"swarm:ready"}, "Epic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.DecomposeIssue(ctx, tmp, epicIss.Number, nil, true); err == nil {
		t.Fatal("expected DecomposeIssue to reject Epic issue")
	}
	if _, err := eng.Preflight(ctx, WorkOptions{RepoRoot: tmp, IssueNumber: epicIss.Number, Phase: "plan"}); err == nil {
		t.Fatal("expected Preflight to reject Epic issue")
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
	engAlice, err := NewEngine(tmp, lt.WithUser("alice", "OWNER"))
	if err != nil {
		t.Fatal(err)
	}
	resPlan, err := engAlice.ExecuteWork(ctx, WorkOptions{
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
	if resPlan.HandedOffToPhase != "review" || resPlan.State.Lease.Status != "handoff" {
		t.Fatalf("repository agent gate was bypassed by task gate none: %+v", resPlan.State.Lease)
	}

	// A second authenticated contributor must review before implementation.
	engBob, err := NewEngine(tmp, lt.WithUser("bob", "OWNER"))
	if err != nil {
		t.Fatal(err)
	}
	review, err := engBob.ExecuteWork(ctx, WorkOptions{
		RepoRoot: tmp, IssueNumber: children[0].Number, AgentName: "mock",
		Phase: "review", Headless: true,
	})
	if err != nil {
		t.Fatalf("bob review after handoff: %v", err)
	}
	resImpl, err := engBob.ExecuteWork(ctx, WorkOptions{
		RepoRoot:    tmp,
		IssueNumber: children[0].Number,
		Owner:       "bob",
		ClaimID:     review.State.Lease.ClaimID,
		AgentName:   "mock",
		Phase:       "implement",
		Headless:    true,
	})
	if err != nil {
		t.Fatalf("bob implement after handoff failed: %v", err)
	}
	if !resImpl.ValidationPassed {
		t.Fatal("reviewed handoff did not reach validated implementation")
	}
}

func TestReviewPhaseHandoff(t *testing.T) {
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
	configureValidation(t, tmp)
	if out, err := exec.Command("git", "-C", tmp, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add harness failed: %v (%s)", err, string(out))
	}
	if out, err := exec.Command("git", "-C", tmp, "-c", "user.name=SwarmOS", "-c", "user.email=swarm@example.test", "commit", "-m", "initialize harness").CombinedOutput(); err != nil {
		t.Fatalf("git commit harness failed: %v (%s)", err, string(out))
	}

	lt := tracker.NewLocalTracker(tmp).WithUser("local-dev", "OWNER")
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
	if _, err := eng.LeaseManager.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	wu.IssueNumber = iss.Number

	t.Setenv("SWARM_MOCK_VERDICT", "APPROVED")
	engPlanner, err := NewEngine(tmp, lt.WithUser("planner", "OWNER"))
	if err != nil {
		t.Fatal(err)
	}
	planResult, err := engPlanner.ExecuteWork(ctx, WorkOptions{
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

	engReviewer, err := NewEngine(tmp, lt.WithUser("grok-bot", "OWNER"))
	if err != nil {
		t.Fatal(err)
	}
	reviewResult, err := engReviewer.ExecuteWork(ctx, WorkOptions{
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

}

func TestParseReviewVerdictFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		ok      bool
	}{
		{name: "approved", content: "VERDICT: APPROVED\n\nPlan is bounded.", want: "APPROVED", ok: true},
		{name: "changes requested", content: "VERDICT: CHANGES_REQUESTED\n\nAdd coverage.", want: "CHANGES_REQUESTED", ok: true},
		{name: "missing", content: "I could not complete this review.", ok: false},
		{name: "substring", content: "VERDICT: APPROVED with caveats", ok: false},
		{name: "contradictory", content: "VERDICT: APPROVED\nVERDICT: CHANGES_REQUESTED", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseReviewVerdict(tc.content)
			if tc.ok {
				if err != nil || got != tc.want {
					t.Fatalf("ParseReviewVerdict() = %q, %v; want %q, nil", got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseReviewVerdict(%q) unexpectedly accepted %q", tc.content, got)
			}
		})
	}
}

func configureValidation(t *testing.T, root string) {
	t.Helper()
	// Mock lifecycle tests explicitly exercise host compatibility mode.
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	for _, args := range [][]string{
		{"-C", root, "config", "user.name", "SwarmOS"},
		{"-C", root, "config", "user.email", "swarm@example.test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	path := filepath.Join(root, ".swarm", "validation", "required-checks.yaml")
	content := "required_checks:\n  - name: repository status\n    command: git status --porcelain\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n  - .swarm/contracts/**\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEngineSetupProgressSharedSessionBundles(t *testing.T) {
	root, engine, tr := setupProjectPlanTestRepo(t)
	configureValidation(t, root)
	if out, err := exec.Command("git", "-C", root, "commit", "-am", "validation policy").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	wu := &contract.WorkUnit{Objective: "Setup status", Constraints: []string{"Keep reporting private"}, Acceptance: []string{"Progress precedes execution"}, Workflow: "feature", Gate: contract.GateAgent}
	issue, err := tr.CreateIssue(t.Context(), "private-title\nsecret-token", contract.FormatIssueMarkdown(wu), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.LeaseManager.ApproveTask(t.Context(), issue.Number, "maintainer"); err != nil {
		t.Fatal(err)
	}
	// The mock's real child command checks evidence recorded synchronously by
	// the observer, so final-output ordering alone cannot satisfy this test.
	marker := filepath.Join(t.TempDir(), "setup-delivered")
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "test -f '"+marker+"' && printf 'changed\\n' > implementation.txt && printf 'private-child-output secret-token\\n'")
	var events []ProgressEvent
	reporter := NewProgressReporter(t.Context(), func(event ProgressEvent) {
		events = append(events, event)
		if event.Stage == "bundle" && event.Status == "started" {
			var starts []string
			for _, e := range events {
				if e.Status == "started" && (e.Stage == "claim" || e.Stage == "workspace" || e.Stage == "bundle") {
					starts = append(starts, e.Stage)
				}
			}
			if len(starts) >= 3 && slices.Equal(starts[:3], []string{"claim", "workspace", "bundle"}) {
				if err := os.WriteFile(marker, []byte("delivered"), 0600); err != nil {
					t.Error(err)
				}
			}
		}
	})
	defer reporter.Close()
	result, err := engine.ExecuteWork(t.Context(), WorkOptions{
		RepoRoot: root, IssueNumber: issue.Number, AgentName: "mock", Headless: true,
		SetupProgress: reporter, OnProgress: func(ProgressEvent) { t.Error("second callback invoked") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ValidationPassed {
		t.Fatal("validation did not run")
	}
	// ExecuteWork must leave the supplied session alive for its command owner.
	reporter.mu.Lock()
	closed := reporter.closed
	reporter.mu.Unlock()
	if closed {
		t.Fatal("engine closed command session")
	}
	reporter.Close()
	var starts []string
	for _, event := range events {
		if event.Status == "started" {
			starts = append(starts, event.Stage)
		}
	}
	want := []string{"preflight", "claim", "workspace", "plan", "bundle", "review", "bundle", "implement", "bundle", "checkpoint", "test", "bundle", "required-checks"}
	if !slices.Equal(starts, want) {
		t.Fatalf("starts: %v", starts)
	}
}
