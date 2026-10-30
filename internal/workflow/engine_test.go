package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestEnginePlanGateLifecycle(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	if out, err := gitfixture.Command(t, "", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatal(err)
	}
	configureValidation(t, tmp)
	if out, err := gitfixture.Command(t, "", "-C", tmp, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add harness failed: %v (%s)", err, string(out))
	}
	if out, err := gitfixture.Command(t, "", "-C", tmp, "-c", "user.name=SwarmOS", "-c", "user.email=swarm@example.test", "commit", "-m", "initialize harness").CombinedOutput(); err != nil {
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
	if out, err := gitfixture.Command(t, "", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatal(err)
	}
	configureValidation(t, tmp)
	if out, err := gitfixture.Command(t, "", "-C", tmp, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add harness failed: %v (%s)", err, string(out))
	}
	if out, err := gitfixture.Command(t, "", "-C", tmp, "-c", "user.name=SwarmOS", "-c", "user.email=swarm@example.test", "commit", "-m", "initialize harness").CombinedOutput(); err != nil {
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
	if out, err := gitfixture.Command(t, "", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}
	const originalREADME = "SwarmOS test repository\n"
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte(originalREADME), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := gitfixture.Command(t, "", "-C", tmp, "add", "README.md").CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v (%s)", err, string(out))
	}
	if out, err := gitfixture.Command(t, "", "-C", tmp, "-c", "user.name=SwarmOS", "-c", "user.email=swarm@example.test", "commit", "-m", "initial README").CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v (%s)", err, string(out))
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatal(err)
	}
	configureValidation(t, tmp)
	if out, err := gitfixture.Command(t, "", "-C", tmp, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add harness failed: %v (%s)", err, string(out))
	}
	if out, err := gitfixture.Command(t, "", "-C", tmp, "-c", "user.name=SwarmOS", "-c", "user.email=swarm@example.test", "commit", "-m", "initialize harness").CombinedOutput(); err != nil {
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
	t.Parallel()
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
			t.Parallel()
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
		if out, err := gitfixture.Command(t, "", args...).CombinedOutput(); err != nil {
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
	if out, err := gitfixture.Command(t, "", "-C", root, "commit", "-am", "validation policy").CombinedOutput(); err != nil {
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
	if _, err := NewEngineWithManager(t.TempDir(), tr, nil, cfg, false, "").configuredRunner(context.Background(), "mock", cfg, true); err == nil {
		t.Fatal("default engine ignored the process execution mode")
	}
	trusted := func(name string) (string, bool) {
		if name == "SWARM_EXECUTION_MODE" {
			return contract.ExecutionTrustedLocal, true
		}
		return "", false
	}
	e := NewEngineWithManagerOptions(t.TempDir(), tr, nil, cfg, false, "", EngineOptions{LookupEnv: trusted, NodeProfilePath: filepath.Join(t.TempDir(), "node.yaml")})
	if _, err := e.configuredRunner(context.Background(), "mock", cfg, true); err != nil {
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

type providerFixtureError struct{ class string }

func (e *providerFixtureError) Error() string        { return "safe failure" }
func (e *providerFixtureError) FailureClass() string { return e.class }

func TestProviderFailureResume(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"planner", "reviewer", "developer", "tester"} {
		phase := "implement"
		if role == "planner" {
			phase = "plan"
		}
		if role == "reviewer" {
			phase = "review"
		}
		for _, pr := range []int{0, 42} {
			opts := WorkOptions{IssueNumber: 249, PRNumber: pr, DeveloperAgent: "other'agent"}
			command := failureResume(opts, role)
			prefix := "swarm issue work 249"
			if pr != 0 {
				prefix = "swarm pr work 42"
			}
			if !strings.HasPrefix(command, prefix+" --phase "+phase) {
				t.Fatal(command)
			}
			script := "set -- " + command + "; test \"$7\" = \"--developer-agent\" && test \"$8\" = \"other'agent\""
			if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
				t.Fatalf("quote: %s %v %s", command, err, out)
			}
			original := &providerFixtureError{"capacity"}
			_, err := (&Engine{}).failedRun(context.Background(), nil, opts, nil, original, role)
			if !errors.Is(err, original) || !strings.Contains(err.Error(), "provider model at capacity; resume: "+command) {
				t.Fatal(err)
			}
		}
	}
	original := &providerFixtureError{"unknown"}
	_, err := (&Engine{}).failedRun(context.Background(), nil, WorkOptions{}, nil, original, "developer")
	if err != original {
		t.Fatal("unknown message changed")
	}
}

type upstreamWorkflowTracker struct{ tracker.Tracker }

func (t upstreamWorkflowTracker) BackendName() string                     { return "github (octo/swarm)" }
func (t upstreamWorkflowTracker) UpstreamRepo() string                    { return "octo/swarm" }
func (t upstreamWorkflowTracker) Authorize(context.Context, string) error { return nil }
func (t upstreamWorkflowTracker) CurrentUser(context.Context) (string, error) {
	return "maintainer", nil
}
func (t upstreamWorkflowTracker) VerifyPlanReviewFork(_ context.Context, slug, owner string) (string, error) {
	if slug != "octo/personal" || owner != "octo" {
		return "", errors.New("invalid fork")
	}
	return slug, nil
}

type upstreamWorkflowRunner struct {
	att contract.RoleAttestation
	run func(context.Context, agent.RunRequest) (*agent.RunResult, error)
}

func (r upstreamWorkflowRunner) Name() string              { return r.att.Agent }
func (r upstreamWorkflowRunner) Available() (string, bool) { return "fixture", true }
func (r upstreamWorkflowRunner) BuildCommand(agent.RunRequest) (string, []string) {
	return "fixture", nil
}
func (r upstreamWorkflowRunner) Provenance(string, bool) contract.RoleAttestation { return r.att }
func (r upstreamWorkflowRunner) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	return r.run(ctx, req)
}
func TestUpstreamPlanReviewInitialCompletion(t *testing.T) {
	for _, boundary := range []string{"codex-native", "harness-tools-bwrap"} {
		for _, outcome := range []string{"approved", "rejected", "runner failure", "canceled", "invalid report", "invalid provenance"} {
			t.Run(boundary+"/"+outcome, func(t *testing.T) {
				ctx := context.Background()
				root := t.TempDir()
				git := func(args ...string) string {
					t.Helper()
					out, err := gitfixture.Command(t, root, args...).CombinedOutput()
					if err != nil {
						t.Fatalf("git %v: %v: %s", args, err, out)
					}
					return strings.TrimSpace(string(out))
				}
				git("init", "-b", "main")
				if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
					t.Fatal(err)
				}
				git("add", ".")
				git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "approved base")
				lt := tracker.NewLocalTracker(root).WithUser("contributor", "OWNER")
				wu := &contract.WorkUnit{Objective: "Review current plan", Constraints: []string{"Keep lease"}, Acceptance: []string{"Exact plan"}, Workflow: "feature", Gate: contract.GateAgent}
				issue, err := lt.CreateIssue(ctx, "Review current plan", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := harness.LoadConfig(root)
				if err != nil {
					t.Fatal(err)
				}
				worker := lease.NewManager(lt, cfg.Security)
				worker.SetStore(root, coordination.NewFileStore(root))
				if _, err := worker.ApproveTask(ctx, issue.Number, "contributor"); err != nil {
					t.Fatal(err)
				}
				if _, err := worker.Acquire(ctx, issue.Number, "contributor", "claude", "plan", time.Hour, 3); err != nil {
					t.Fatal(err)
				}
				for _, kind := range []contract.ArtifactType{contract.ArtifactDesign, contract.ArtifactTestPlan} {
					if _, err := worker.UpdateArtifact(ctx, issue.Number, "contributor", contract.Artifact{Type: kind, Producer: "claude:planner", Summary: "fixture plan", Body: "exact " + string(kind) + "\n"}); err != nil {
						t.Fatal(err)
					}
				}
				store := worker.Store()
				raw, rev, err := store.Load(ctx, issue.Number)
				if err != nil {
					t.Fatal(err)
				}
				var st lease.IssueState
				if err := json.Unmarshal(raw, &st); err != nil {
					t.Fatal(err)
				}
				st.ForkRepo = "octo/personal"
				st.HeadSHA, st.Branch = "", ""
				st.CheckpointHeads = nil
				raw, _ = json.Marshal(st)
				if _, err := store.CompareAndSwap(ctx, issue.Number, rev, raw); err != nil {
					t.Fatal(err)
				}
				// An unrelated upstream checkout head must never become the initial source.
				if err := os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("unrelated checkout"), 0644); err != nil {
					t.Fatal(err)
				}
				git("add", "unrelated.txt")
				git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "unrelated upstream head")
				tr := upstreamWorkflowTracker{lt}
				manager := lease.NewManager(tr, cfg.Security)
				manager.SetStore(root, store)
				if _, err := manager.Acquire(ctx, issue.Number, "maintainer", "codex", "review", time.Hour, 3); !errors.Is(err, lease.ErrAlreadyClaimed) {
					t.Fatalf("separate checkout acquisition: %v", err)
				}
				o, err := manager.PrepareUpstreamPlanReview(ctx, issue.Number)
				if err != nil {
					t.Fatal(err)
				}
				if o.Source().Commit != st.BaseSHA || o.Source().Kind != "FreshInitialApprovedBase" {
					t.Fatal(o.Source())
				}
				calls := 0
				att := contract.RoleAttestation{Role: "reviewer", Agent: "codex", Harness: "codex", HarnessVersion: "fixture", Model: "review-fixture", Sandbox: boundary}
				runner := upstreamWorkflowRunner{att: att}
				runner.run = func(runCtx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					calls++
					if !req.ReadOnly || req.WorktreePath == root || req.Role != "reviewer" {
						t.Fatalf("invalid isolated request: %+v", req)
					}
					if _, err := os.Stat(filepath.Join(req.WorktreePath, "unrelated.txt")); !os.IsNotExist(err) {
						t.Fatal("reviewed upstream checkout instead of approved base")
					}
					for name, body := range map[string]string{"PLAN.md": "exact design\n", "TEST_PLAN.md": "exact test-plan\n"} {
						data, err := os.ReadFile(filepath.Join(req.WorktreePath, ".swarm/active", name))
						if err != nil || string(data) != body {
							t.Fatalf("%s: %q %v", name, data, err)
						}
					}
					if outcome == "runner failure" {
						return nil, errors.New("fixture boundary failure")
					}
					if outcome == "canceled" {
						return nil, context.Canceled
					}
					body := "VERDICT: APPROVED\n"
					if outcome == "rejected" {
						body = "VERDICT: CHANGES_REQUESTED\n"
					}
					if outcome == "invalid report" {
						body = "APPROVED"
					}
					if err := os.WriteFile(filepath.Join(req.WorktreePath, ".swarm/active/REVIEW.md"), []byte(body), 0644); err != nil {
						return nil, err
					}
					resultAtt := att
					if outcome == "invalid provenance" {
						resultAtt.Sandbox = "host"
					}
					return &agent.RunResult{Agent: "codex", Role: "reviewer", Attestation: resultAtt}, nil
				}
				engine := NewEngineWithManagerOptions(root, tr, manager, cfg, false, "", EngineOptions{NewRunner: func(string, *contract.SwarmConfig, bool) (agent.Runner, error) { return runner, nil }})
				verdict, updated, runErr := engine.RunUpstreamPlanReview(ctx, o, "codex", true, nil)
				if outcome == "approved" {
					if runErr != nil || verdict != "APPROVED" {
						t.Fatalf("completion: %s %v", verdict, runErr)
					}
					parsed, _ := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
					if err := lease.VerifyApproval(updated, parsed); err != nil {
						t.Fatal(err)
					}
					branch := fmt.Sprintf("swarm/issue-%d-%s", issue.Number, updated.Lease.ClaimID)
					checkpoint, err := worker.RecordCheckpoint(ctx, issue.Number, "contributor", branch, git("rev-parse", "HEAD"))
					if err != nil {
						t.Fatal(err)
					}
					if err := lease.VerifyApproval(checkpoint, parsed); err != nil {
						t.Fatalf("worker checkpoint continuation: %v", err)
					}

				} else if runErr == nil {
					t.Fatal("unsafe outcome accepted")
				}
				if calls != 1 {
					t.Fatalf("runner calls=%d", calls)
				}
				after, err := worker.GetCurrentState(ctx, issue.Number)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(st.Lease, after.Lease) || !reflect.DeepEqual(st.ClaimBudget, after.ClaimBudget) {
					t.Fatal("review changed contributor lease/budget")
				}
				if outcome == "approved" {
					wu, _ := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
					// Exercise the worker's real resume and implementation admission,
					// without a planner/reviewer rerun or model invocation.
					workerEngine := NewEngineWithManagerOptions(root, lt, worker, cfg, false, "", EngineOptions{})
					run := &workRun{e: workerEngine, ctx: ctx, coordinatorCtx: ctx,
						opts: WorkOptions{IssueNumber: issue.Number, Owner: "contributor", ClaimID: after.Lease.ClaimID, Phase: "implement"},
						pre:  &PreflightResult{WorkUnit: wu}, cfg: cfg, wtPath: t.TempDir(), res: &WorkResult{}}
					run.progress = startWorkProgress(ctx, issue.Number, nil)
					defer run.progress.finish(nil, nil)
					if err := run.claimRun(); err != nil {
						t.Fatal(err)
					}
					run.heartbeat = worker.StartHeartbeat(ctx, issue.Number, "contributor", time.Hour)
					paused, err := run.prepareImplementation()
					run.heartbeat.Stop()
					if err != nil || paused {
						t.Fatalf("resumed implementation admission: paused=%v err=%v", paused, err)
					}
				}
				if err := worker.CheckClaim(ctx, issue.Number, "contributor"); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(root, ".swarm/active/REVIEW.md")); !os.IsNotExist(err) {
					t.Fatal("review wrote upstream checkout")
				}
			})
		}
	}
}

func TestIssueWorkOutputExpiredRecoveryClaim(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			st := &lease.IssueState{}
			st.Lease.IssueNumber = 42
			st.Lease.Status = "active"
			st.Lease.ExpiresAt = time.Now().Add(-time.Hour)
			if active {
				st.Lease.ExpiresAt = time.Now().Add(time.Hour)
			}
			var recovery *IssueRecoveryError
			if err := contractRevisionRecovery(st, lease.ErrPlanReplanLimit); !errors.As(err, &recovery) {
				t.Fatal(err)
			}
			want := "edit"
			if active {
				want = "release"
			}
			if recovery.Operation != want {
				t.Fatalf("operation=%s want %s", recovery.Operation, want)
			}
		})
	}
}

func TestPlanReviewHistoryAndPlannerReplanTelemetry(t *testing.T) {
	root, e, tr := setupProjectPlanTestRepo(t)
	ctx := context.Background()
	wu := &contract.WorkUnit{Objective: "Converge review", Constraints: []string{"Preserve approval"}, Acceptance: []string{"Review revised plan"}, Workflow: "feature", Gate: contract.GateAgent}
	issue, err := tr.CreateIssue(ctx, "Converge", contract.FormatIssueMarkdown(wu), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.LeaseManager.ApproveTask(ctx, issue.Number, "maintainer"); err != nil {
		t.Fatal(err)
	}
	wu, err = contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		t.Fatal(err)
	}
	mock := &agent.MockRunner{}
	planners, reviews := 0, 0
	var reservationCounts []int
	var firstPlan, firstTest string
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			active := filepath.Join(req.WorktreePath, ".swarm/active")
			if req.Role == "planner" {
				st, err := e.LeaseManager.GetCurrentState(ctx, issue.Number)
				if err != nil {
					t.Fatal(err)
				}
				reservationCounts = append(reservationCounts, st.PlanReplanCount)
			}
			if req.Role == "reviewer" {
				reviews++
				b, err := os.ReadFile(filepath.Join(active, "REVIEW_EVIDENCE.md"))
				if err != nil {
					t.Fatal(err)
				}
				sys, err := os.ReadFile(filepath.Join(active, "SYSTEM_PROMPT.md"))
				if err != nil {
					t.Fatal(err)
				}
				if reviews == 1 {
					if strings.Contains(string(b), "Earlier-round") || strings.Contains(string(sys), "resolved or unresolved") {
						t.Fatal("first review has history")
					}
				} else {
					if !strings.Contains(string(b), "Earlier-round evidence: round 1") || !strings.Contains(string(b), "continuity-finding") || !strings.Contains(string(sys), "resolved or unresolved") || !strings.Contains(string(sys), "reversal of earlier guidance") {
						t.Fatalf("second bundle missing context: %s", b)
					}
					st, err := e.LeaseManager.GetCurrentState(ctx, issue.Number)
					if err != nil {
						t.Fatal(err)
					}
					bindings := lease.ReviewBindings(st)
					if bindings["plan_digest"] == firstPlan || bindings["test_plan_digest"] == firstTest {
						t.Fatal("reports unchanged")
					}
					if lease.VerifyApproval(st, wu) == nil {
						t.Fatal("history approved new reports")
					}
				}
			}
			result, err := mock.Run(ctx, req)
			if err != nil {
				return result, err
			}
			if req.Role == "planner" {
				planners++
				if planners == 2 {
					for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
						path := filepath.Join(active, name)
						b, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, append(b, []byte("\nRevised with missing coverage.\n")...), 0644); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if req.Role == "reviewer" && reviews == 1 {
				text := "VERDICT: CHANGES_REQUESTED\n\ncontinuity-finding: add missing coverage"
				if err := os.WriteFile(filepath.Join(active, "REVIEW.md"), []byte(text), 0644); err != nil {
					t.Fatal(err)
				}
				result.Output = text
			}
			return result, nil
		}}, nil
	}
	opts := WorkOptions{RepoRoot: root, IssueNumber: issue.Number, AgentName: "mock", Phase: "all", Headless: true}
	first, err := e.ExecuteWork(ctx, opts)
	if !errors.Is(err, ErrPlanReviewRejected) {
		t.Fatalf("first: %v", err)
	}
	bindings := lease.ReviewBindings(first.State)
	firstPlan, firstTest = bindings["plan_digest"], bindings["test_plan_digest"]
	opts.ClaimID = first.State.Lease.ClaimID
	opts.Phase = "plan"
	revised, err := e.ExecuteWork(ctx, opts)
	if err != nil {
		t.Fatalf("replan: %v", err)
	}
	if revised.State.PlanReplanCount != 1 || !reflect.DeepEqual(reservationCounts, []int{0, 1}) {
		t.Fatalf("reservations: %v", reservationCounts)
	}
	var counts []int
	for _, run := range revised.State.RoleRuns {
		if run.Role == "planner" {
			if run.ReplanCount == nil {
				t.Fatal("missing count")
			}
			counts = append(counts, int(*run.ReplanCount))
		}
	}
	if !reflect.DeepEqual(counts, reservationCounts) {
		t.Fatalf("telemetry %v != persisted reservations %v", counts, reservationCounts)
	}
	opts.Phase = "review"
	reviewed, err := e.ExecuteWork(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if reviews != 2 || len(reviewed.State.PlanReviewHistory) != 2 {
		t.Fatal("missing review rounds")
	}
	if err := lease.VerifyApproval(reviewed.State, wu); err != nil {
		t.Fatal(err)
	}
	current := lease.ReviewBindings(reviewed.State)
	for _, a := range reviewed.State.Artifacts {
		if a.Type == contract.ArtifactReview && (a.Metadata["plan_digest"] != current["plan_digest"] || a.Metadata["test_plan_digest"] != current["test_plan_digest"]) {
			t.Fatal("review binds stale reports")
		}
	}
}

func TestPlanReviewHistoryRoleAndRevisionScope(t *testing.T) {
	root, _, _ := setupProjectPlanTestRepo(t)
	out, err := gitfixture.Command(t, root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	st := &lease.IssueState{BaseSHA: strings.TrimSpace(string(out)), TaskDigest: "task", PolicyDigest: "policy"}
	wu := &contract.WorkUnit{ID: "ISSUE-375", IssueNumber: 375, Workflow: "feature"}
	history := []workspace.PlanReviewEvidence{{Round: 1, Findings: "history-secret-marker", TaskDigest: st.TaskDigest, PolicyDigest: st.PolicyDigest, BaseSHA: st.BaseSHA}}
	for _, tc := range []struct {
		role    string
		pr      int
		fork    bool
		gateway bool
	}{{"planner", 0, false, false}, {"developer", 0, false, false}, {"tester", 0, false, false}, {"reviewer", 0, true, false}, {"reviewer", 12, false, false}, {"reviewer", 0, false, true}, {"reviewer", 0, false, false}} {
		t.Run(fmt.Sprintf("%s/%d/%t/%t", tc.role, tc.pr, tc.fork, tc.gateway), func(t *testing.T) {
			st.ForkRepo = ""
			if tc.fork {
				st.ForkRepo = "owner/fork"
			}
			st.PlanReviewHistory = nil
			if err := compileExecutionBundle(root, wu, nil, st, tc.role, tc.pr, tc.gateway); err != nil {
				t.Fatal(err)
			}
			read := func(name string) string {
				b, err := os.ReadFile(filepath.Join(root, ".swarm/active", name))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			beforeEvidence, beforePrompt := read("REVIEW_EVIDENCE.md"), read("SYSTEM_PROMPT.md")
			st.PlanReviewHistory = history
			if err := compileExecutionBundle(root, wu, nil, st, tc.role, tc.pr, tc.gateway); err != nil {
				t.Fatal(err)
			}
			included := tc.role == "reviewer" && !tc.fork && tc.pr == 0 && !tc.gateway
			if included {
				if !strings.Contains(read("REVIEW_EVIDENCE.md"), "history-secret-marker") {
					t.Fatal("missing history")
				}
				st.TaskDigest = "new task"
				if err := compileExecutionBundle(root, wu, nil, st, tc.role, tc.pr, tc.gateway); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(read("REVIEW_EVIDENCE.md"), "history-secret-marker") || strings.Contains(read("SYSTEM_PROMPT.md"), "resolved or unresolved") {
					t.Fatal("stale revision context")
				}
				st.TaskDigest = "task"
			} else if beforeEvidence != read("REVIEW_EVIDENCE.md") || beforePrompt != read("SYSTEM_PROMPT.md") {
				t.Fatal("history leaked into excluded bundle")
			}
		})
	}
}
