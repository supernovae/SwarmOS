package workflow

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

const splitPlanAppend = "Split recommendation: required\n\n### Child one\n\n**Objective**: First half.\n\n**Acceptance Criteria**\n- [ ] `go test ./a` passes"

func TestPlanRecommendsSplit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		plan string
		want bool
	}{
		{"absent", "# Plan\n\nNormal work.\n", false},
		{"exact LF", "# Plan\n\nSplit recommendation: required\n\n### Child\n", true},
		{"exact CRLF", "# Plan\r\n\r\nSplit recommendation: required\r\n### Child\r\n", true},
		{"trailing space", "Split recommendation: required  \n", true},
		{"first line", "Split recommendation: required", true},
		{"inline prose", "We considered a `Split recommendation: required` line but rejected it.\n", false},
		{"quoted", "> Split recommendation: required\n", false},
		{"prefixed", "- Split recommendation: required\n", false},
		{"indented", "    Split recommendation: required\n", false},
		{"case variant", "split recommendation: required\n", false},
		{"other value", "Split recommendation: optional\n", false},
		{"guidance only", "# Plan\n\nNormal work.\n" + guidanceStart + "## Operator guidance\n\n> x\nSplit recommendation: required\n<!-- swarm:guidance:end -->\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PlanRecommendsSplit(tc.plan); got != tc.want {
				t.Fatalf("PlanRecommendsSplit(%q) = %t, want %t", tc.plan, got, tc.want)
			}
		})
	}
}

func TestSplitRecommendedReadsOnlyLatestRecordedDesign(t *testing.T) {
	t.Parallel()
	marker := "Split recommendation: required\n"
	for _, tc := range []struct {
		name string
		arts []contract.Artifact
		want bool
	}{
		{"latest design", []contract.Artifact{{Type: contract.ArtifactDesign, Body: "old"}, {Type: contract.ArtifactDesign, Body: marker}}, true},
		{"superseded design", []contract.Artifact{{Type: contract.ArtifactDesign, Body: marker}, {Type: contract.ArtifactDesign, Body: "normal"}}, false},
		{"test plan only", []contract.Artifact{{Type: contract.ArtifactDesign, Body: "normal"}, {Type: contract.ArtifactTestPlan, Body: marker}}, false},
		{"review evidence only", []contract.Artifact{{Type: contract.ArtifactDesign, Body: "normal"}, {Type: contract.ArtifactReview, Body: marker}}, false},
		{"no design", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := splitRecommended(&lease.IssueState{Artifacts: tc.arts}); got != tc.want {
				t.Fatalf("splitRecommended = %t, want %t", got, tc.want)
			}
		})
	}
	if splitRecommended(nil) {
		t.Fatal("nil state recommended a split")
	}
}

// splitSpy records which roles run and delegates to the mock agent.
type splitSpy struct {
	agent.MockRunner
	mu    sync.Mutex
	roles []string
}

func (s *splitSpy) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	s.mu.Lock()
	s.roles = append(s.roles, req.Role)
	s.mu.Unlock()
	return s.MockRunner.Run(ctx, req)
}

func (s *splitSpy) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	roles := s.roles
	s.roles = nil
	return roles
}

func splitEngine(t *testing.T, gate contract.GateMode) (*Engine, *tracker.LocalTracker, *tracker.Issue, *splitSpy) {
	t.Helper()
	root, e, tr := setupProjectPlanTestRepo(t)
	configureValidation(t, root)
	if out, err := gitfixture.Command(t, "", "-C", root, "commit", "-am", "test validation").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	wu := &contract.WorkUnit{Objective: "Large work", Constraints: []string{"Keep fencing"}, Acceptance: []string{"go test passes"}, Workflow: "feature", Gate: gate}
	issue, err := tr.CreateIssue(t.Context(), "Split", contract.FormatIssueMarkdown(wu), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.LeaseManager.ApproveTask(t.Context(), issue.Number, "maintainer"); err != nil {
		t.Fatal(err)
	}
	spy := &splitSpy{}
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) { return spy, nil }
	return e, tr, issue, spy
}

func assertSplitStop(t *testing.T, res *WorkResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("split stop returned error: %v", err)
	}
	if !res.SplitRecommended || res.Outcome() != "split-recommended" || res.PhaseCompleted != "plan" || res.ValidationPassed || res.HandedOffToPhase != "" || res.PausedForHuman {
		t.Fatalf("unexpected split result: %+v", res)
	}
	for _, a := range res.State.Artifacts {
		switch a.Type {
		case contract.ArtifactReview, contract.ArtifactImplementation, contract.ArtifactValidation:
			t.Fatalf("split stop recorded a %s artifact (%s)", a.Type, a.Verdict)
		}
		if a.Verdict == "FAIL" {
			t.Fatalf("split stop recorded a FAIL verdict: %+v", a)
		}
	}
	if res.State.Lease.Status == "handoff" {
		t.Fatalf("split stop handed off the claim: %+v", res.State.Lease)
	}
}

func TestExecuteWorkStopsOnSplitRecommendation(t *testing.T) {
	for _, gate := range []contract.GateMode{contract.GateNone, contract.GateAgent, contract.GateHuman} {
		t.Run(string(gate), func(t *testing.T) {
			e, tr, issue, spy := splitEngine(t, gate)
			before, err := tr.GetIssue(t.Context(), issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true}
			t.Setenv("SWARM_MOCK_PLAN_APPEND", splitPlanAppend)
			res, err := e.ExecuteWork(t.Context(), opts)
			assertSplitStop(t, res, err)
			opts.ClaimID = res.State.Lease.ClaimID
			if roles := spy.take(); !slices.Equal(roles, []string{"planner"}) {
				t.Fatalf("roles run = %v, want only planner", roles)
			}
			if !hasDesignAndTestPlanArtifacts(res.State) {
				t.Fatal("split stop lost the recorded plan artifacts")
			}
			wu, err := contract.ParseWorkUnitFromIssue(before.Number, before.Title, before.Body)
			if err != nil {
				t.Fatal(err)
			}
			if lease.VerifyApproval(res.State, wu) == nil && gate != contract.GateNone {
				t.Fatal("split recommendation satisfied the plan gate")
			}

			// Resuming reuses the recorded plan and stops again before review
			// or implementation, whichever phase is requested.
			t.Setenv("SWARM_MOCK_PLAN_APPEND", "")
			for _, phase := range []string{"all", "review", "implement"} {
				opts.Phase = phase
				res, err := e.ExecuteWork(t.Context(), opts)
				assertSplitStop(t, res, err)
				if roles := spy.take(); len(roles) != 0 {
					t.Fatalf("phase %s ran %v on a split plan", phase, roles)
				}
			}

			// The WorkUnit, its approval, and the issue set are unchanged.
			after, err := tr.GetIssue(t.Context(), issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			if after.Title != before.Title || after.Body != before.Body {
				t.Fatal("split recommendation changed the WorkUnit")
			}
			issues, err := tr.ListIssues(t.Context(), tracker.IssueFilter{State: "all"})
			if err != nil {
				t.Fatal(err)
			}
			if len(issues) != 1 {
				t.Fatalf("split recommendation created issues: %d", len(issues))
			}

			// A fresh normal plan goes back through the usual gates.
			opts.Phase = "plan"
			if _, err := e.ExecuteWork(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
			spy.take()
			opts.Phase = "all"
			res, err = e.ExecuteWork(t.Context(), opts)
			if res != nil && res.SplitRecommended {
				t.Fatal("normal re-plan still stopped for a split")
			}
			roles := spy.take()
			switch gate {
			case contract.GateHuman:
				if err != nil || !res.PausedForHuman || slices.Contains(roles, "developer") {
					t.Fatalf("human gate after re-plan: %v %+v roles=%v", err, res, roles)
				}
			default:
				if err != nil || !slices.Contains(roles, "reviewer") || !slices.Contains(roles, "developer") {
					t.Fatalf("agent gate after re-plan should review then implement: %v roles=%v", err, roles)
				}
			}
		})
	}
}

func TestSplitRecommendationPlanHandoffStops(t *testing.T) {
	e, _, issue, spy := splitEngine(t, contract.GateAgent)
	t.Setenv("SWARM_MOCK_PLAN_APPEND", splitPlanAppend)
	res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "plan", Handoff: true, Headless: true})
	assertSplitStop(t, res, err)
	if roles := spy.take(); !slices.Equal(roles, []string{"planner"}) {
		t.Fatalf("roles run = %v", roles)
	}
}

func TestSplitRecommendationCannotSkipRequiredGate(t *testing.T) {
	for _, gate := range []contract.GateMode{contract.GateAgent, contract.GateHuman} {
		t.Run(string(gate), func(t *testing.T) {
			e, tr, issue, spy := splitEngine(t, gate)
			opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "plan", Headless: true}
			t.Setenv("SWARM_MOCK_PLAN_APPEND", splitPlanAppend)
			res, err := e.ExecuteWork(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			opts.ClaimID = res.State.Lease.ClaimID
			// Replace the split plan with a normal one, then try to implement
			// without a review: the gate still refuses.
			t.Setenv("SWARM_MOCK_PLAN_APPEND", "")
			if _, err := e.ExecuteWork(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
			spy.take()
			opts.Phase = "implement"
			_, err = e.ExecuteWork(t.Context(), opts)
			if !errors.Is(err, ErrPlanApprovalNeeded) {
				t.Fatalf("implement without approval: %v", err)
			}
			if roles := spy.take(); slices.Contains(roles, "developer") {
				t.Fatalf("developer ran without plan approval: %v", roles)
			}
			st, err := e.LeaseManager.GetCurrentState(t.Context(), issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			iss, err := tr.GetIssue(t.Context(), issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			wu, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.VerifyApproval(st, wu); err == nil {
				t.Fatal("plan approval appeared without a review")
			}
		})
	}
}

func TestSplitRecommendationRequiresTheClaim(t *testing.T) {
	e, _, issue, _ := splitEngine(t, contract.GateAgent)
	t.Setenv("SWARM_MOCK_PLAN_APPEND", splitPlanAppend)
	if _, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "plan", Headless: true}); err != nil {
		t.Fatal(err)
	}
	// Another actor cannot record a plan on the held claim, split or not.
	_, err := e.LeaseManager.UpdateArtifact(t.Context(), issue.Number, "intruder", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock:planner", Body: "Split recommendation: required\n"})
	if err == nil {
		t.Fatal("artifact recorded without holding the claim")
	}
	if strings.TrimSpace(err.Error()) == "" {
		t.Fatal("empty fence error")
	}
}
