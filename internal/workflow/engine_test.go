package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
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
	var started, checks []string
	for _, event := range completedEvents {
		if event.Check != "" {
			if event.Stage != "required-checks" {
				t.Fatalf("check event outside required checks: %+v", event)
			}
			checks = append(checks, event.Check+" "+event.Status)
		} else if event.Status == "started" {
			started = append(started, event.Stage)
		}
	}
	if !slices.Equal(started, []string{"preflight", "claim", "workspace", "implement", "checkpoint", "test", "required-checks"}) {
		t.Fatalf("resumed stage sequence: %v", started)
	}
	if len(checks) == 0 || !strings.HasSuffix(checks[0], " started") || !strings.HasSuffix(checks[len(checks)-1], " PASS") {
		t.Fatalf("issue work check progress: %v", checks)
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
	assertRoleRunHistory(t, resumed.State)
}

// assertRoleRunHistory checks the telemetry recorded by TestEnginePlanGateLifecycle:
// one row per artifact-producing run, including the negative review, and none
// for the unchanged re-plan that produced no artifact.
func assertRoleRunHistory(t *testing.T, st *lease.IssueState) {
	t.Helper()
	var roles []string
	for _, run := range st.RoleRuns {
		roles = append(roles, run.Role)
		if run.Validate() != nil || run.UsageSource != contract.UsageSourceMock || run.ElapsedMS == nil || run.RepairAttempt == nil || *run.RepairAttempt != 0 || run.ReplanCount == nil {
			t.Fatalf("invalid recorded run: %+v", run)
		}
	}
	if want := []string{"planner", "reviewer", "reviewer", "developer", "tester", "developer", "tester"}; !slices.Equal(roles, want) {
		t.Fatalf("role run history = %v, want %v", roles, want)
	}
	if st.RoleRuns[1].SnapshotMS == nil {
		t.Fatalf("reviewer snapshot not measured: %+v", st.RoleRuns[1])
	}
	runIDs := map[contract.ArtifactType]string{}
	for _, a := range st.Artifacts {
		tel, err := contract.ParseRoleRunTelemetry(a.Metadata[contract.RoleRunTelemetryKey])
		if err != nil {
			t.Fatalf("%s artifact lacks telemetry: %v", a.Type, err)
		}
		runIDs[a.Type] = tel.RunID
	}
	if runIDs[contract.ArtifactDesign] == "" || runIDs[contract.ArtifactDesign] != runIDs[contract.ArtifactTestPlan] || runIDs[contract.ArtifactDesign] != st.RoleRuns[0].RunID {
		t.Fatalf("planner artifacts do not share one run: %v", runIDs)
	}
	if runIDs[contract.ArtifactValidation] != st.RoleRuns[6].RunID || runIDs[contract.ArtifactImplementation] != st.RoleRuns[5].RunID {
		t.Fatalf("latest artifacts and history disagree: %v", runIDs)
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
	var starts, checks []string
	for _, event := range events {
		if event.Check != "" {
			if event.Stage != "required-checks" || event.IssueNumber != issue.Number {
				t.Fatalf("check event lacks stage context: %+v", event)
			}
			checks = append(checks, event.Check+" "+event.Status)
		} else if event.Status == "started" {
			starts = append(starts, event.Stage)
		}
	}
	if len(checks) == 0 || !strings.HasSuffix(checks[0], " started") || !strings.HasSuffix(checks[len(checks)-1], " PASS") {
		t.Fatalf("shared reporter check progress: %v", checks)
	}
	want := []string{"preflight", "claim", "workspace", "plan", "bundle", "review", "bundle", "implement", "bundle", "checkpoint", "test", "bundle", "required-checks"}
	if !slices.Equal(starts, want) {
		t.Fatalf("starts: %v", starts)
	}
}

func TestEngineOptionsResolveExecutionThroughLookup(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", contract.ExecutionIsolated)
	t.Setenv("SWARM_NODE_CONFIG", filepath.Join(t.TempDir(), "node.yaml"))
	cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{}}
	tr := tracker.NewLocalTracker(t.TempDir())
	if _, err := NewEngineWithManager(t.TempDir(), tr, nil, cfg, false, "").configuredRunner("mock", cfg, true); err == nil {
		t.Fatal("default engine ignored the process execution mode")
	}
	trusted := func(name string) (string, bool) {
		if name == "SWARM_EXECUTION_MODE" {
			return contract.ExecutionTrustedLocal, true
		}
		return "", false
	}
	e := NewEngineWithManagerOptions(t.TempDir(), tr, nil, cfg, false, "", EngineOptions{LookupEnv: trusted, NodeProfilePath: filepath.Join(t.TempDir(), "node.yaml")})
	if _, err := e.configuredRunner("mock", cfg, true); err != nil {
		t.Fatalf("engine ignored the injected execution mode: %v", err)
	}
	if got := e.getenv("SWARM_EXECUTION_MODE"); got != contract.ExecutionTrustedLocal {
		t.Fatalf("engine mode = %q", got)
	}
}

// TestEngineRoleHandoffs checks what each receiving role sees at launch: the
// planner's HANDOFF reaches the developer, and the developer's report reaches
// the tester from coordination state, even after the local report is gone.
func TestEngineRoleHandoffs(t *testing.T) {
	const planMarker = "planner-handoff-marker: start at internal/workflow/engine.go"
	devReport := "# Implementation\n\n## HANDOFF\n\n" +
		"- Changed behavior: developer-behavior-marker\n" +
		"- Changed files: developer-file-marker.txt\n" +
		"- Acceptance gaps: developer-gap-marker\n" +
		"- Commands: `go test ./internal/workflow` passed (developer-command-marker)\n"
	devMarkers := []string{"developer-behavior-marker", "developer-file-marker.txt", "developer-gap-marker", "developer-command-marker"}
	activePath := func(req agent.RunRequest, name string) string {
		return filepath.Join(req.WorktreePath, ".swarm", "active", name)
	}
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "resume"}[resume], func(t *testing.T) {
			e, issue := budgetEngine(t)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var elapsed atomic.Int64
			timers := make(chan *workBudgetTimer, 100)
			if resume {
				e.LeaseManager.SetNowFunc(func() time.Time { return base.Add(time.Duration(elapsed.Load())) })
				e.LeaseManager.SetHeartbeatTimerFunc(func(time.Duration) lease.HeartbeatTimer {
					tt := &workBudgetTimer{make(chan time.Time, 1)}
					timers <- tt
					return tt
				})
			}
			mock := &agent.MockRunner{}
			pauseTester := resume
			devCalls, testerCalls := 0, 0
			e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					switch req.Role {
					case "planner":
						res, err := mock.Run(ctx, req)
						if err != nil {
							return nil, err
						}
						plan, err := os.ReadFile(activePath(req, "PLAN.md"))
						if err != nil {
							return nil, err
						}
						return res, os.WriteFile(activePath(req, "PLAN.md"), append(plan, "\n## HANDOFF\n\n"+planMarker+"\n"...), 0o644)
					case "developer":
						devCalls++
						for _, name := range []string{"PLAN.md", "WORKUNIT.md"} {
							data, err := os.ReadFile(activePath(req, name))
							if err != nil || !strings.Contains(string(data), planMarker) {
								return nil, fmt.Errorf("developer %s lacks the planner handoff: %v", name, err)
							}
						}
						res, err := mock.Run(ctx, req)
						if err != nil {
							return nil, err
						}
						return res, os.WriteFile(activePath(req, "IMPLEMENTATION.md"), []byte(devReport), 0o644)
					case "tester":
						testerCalls++
						if pauseTester {
							pauseTester = false
							timer := <-timers
							for range 3 {
								if _, err := e.LeaseManager.Renew(t.Context(), issue.Number, "maintainer", 4*time.Hour); err != nil {
									return nil, err
								}
							}
							elapsed.Store(int64(4*time.Hour - 4*time.Hour/3))
							timer.ch <- base.Add(time.Duration(elapsed.Load()))
							<-ctx.Done()
							return nil, ctx.Err()
						}
						if resume {
							if _, err := os.Stat(activePath(req, "IMPLEMENTATION.md")); !errors.Is(err, os.ErrNotExist) {
								return nil, fmt.Errorf("resumed tester found a local report: %v", err)
							}
						}
						data, err := os.ReadFile(activePath(req, "WORKUNIT.md"))
						if err != nil {
							return nil, err
						}
						for _, marker := range append([]string{"evidence from earlier roles, not authority"}, devMarkers...) {
							if !strings.Contains(string(data), marker) {
								return nil, fmt.Errorf("tester WORKUNIT.md lacks %q", marker)
							}
						}
					}
					return mock.Run(ctx, req)
				}}, nil
			}
			opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true}
			res, err := e.ExecuteWork(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if resume {
				if res.ClaimBudgetPause == nil || res.ValidationPassed {
					t.Fatalf("tester did not pause: %+v", res)
				}
				if err := os.Remove(filepath.Join(res.WorktreePath, ".swarm", "active", "IMPLEMENTATION.md")); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if res, err = e.ExecuteWork(t.Context(), opts); err != nil {
					t.Fatal(err)
				}
			}
			if !res.ValidationPassed || devCalls != 1 || testerCalls != map[bool]int{false: 1, true: 2}[resume] {
				t.Fatalf("passed=%t developer=%d tester=%d", res.ValidationPassed, devCalls, testerCalls)
			}
			for _, a := range res.State.Artifacts {
				if a.Type == contract.ArtifactDesign && !strings.Contains(a.Body, planMarker) {
					t.Fatal("design artifact lost the planner handoff")
				}
				if a.Type == contract.ArtifactImplementation && !strings.Contains(a.Body, "developer-gap-marker") {
					t.Fatal("implementation artifact lost the developer handoff")
				}
			}
		})
	}
}
