package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
)

func TestWorkValidationReportPropagation(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		runnerErr     error
		wantReport    bool
		wantPassed    bool
	}{
		{name: "passing checks", command: "git status --porcelain", wantReport: true, wantPassed: true},
		{name: "failing check", command: "echo work-check-failed; exit 3", wantReport: true},
		{name: "failure before validation", command: "git status --porcelain", runnerErr: errors.New("runner unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, e, tr := setupProjectPlanTestRepo(t)
			configureValidation(t, root)
			policy := "required_checks:\n  - name: work-check\n    command: " + tc.command + "\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n  - .swarm/contracts/**\n"
			if err := os.WriteFile(filepath.Join(root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("git", "-C", root, "commit", "-am", "test validation").CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			wu := &contract.WorkUnit{Objective: "Report checks", Constraints: []string{"Keep fencing"}, Acceptance: []string{"Carry the report"}, Workflow: "feature", Gate: contract.GateNone}
			issue, err := tr.CreateIssue(t.Context(), "Report", contract.FormatIssueMarkdown(wu), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = e.LeaseManager.ApproveTask(t.Context(), issue.Number, "maintainer"); err != nil {
				t.Fatal(err)
			}
			if tc.runnerErr != nil {
				e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
					return &budgetTestRunner{run: func(context.Context, agent.RunRequest) (*agent.RunResult, error) {
						return nil, tc.runnerErr
					}}, nil
				}
			}
			res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
			if res == nil {
				t.Fatalf("no result: %v", err)
			}
			if (err == nil) != tc.wantPassed {
				t.Fatalf("err = %v", err)
			}
			if !tc.wantReport {
				if res.Validation != nil {
					t.Fatalf("report before validation: %+v", res.Validation)
				}
				return
			}
			if res.Validation == nil || res.Validation.Passed != tc.wantPassed || res.ValidationPassed != tc.wantPassed {
				t.Fatalf("report = %+v, passed = %t", res.Validation, res.ValidationPassed)
			}
			if !tc.wantPassed {
				outcome := res.Validation.CheckOutcomes[len(res.Validation.CheckOutcomes)-1]
				if outcome.Passed || outcome.ExitCode != 3 || outcome.OutputTail != "work-check-failed" {
					t.Fatalf("failed outcome = %+v", outcome)
				}
			}
		})
	}
}
