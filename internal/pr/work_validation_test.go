package pr

import (
	"context"
	"errors"
	"testing"

	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workflow"
)

func TestWorkValidationReportOnFailedPRWork(t *testing.T) {
	s, _, number, _ := fixture(t, "required_checks:\n  - name: pr-check\n    command: if test -f fail-check; then echo pr-check-failed; exit 4; fi\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n")
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "echo fail > fail-check")
	result, err := s.Work(context.Background(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
	if !errors.Is(err, validation.ErrRequiredCheckFailed) {
		t.Fatalf("work error = %v", err)
	}
	if result == nil || result.Work == nil || result.Work.Validation == nil {
		t.Fatalf("failed work dropped the validation report: %+v", result)
	}
	report := result.Work.Validation
	if report.Passed || result.Work.ValidationPassed || result.Validation != nil {
		t.Fatalf("report = %+v, standalone validation = %+v", report, result.Validation)
	}
	outcome := report.CheckOutcomes[len(report.CheckOutcomes)-1]
	if outcome.Name != "pr-check" || outcome.Passed || outcome.ExitCode != 4 || outcome.OutputTail != "pr-check-failed" {
		t.Fatalf("failed outcome = %+v", outcome)
	}
}
