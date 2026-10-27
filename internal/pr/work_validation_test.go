package pr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestPRStatusShowsTrustedRepairSummary(t *testing.T) {
	s, _, number, _ := fixture(t, "required_checks:\n  - name: test\n    command: go test -count=1 ./...\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n")
	// The fake toolchain fails TestBroken everywhere, including at the base.
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\necho '--- FAIL: TestBroken (0.00s)'; echo FAIL; printf 'FAIL\\texample.com/m/broken\\t0.01s\\n'; exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "echo change > change.txt")
	result, err := s.Work(context.Background(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
	if !errors.Is(err, workflow.ErrPreExistingFailures) || result == nil || result.Work == nil || result.Work.Validation.Repair == nil {
		t.Fatalf("work = %+v, %v", result, err)
	}
	calls, _ := os.ReadFile(log)
	status, err := s.Status(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(log); string(after) != string(calls) {
		t.Fatalf("status ran checks: %s", after[len(calls):])
	}
	if status.Repair == nil || !status.Repair.AllPreExisting || len(status.Repair.Tests) != 1 || status.Repair.Tests[0].Classification != validation.TestPreExisting || status.Repair.Tests[0].Name != "TestBroken" {
		t.Fatalf("status repair = %+v", status.Repair)
	}
	raw, err := json.Marshal(status)
	if err != nil || !strings.Contains(string(raw), `"classification":"pre-existing"`) || !strings.Contains(string(raw), `"all_pre_existing":true`) {
		t.Fatalf("status JSON: %s %v", raw, err)
	}
}
