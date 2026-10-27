package workflow

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
)

const developerOutputRerun = " --phase implement --instructions 'Record notes, test results, and blocked checks in .swarm/active/IMPLEMENTATION.md. Do not change other .swarm/active/ files except LESSONS.md.'"

func TestDeveloperOutputRejectionGuidance(t *testing.T) {
	const validation = ".swarm/active/VALIDATION.md"
	for _, tc := range []struct {
		name  string
		role  string
		paths []string
		// hint applies without harness changes; harnessHint when they are allowed.
		hint, harnessHint bool
	}{
		{"validation only", "developer", []string{validation}, true, true},
		{"duplicate validation", "developer", []string{validation, validation}, true, true},
		{"accepted edits plus validation", "developer", []string{"src/main.go", ".swarm/active/IMPLEMENTATION.md", ".swarm/active/LESSONS.md", validation}, true, true},
		{"tester", "tester", []string{".swarm/active/IMPLEMENTATION.md"}, false, false},
		{"plan", "developer", []string{".swarm/active/PLAN.md"}, false, false},
		{"test plan", "developer", []string{".swarm/active/TEST_PLAN.md"}, false, false},
		{"workunit", "developer", []string{".swarm/active/WORKUNIT.md"}, false, false},
		{"system prompt", "developer", []string{".swarm/active/SYSTEM_PROMPT.md"}, false, false},
		{"unknown active file", "developer", []string{".swarm/active/unknown"}, false, false},
		{"validation with approved input", "developer", []string{validation, ".swarm/active/PLAN.md"}, false, false},
		{"validation with protected path", "developer", []string{validation, "policy/checks"}, false, true},
	} {
		for _, harness := range []bool{false, true} {
			for _, fork := range []bool{false, true} {
				err := writableImportPolicy(tc.role, tc.paths, []string{"policy/**"}, harness, fork)
				var rejection *importRejection
				if !errors.As(err, &rejection) {
					t.Fatalf("%s harness=%v fork=%v: expected rejection, got %v", tc.name, harness, fork, err)
				}
				hint := tc.hint
				if harness {
					hint = tc.harnessHint
				}
				if got := rejection.developerOutputOnly(); got != hint {
					t.Fatalf("%s harness=%v fork=%v: developerOutputOnly=%v want %v (%v)", tc.name, harness, fork, got, hint, rejection.Paths)
				}
				reason := importRejectionReason(rejection, 7, 0)
				if hint != strings.Contains(reason, "--instructions") || hint != strings.Contains(reason, "record results in .swarm/active/IMPLEMENTATION.md") {
					t.Fatalf("%s harness=%v fork=%v: reason %q", tc.name, harness, fork, reason)
				}
			}
		}
	}
	if (&importRejection{Role: "developer"}).developerOutputOnly() {
		t.Fatal("empty rejection must not get developer output guidance")
	}
}

func TestImportRejectionReasonRouting(t *testing.T) {
	hinted := &importRejection{Paths: []string{".swarm/active/VALIDATION.md"}, Role: "developer"}
	generic := &importRejection{Paths: []string{".swarm/active/PLAN.md"}, Role: "developer"}
	for _, tc := range []struct {
		rejection *importRejection
		issue, pr int
		want      string
	}{
		{hinted, 7, 0, "rerun with swarm issue work 7" + developerOutputRerun},
		{hinted, 7, 42, "rerun with swarm pr work 42" + developerOutputRerun},
		{generic, 7, 0, "rejected agent changes to: .swarm/active/PLAN.md; returned changes were not imported; worktree was left unchanged; rerun with swarm issue work 7 --phase implement"},
		{generic, 7, 42, "rejected agent changes to: .swarm/active/PLAN.md; returned changes were not imported; worktree was left unchanged; rerun with swarm pr work 42 --phase implement"},
	} {
		got := importRejectionReason(tc.rejection, tc.issue, tc.pr)
		if !strings.HasSuffix(got, tc.want) {
			t.Fatalf("got %q want suffix %q", got, tc.want)
		}
	}
}

func TestDeveloperOutputImportRecoveryGuidance(t *testing.T) {
	e, issue := budgetEngine(t)
	mock := &agent.MockRunner{}
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			if req.Role == "developer" {
				return nil, req.CheckImport(ctx, []string{"src/main.go", ".swarm/active/IMPLEMENTATION.md", ".swarm/active/VALIDATION.md"})
			}
			return mock.Run(ctx, req)
		}}, nil
	}
	opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true}
	res, err := e.ExecuteWork(t.Context(), opts)
	var rejection *importRejection
	if !errors.As(err, &rejection) {
		t.Fatalf("expected rejection: %v", err)
	}
	state, loadErr := e.LeaseManager.GetCurrentState(t.Context(), issue.Number)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	rerun := "swarm issue work " + strconv.Itoa(issue.Number) + developerOutputRerun
	for _, text := range []string{"rejected agent changes to: .swarm/active/VALIDATION.md;", "worktree was left unchanged", "developers record results in .swarm/active/IMPLEMENTATION.md, not .swarm/active/VALIDATION.md", rerun} {
		if !strings.Contains(err.Error(), text) || !strings.Contains(state.NextPhaseReason, text) {
			t.Fatalf("missing %q: %v %+v", text, err, state)
		}
	}
	if state.Lease.NextPhase != "implement" || res.ClaimBudgetPause != nil {
		t.Fatalf("bad recovery: %+v", res)
	}
	for _, a := range state.Artifacts {
		if a.Type == contract.ArtifactValidation || a.Type == contract.ArtifactImplementation {
			t.Fatalf("rejected run recorded artifact: %+v", a)
		}
	}
}
