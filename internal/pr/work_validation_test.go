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
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
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

func TestPRTestRetryRefusesChangedIdentityWithoutReset(t *testing.T) {
	for _, scenario := range []string{"remote moved", "revision identity", "worktree branch", "rebase intent", "rebase in progress"} {
		t.Run(scenario, func(t *testing.T) {
			s, issue, number, published := fixture(t, "required_checks:\n  - name: retry-sentinel\n    command: echo pr-retry-check-executed\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n")
			mock := &agent.MockRunner{}
			initial := true
			calls := map[string]int{}
			s.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &pausePRRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					calls[req.Role]++
					if req.Role == "tester" && initial {
						return nil, req.CheckImport(ctx, []string{".swarm/active/IMPLEMENTATION.md"}, nil)
					}
					return mock.Run(ctx, req)
				}}, nil
			}
			if _, err := s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}}); err == nil || calls["developer"] != 1 || calls["tester"] != 1 {
				t.Fatalf("initial: %v calls %v", err, calls)
			}
			initial = false
			git := func(dir string, args ...string) string {
				t.Helper()
				out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			worktree := workspace.PRWorktreePath(s.Root, number)
			wantPublished := published
			switch scenario {
			case "remote moved":
				// Someone else publishes to the PR after the implementation.
				moved := git(s.Root, "-c", "user.name=Other", "-c", "user.email=other@example.invalid", "commit-tree", "-p", published, "-m", "external push", published+"^{tree}")
				if err := s.Tracker.(*tracker.LocalTracker).PublishPRHead(t.Context(), number, published, moved); err != nil {
					t.Fatal(err)
				}
				if status, err := s.Status(t.Context(), number); err != nil || status.PR.HeadSHA != moved {
					t.Fatalf("remote did not move: %+v %v", status, err)
				}
				wantPublished = moved
			case "revision identity", "rebase intent":
				prMutateState(t, s, issue, func(st *lease.IssueState) {
					if scenario == "rebase intent" {
						st.PRRevision.Intent = "rebase"
					} else {
						st.PRRevision.RemoteHeadSHA = strings.Repeat("e", 40)
					}
				})
			case "worktree branch":
				git(worktree, "checkout", "-q", "-b", "detour")
			case "rebase in progress":
				dir := git(worktree, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				for name, body := range map[string]string{"head-name": "refs/heads/" + workspace.PRBranchName(number), "onto": published, "orig-head": git(worktree, "rev-parse", "HEAD")} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := s.Manager.GetCurrentState(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if before.PRRevision == nil || before.PRRevision.LocalHeadSHA == published {
				t.Fatalf("no local implementation: %+v", before.PRRevision)
			}
			revision := *before.PRRevision
			localHead := git(worktree, "rev-parse", "HEAD")
			clear(calls)
			retry, err := s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "test"}})
			if err == nil || (retry != nil && retry.Work != nil && (retry.Work.ValidationPassed || retry.Work.PhaseCompleted == "test")) {
				t.Fatalf("changed PR context accepted: %v %+v", err, retry)
			}
			if len(calls) != 0 {
				t.Fatalf("agents launched %v: %v", calls, err)
			}
			after, err := s.Manager.GetCurrentState(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if after.PRRevision == nil || *after.PRRevision != revision {
				t.Fatalf("revision reset:\nbefore %+v\nafter  %+v", revision, after.PRRevision)
			}
			if !reflect.DeepEqual(implementationOf(after), implementationOf(before)) {
				t.Fatal("implementation evidence changed")
			}
			for _, a := range after.Artifacts {
				if a.Type == contract.ArtifactValidation && a.Verdict == "PASS" {
					t.Fatalf("refused retry persisted PASS: %+v", a)
				}
			}
			if head := git(worktree, "rev-parse", "HEAD"); head != localHead {
				t.Fatalf("checkpoint reset %s -> %s", localHead, head)
			}
			status, err := s.Status(t.Context(), number)
			if err != nil || status.PR.HeadSHA != wantPublished {
				t.Fatalf("implicit publication: %+v %v", status, err)
			}
		})
	}
}

func implementationOf(st *lease.IssueState) contract.Artifact {
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactImplementation {
			return a
		}
	}
	return contract.Artifact{}
}

// prMutateState models an out-of-band coordinator change with a raw CAS write.
func prMutateState(t *testing.T, s *Service, issue int, change func(*lease.IssueState)) {
	t.Helper()
	store := s.Manager.Store()
	data, rev, err := store.Load(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	var st lease.IssueState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	change(&st)
	if data, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwap(t.Context(), issue, rev, data); err != nil {
		t.Fatal(err)
	}
}
