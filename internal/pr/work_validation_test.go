package pr

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/testutil"
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
	if err := testutil.WriteExecutable(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
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

func TestPRTesterRetryRetainsUnpublishedImplementation(t *testing.T) {
	s, issue, number, published := fixture(t, "required_checks:\n  - name: retry-sentinel\n    command: echo pr-retry-check-executed\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n")
	mock := &agent.MockRunner{}
	initial := true
	developers, testers := 0, 0
	s.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &pausePRRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			if req.Role == "developer" {
				developers++
			}
			if req.Role == "tester" {
				testers++
				if initial {
					return nil, req.CheckImport(ctx, []string{".swarm/active/IMPLEMENTATION.md"}, nil)
				}
			}
			return mock.Run(ctx, req)
		}}, nil
	}
	opts := Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}}
	first, err := s.Work(t.Context(), opts)
	if err == nil || first == nil || developers != 1 || testers != 1 {
		t.Fatalf("initial: %+v %v calls %d/%d", first, err, developers, testers)
	}
	before, err := s.Manager.GetCurrentState(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if before.PRRevision == nil || before.PRRevision.LocalHeadSHA == published {
		t.Fatalf("no local implementation: %+v", before.PRRevision)
	}
	revision := *before.PRRevision
	var implementation contract.Artifact
	for _, a := range before.Artifacts {
		if a.Type == contract.ArtifactImplementation {
			implementation = a
		}
	}
	initial = false
	developers, testers = 0, 0
	opts.Work.Phase = "test"
	retry, err := s.Work(t.Context(), opts)
	if err != nil || retry == nil || retry.Work == nil || !retry.Work.ValidationPassed || retry.Work.PhaseCompleted != "test" {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	if developers != 0 || testers != 1 || len(retry.Work.Validation.CheckOutcomes) == 0 {
		t.Fatalf("calls/checks %d/%d %+v", developers, testers, retry.Work.Validation)
	}
	after, err := s.Manager.GetCurrentState(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if after.PRRevision == nil || *after.PRRevision != revision {
		t.Fatalf("revision reset: %+v", after.PRRevision)
	}
	for _, a := range after.Artifacts {
		if a.Type == contract.ArtifactImplementation && !reflect.DeepEqual(a, implementation) {
			t.Fatal("implementation replaced")
		}
	}
	status, err := s.Status(t.Context(), number)
	if err != nil || status.PR.HeadSHA != published {
		t.Fatalf("implicit publication: %+v %v", status, err)
	}
}

func TestPRTestRetryRefusesFreshRevisionAndGateway(t *testing.T) {
	for _, mode := range []string{"fresh", "fork", "gateway"} {
		t.Run(mode, func(t *testing.T) {
			s, issue, number, _ := fixture(t)
			before, _ := s.Manager.GetCurrentState(t.Context(), issue)
			if mode == "fork" {
				s.Fork = true
			}
			if mode == "gateway" {
				s.Manager.SetWorkerGateway(&testRetryGateway{t: t})
			}
			if _, err := s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{Phase: "test", AgentName: "mock"}}); err == nil {
				t.Fatal("unsupported retry accepted")
			}
			after, _ := s.Manager.GetCurrentState(t.Context(), issue)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("unsupported retry mutated state")
			}
		})
	}
}

type testRetryGateway struct{ t *testing.T }

func (g *testRetryGateway) Apply(context.Context, lease.ForkRequest) (*lease.IssueState, error) {
	g.t.Fatal("gateway dispatched retry")
	return nil, nil
}
