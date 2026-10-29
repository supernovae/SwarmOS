package workflow

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestEngineDroppedCheckpointRecovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.test")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	configureValidation(t, root)
	git("add", ".")
	git("commit", "-m", "harness")
	remote := filepath.Join(t.TempDir(), "remote.git")
	git("init", "--bare", "--initial-branch=main", remote)
	git("remote", "add", "origin", remote)
	git("push", "origin", "main")
	tr := tracker.NewLocalTracker(root).WithUser("local-dev", "OWNER")
	wu := &contract.WorkUnit{Objective: "Recover checkpoint", Constraints: []string{"Keep commits"}, Acceptance: []string{"Resume at base"}, Workflow: "feature", Gate: contract.GateNone}
	issue, err := tr.CreateIssue(ctx, "Recover checkpoint", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(root, tr)
	if err != nil {
		t.Fatal(err)
	}
	m := eng.LeaseManager
	m.SetStore(root, coordination.NewGitStore(root))
	approved, err := m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	path, branch, err := workspace.ProvisionAt(root, issue.Number, issue.Title, approved.BaseSHA, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RecordCheckpoint(ctx, issue.Number, "local-dev", branch, approved.BaseSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, issue.Number, "local-dev", "test"); err != nil {
		t.Fatal(err)
	}
	git("commit", "--allow-empty", "-m", "new base")
	base := git("rev-parse", "HEAD")
	git("push", "origin", "main")
	if _, err := m.ApproveTask(ctx, issue.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	result, err := eng.ExecuteWork(ctx, WorkOptions{RepoRoot: root, IssueNumber: issue.Number, PlannerAgent: "mock", ReviewerAgent: "mock", DeveloperAgent: "mock", TesterAgent: "mock", Phase: "all", Gate: contract.GateNone, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorktreePath != path || result.BranchName != branch || result.BaseSHA != base || !result.ValidationPassed {
		t.Fatalf("unexpected recovery: %+v", result)
	}
	final, err := m.GetCurrentState(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	if final.DroppedCheckpoint != nil || final.HeadSHA == "" {
		t.Fatal("engine did not publish recovered checkpoint")
	}
	if ok, err := workspace.BaseIsAncestor(ctx, root, base, final.HeadSHA); err != nil || !ok {
		t.Fatalf("checkpoint ancestry: %v %v", ok, err)
	}
	if !strings.HasPrefix(git("ls-remote", "origin", "refs/heads/"+branch), final.HeadSHA) {
		t.Fatal("engine remote publication missing")
	}
}

// This fixture is reused for output, identity-race and boundary regressions.
func checkpointPlanEngine(t *testing.T, malicious ...bool) (*Engine, WorkOptions, string) {
	t.Helper()
	root, e, tr := setupProjectPlanTestRepo(t)
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Execution = contract.ExecutionConfig{Mode: contract.ExecutionIsolated, Backend: contract.BackendNative}
	cfg.PlanMode = "agent"
	cfg.Roles = map[string]string{}
	cfg.Roles["planner"] = "codex"
	cfg.Roles["reviewer"] = "claude"
	cfg.Security.IndependentReview = true
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, ".swarm/config.yaml"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, ".swarm/validation/required-checks.yaml"), []byte("required_checks:\n  - name: true\n    command: true\n    timeout: 1m\nprotected_paths: ['.swarm/**']\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(root, "add", ".")
	git(root, "commit", "-m", "isolated recovery policy")
	e.LookupEnv = func(key string) (string, bool) {
		if key == "SWARM_EXECUTION_MODE" {
			return "isolated", true
		}
		return os.LookupEnv(key)
	}
	original := prepareAgentDependencies
	prepareAgentDependencies = func(context.Context, contract.ExecutionConfig, string, string, sandbox.DependencyPolicy) (sandbox.Dependencies, error) {
		return sandbox.Dependencies{}, nil
	}
	t.Cleanup(func() { prepareAgentDependencies = original })
	wu := &contract.WorkUnit{Objective: "Recover preserved work", Constraints: []string{"Keep checkpoints"}, Acceptance: []string{"Review replacement"}, Workflow: "feature", Gate: contract.GateAgent}
	issue, err := tr.CreateIssue(t.Context(), "Recovery", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.LeaseManager.ApproveTask(t.Context(), issue.Number, "maintainer")
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.LeaseManager.Acquire(t.Context(), issue.Number, "maintainer", "codex", "plan", 4*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	path, branch, err := workspace.ProvisionAt(root, issue.Number, issue.Title, st.BaseSHA, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.LeaseManager.RecordCheckpoint(t.Context(), issue.Number, "maintainer", branch, st.BaseSHA); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "preserved.txt"), []byte("candidate source\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Malicious candidate instructions must not become active inputs.
	if len(malicious) > 0 && malicious[0] {
		if err = os.WriteFile(filepath.Join(path, "AGENTS.md"), []byte("CANDIDATE-INSTRUCTION-ATTACK\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(path, ".swarm/config.yaml"), []byte("CANDIDATE-POLICY-ATTACK\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git(path, "add", ".")
	git(path, "commit", "-m", "preserved candidate")
	candidate := git(path, "rev-parse", "HEAD")
	e.NewRunner = func(name string, _ *contract.SwarmConfig, _ bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			data, err := os.ReadFile(filepath.Join(req.WorktreePath, "preserved.txt"))
			if err != nil || string(data) != "candidate source\n" {
				t.Fatalf("wrong candidate: %s %v", data, err)
			}
			for _, file := range []string{"AGENTS.md", ".swarm/config.yaml", ".swarm/active/SYSTEM_PROMPT.md"} {
				data, err := os.ReadFile(filepath.Join(req.WorktreePath, file))
				if err != nil || strings.Contains(string(data), "CANDIDATE-") {
					t.Fatalf("untrusted inputs %s: %s %v", file, data, err)
				}
			}
			if req.Role == "planner" {
				if !req.ReadOnly || req.CheckImport == nil {
					t.Fatal("planner boundary not read-only/output checked")
				}
				for _, file := range []string{"PLAN.md", "TEST_PLAN.md"} {
					if err := os.WriteFile(filepath.Join(req.WorktreePath, ".swarm/active", file), []byte("Review preserved source and test it\n"), 0644); err != nil {
						return nil, err
					}
				}
			} else if req.Role == "reviewer" {
				if err := os.WriteFile(filepath.Join(req.WorktreePath, ".swarm/active-output/REVIEW.md"), []byte("VERDICT: APPROVED\n\nExact candidate reviewed.\n"), 0644); err != nil {
					return nil, err
				}
			} else {
				t.Fatalf("unexpected role %s", req.Role)
			}
			return &agent.RunResult{Attestation: contract.RoleAttestation{Role: req.Role, Agent: name, Harness: name, Model: "test", ExecutionMode: contract.ExecutionIsolated}}, nil
		}}, nil
	}
	return e, WorkOptions{PlanCheckpoint: true, IssueNumber: issue.Number, Owner: "maintainer", ClaimID: st.Lease.ClaimID, RepoRoot: root, Phase: "plan", Headless: true, ExpectedHead: st.BaseSHA, CandidateHead: candidate}, path
}

func TestCheckpointRecoveryPlanCycle(t *testing.T) {
	e, opts, path := checkpointPlanEngine(t)
	ordinary := opts
	ordinary.PlanCheckpoint = false
	lookup := e.LookupEnv
	e.LookupEnv = nil // ordinary mock preflight; the real workspace check must reject divergence
	ordinary.PlannerAgent = "mock"
	_, ordinaryErr := e.ExecuteWork(t.Context(), ordinary)
	e.LookupEnv = lookup
	ordinary.PlannerAgent = ""
	if ordinaryErr == nil || !strings.Contains(ordinaryErr.Error(), opts.CandidateHead) || !strings.Contains(ordinaryErr.Error(), opts.ExpectedHead) {
		t.Fatalf("ordinary plan did not reject exact checkpoint divergence: %v", ordinaryErr)
	}
	var events []ProgressEvent
	reporter := NewProgressReporterWithTicks(t.Context(), func(event ProgressEvent) { events = append(events, event) }, nil, time.Now)
	defer reporter.Close()
	opts.SetupProgress = reporter
	opts.OnProgress = func(ProgressEvent) { t.Fatal("used duplicate observer") }
	result, err := e.ExecuteWork(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	foundCompletion := func(stage string) bool {
		for _, event := range events {
			if event.IssueNumber == opts.IssueNumber && event.Stage == stage && event.Status == "completed" && event.StageElapsed > 0 {
				return true
			}
		}
		return false
	}
	if !foundCompletion("plan") {
		t.Fatalf("missing measured plan completion: %+v", events)
	}
	if result.State.HeadSHA != opts.ExpectedHead {
		t.Fatal("planning recorded candidate")
	}
	expected, candidate, present, err := lease.CheckpointPlanHeads(result.State)
	if err != nil || !present || expected != opts.ExpectedHead || candidate != opts.CandidateHead {
		t.Fatalf("binding: %s %s %v %v", expected, candidate, present, err)
	}
	for _, phase := range []string{"all", "plan", "implement", "test"} {
		ordinary.Phase = phase
		if _, err := e.ExecuteWork(t.Context(), ordinary); !errors.Is(err, lease.ErrApprovalRequired) {
			t.Fatalf("phase %s: %v", phase, err)
		}
	}
	ordinary.Phase = "review"
	ordinary.SetupProgress = reporter
	ordinary.OnProgress = opts.OnProgress
	result, err = e.ExecuteWork(t.Context(), ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if !foundCompletion("review") {
		t.Fatalf("missing measured review completion: %+v", events)
	}
	if result.PhaseCompleted != "review" || result.State.HeadSHA != opts.ExpectedHead {
		t.Fatal("review advanced checkpoint")
	}
	for _, a := range result.State.Artifacts {
		if a.Type == contract.ArtifactImplementation || a.Type == contract.ArtifactValidation {
			t.Fatal("planner/reviewer manufactured implementation evidence")
		}
	}
	if _, err = e.LeaseManager.ReconcileCheckpoint(t.Context(), opts.IssueNumber, opts.Owner, opts.ExpectedHead, opts.CandidateHead, false); err != nil {
		t.Fatal(err)
	}
	st, err := e.LeaseManager.GetCurrentState(t.Context(), opts.IssueNumber)
	if err != nil {
		t.Fatal(err)
	}
	if st.HeadSHA != opts.CandidateHead {
		t.Fatal("explicit reconcile lost candidate")
	}
	if _, _, err = e.LeaseManager.ProvisionIssueWorkspace(t.Context(), st, opts.Owner, "Recovery", "origin", st.Branch, false); err != nil {
		t.Fatal(err)
	}
	// Ordinary work must also finish a new implementation checkpoint, not only
	// provision the reconciled candidate. Host mocks here are explicit test mode.
	e.LookupEnv = nil
	e.NewRunner = nil
	ordinary.Phase = "implement"
	ordinary.OnProgress = nil
	ordinary.SetupProgress = nil
	ordinary.DeveloperAgent = "mock"
	ordinary.TesterAgent = "mock"
	result, err = e.ExecuteWork(t.Context(), ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ValidationPassed {
		t.Fatal("ordinary work did not resume validation")
	}
	if result.State.HeadSHA == opts.CandidateHead {
		t.Fatal("ordinary work did not create a new checkpoint")
	}
	if ancestor, err := workspace.BaseIsAncestor(t.Context(), e.repoRoot, opts.CandidateHead, result.State.HeadSHA); err != nil || !ancestor {
		t.Fatalf("resumed checkpoint lost preserved lineage: %v %v", ancestor, err)
	}

	data, err := os.ReadFile(filepath.Join(path, "preserved.txt"))
	if err != nil || string(data) != "candidate source\n" {
		t.Fatal("lost live source")
	}
}

func TestCheckpointRecoveryPlanIsolation(t *testing.T) {
	for _, attack := range []string{"source", "authority", "extra", "symlink", "missing", "dirty-live", "stale-token", "unavailable"} {
		t.Run(attack, func(t *testing.T) {
			e, opts, path := checkpointPlanEngine(t, true)
			original := e.NewRunner
			calls := 0
			e.NewRunner = func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error) {
				if attack == "unavailable" {
					return nil, fmt.Errorf("boundary unavailable")
				}
				runner, err := original(name, cfg, local)
				if err != nil {
					return nil, err
				}
				return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					calls++
					run, err := runner.Run(ctx, req)
					if err != nil {
						return run, err
					}
					target := ""
					switch attack {
					case "source":
						target = "preserved.txt"
					case "authority":
						target = ".swarm/active/WORKUNIT.md"
					case "extra":
						target = ".swarm/active/IMPLEMENTATION.md"
					case "symlink":
						file := filepath.Join(req.WorktreePath, ".swarm/active/PLAN.md")
						os.Remove(file)
						err = os.Symlink("../../preserved.txt", file)
					case "missing":
						err = os.Remove(filepath.Join(req.WorktreePath, ".swarm/active/TEST_PLAN.md"))
					case "dirty-live":
						err = os.WriteFile(filepath.Join(path, "preserved.txt"), []byte("raced"), 0644)
					case "stale-token":
						e.LeaseManager.BindClaim(opts.IssueNumber, "stale")
					}
					if target != "" {
						err = os.WriteFile(filepath.Join(req.WorktreePath, target), []byte("rejected"), 0644)
					}
					return run, err
				}}, nil
			}
			if _, err := e.ExecuteWork(t.Context(), opts); err == nil {
				t.Fatal("accepted " + attack)
			}
			st, err := e.LeaseManager.GetCurrentState(t.Context(), opts.IssueNumber)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Artifacts) != 0 || st.HeadSHA != opts.ExpectedHead {
				t.Fatal("rejected plan mutated authority")
			}
			if attack != "unavailable" && calls != 1 {
				t.Fatalf("attack not exercised: %d calls", calls)
			}
			if attack == "unavailable" && calls != 0 {
				t.Fatal("ran unavailable boundary")
			}
		})
	}
}

func TestCheckpointRecoveryPlanReviewFences(t *testing.T) {
	for _, scenario := range []string{"dirty-live", "plan-replaced", "same-planner", "human-gate", "reject-replan"} {
		t.Run(scenario, func(t *testing.T) {
			e, opts, path := checkpointPlanEngine(t)
			if _, err := e.ExecuteWork(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
			original := e.NewRunner
			if scenario == "human-gate" {
				if _, err := e.LeaseManager.RequireGate(t.Context(), opts.IssueNumber, opts.Owner, contract.GateHuman); err != nil {
					t.Fatal(err)
				}
			}
			e.NewRunner = func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error) {
				runner, err := original(name, cfg, local)
				if err != nil {
					return nil, err
				}
				return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					run, err := runner.Run(ctx, req)
					if err != nil || req.Role != "reviewer" {
						return run, err
					}
					switch scenario {
					case "dirty-live":
						err = os.WriteFile(filepath.Join(path, "preserved.txt"), []byte("raced"), 0644)
					case "plan-replaced":
						_, err = e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, contract.Artifact{Type: contract.ArtifactDesign, Producer: "test", Summary: "changed", Body: "replacement text"})
					case "same-planner":
						run.Attestation.Harness = "codex"
						run.Attestation.Agent = "codex"
					case "reject-replan":
						err = os.WriteFile(filepath.Join(req.WorktreePath, ".swarm/active-output/REVIEW.md"), []byte("VERDICT: CHANGES_REQUESTED\n\nRevise the plan.\n"), 0644)
					}
					return run, err
				}}, nil
			}
			review := opts
			review.PlanCheckpoint = false
			review.Phase = "review"
			if _, err := e.ExecuteWork(t.Context(), review); err == nil {
				t.Fatal("accepted " + scenario)
			}
			st, err := e.LeaseManager.GetCurrentState(t.Context(), opts.IssueNumber)
			if err != nil {
				t.Fatal(err)
			}
			if st.HeadSHA != opts.ExpectedHead {
				t.Fatal("review recorded candidate")
			}
			if scenario == "reject-replan" {
				if _, err = e.ExecuteWork(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "unchanged") {
					t.Fatalf("unchanged replan: %v", err)
				}
			}
			if scenario == "human-gate" {
				if _, err = e.LeaseManager.RecordHumanReview(t.Context(), opts.IssueNumber, "APPROVED", "self approval"); err == nil {
					t.Fatal("claim owner self approved")
				}
				tr := e.Tracker.(*tracker.LocalTracker).WithUser("independent-maintainer", "OWNER")
				mgr := lease.NewManager(tr, contract.SecurityConfig{})
				mgr.SetStore(e.repoRoot, coordination.NewFileStore(e.repoRoot))
				if _, err = mgr.RecordHumanReview(t.Context(), opts.IssueNumber, "APPROVED", "reviewed candidate"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCheckpointRecoveryPlanDelegatedPlanner(t *testing.T) {
	e, opts, _ := checkpointPlanEngine(t, true)
	opts.PlannerAgent = "claude"
	if _, err := e.ExecuteWork(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	opts.PlanCheckpoint = false
	opts.Phase = "review"
	opts.ReviewerAgent = "codex"
	if _, err := e.ExecuteWork(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRecoveryProgressHeartbeat(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			e, opts, _ := checkpointPlanEngine(t)
			ticks := make(chan time.Time)
			events := make(chan ProgressEvent, 100)
			reporter := NewProgressReporterWithTicks(t.Context(), func(event ProgressEvent) { events <- event }, ticks, time.Now)
			defer reporter.Close()
			opts.SetupProgress = reporter
			original := e.NewRunner
			sentinel := errors.New("fixture planner failed")
			e.NewRunner = func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error) {
				runner, err := original(name, cfg, local)
				if err != nil {
					return nil, err
				}
				return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					select {
					case ticks <- time.Now():
					case <-time.After(5 * time.Second):
						t.Fatal("heartbeat blocked")
					}
					for {
						select {
						case event := <-events:
							if event.Stage == opts.Phase && event.Status == "running" {
								if fail {
									return nil, sentinel
								}
								return runner.Run(ctx, req)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("missing role heartbeat")
						}
					}
				}}, nil
			}
			_, err := e.ExecuteWork(t.Context(), opts)
			if fail && !errors.Is(err, sentinel) || !fail && err != nil {
				t.Fatal(err)
			}
			reporter.Close()
			want := "completed"
			if fail {
				want = "failed"
			}
			found := false
			for len(events) > 0 {
				event := <-events
				if event.Stage == "plan" && event.Status == want && event.StageElapsed > 0 {
					found = true
				}
			}
			if !found {
				t.Fatal("missing measured terminal event")
			}
		})
	}
}
