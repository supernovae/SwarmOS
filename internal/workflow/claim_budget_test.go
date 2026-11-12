package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/validation"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/claimpause"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

func budgetEngine(t *testing.T) (*Engine, *tracker.Issue) {
	t.Helper()
	root, e, tr := setupProjectPlanTestRepo(t)
	configureValidation(t, root)
	if out, err := gitfixture.Command(t, "", "-C", root, "commit", "-am", "test validation").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	wu := &contract.WorkUnit{Objective: "Preserve work", Constraints: []string{"Keep fencing"}, Acceptance: []string{"Resume checkpoint"}, Workflow: "feature", Gate: contract.GateNone}
	issue, err := tr.CreateIssue(t.Context(), "Budget", contract.FormatIssueMarkdown(wu), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.LeaseManager.ApproveTask(t.Context(), issue.Number, "maintainer"); err != nil {
		t.Fatal(err)
	}
	return e, issue
}

type budgetTestRunner struct {
	agent.MockRunner
	run func(context.Context, agent.RunRequest) (*agent.RunResult, error)
}

func (r *budgetTestRunner) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	return r.run(ctx, req)
}

type workBudgetTimer struct{ ch chan time.Time }

func (t *workBudgetTimer) C() <-chan time.Time { return t.ch }
func (t *workBudgetTimer) Stop() bool          { return true }

func TestStageBudgetAdmissionBoundaries(t *testing.T) {
	for _, phase := range []string{"plan", "review", "implement"} {
		for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
			t.Run(phase+offset.String(), func(t *testing.T) {
				e, issue := budgetEngine(t)
				base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				now := base
				e.LeaseManager.SetNowFunc(func() time.Time { return now })
				st, err := e.LeaseManager.Acquire(t.Context(), issue.Number, "maintainer", "mock", phase, 4*time.Hour, 0)
				if err != nil {
					t.Fatal(err)
				}
				now = st.Lease.ExpiresAt.Add(-4 * time.Hour / 3).Add(-offset)
				err = e.admitStage(t.Context(), issue.Number, "maintainer", phase)
				var paused *budgetAdmissionError
				if offset < 0 {
					if !errors.As(err, &paused) || paused.state.Lease.Status != "handoff" || paused.state.Lease.NextPhase != phase {
						t.Fatalf("admission: %v %+v", err, paused)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestBudgetPauseCheckpointsAndResumes(t *testing.T) {
	for _, role := range []string{"developer", "tester"} {
		for _, backend := range []string{"local", "git"} {
			t.Run(role+"/"+backend, func(t *testing.T) {
				e, issue := budgetEngine(t)
				if backend == "git" {
					remote := t.TempDir()
					for _, args := range [][]string{{"init", "--bare", "-b", "main", remote}, {"-C", e.repoRoot, "remote", "add", "origin", remote}, {"-C", e.repoRoot, "push", "origin", "main"}} {
						if out, err := gitfixture.Command(t, "", args...).CombinedOutput(); err != nil {
							t.Fatalf("%v: %s", err, out)
						}
					}
					raw, _, err := e.LeaseManager.Store().Load(t.Context(), issue.Number)
					if err != nil {
						t.Fatal(err)
					}
					store := coordination.NewGitStore(e.repoRoot)
					if _, err := store.CompareAndSwap(t.Context(), issue.Number, "", raw); err != nil {
						t.Fatal(err)
					}
					e.LeaseManager.SetStore(e.repoRoot, store)
				}
				base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				var elapsed atomic.Int64
				e.LeaseManager.SetNowFunc(func() time.Time { return base.Add(time.Duration(elapsed.Load())) })
				timers := make(chan *workBudgetTimer, 100)
				e.LeaseManager.SetHeartbeatTimerFunc(func(time.Duration) lease.HeartbeatTimer {
					tt := &workBudgetTimer{make(chan time.Time, 1)}
					timers <- tt
					return tt
				})
				mock := &agent.MockRunner{}
				stopped := false
				e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
					return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
						if req.Role != role {
							return mock.Run(ctx, req)
						}
						if role == "developer" {
							if err := os.WriteFile(filepath.Join(req.WorktreePath, "partial.go"), []byte("package partial\n"), 0600); err != nil {
								return nil, err
							}
						}
						timer := <-timers
						for range 3 {
							if _, err := e.LeaseManager.Renew(t.Context(), issue.Number, "maintainer", 4*time.Hour); err != nil {
								return nil, err
							}
						}
						elapsed.Store(int64(4*time.Hour - 4*time.Hour/3))
						timer.ch <- base.Add(time.Duration(elapsed.Load()))
						<-ctx.Done()
						stopped = true
						return nil, ctx.Err()
					}}, nil
				}
				opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true}
				res, err := e.ExecuteWork(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				if !stopped || res.ClaimBudgetPause == nil || res.Outcome() != "paused-claim-budget" || res.State.Lease.Status != "handoff" || res.State.Lease.NextPhase != "implement" || res.ValidationPassed {
					t.Fatalf("pause: %+v", res)
				}
				for _, a := range res.State.Artifacts {
					if a.Verdict == "FAIL" || a.Type == contract.ArtifactValidation {
						t.Fatalf("interruption verdict: %+v", a)
					}
				}
				if role == "developer" {
					out, err := gitfixture.Command(t, "", "-C", e.repoRoot, "show", res.HeadSHA+":partial.go").CombinedOutput()
					if err != nil || string(out) != "package partial\n" {
						t.Fatalf("checkpoint lost edits: %s %v", out, err)
					}
				}
				if backend == "git" {
					out, err := gitfixture.Command(t, "", "-C", e.repoRoot, "ls-remote", "origin", "refs/heads/"+res.BranchName).CombinedOutput()
					if err != nil || !strings.HasPrefix(string(out), res.HeadSHA+"\t") {
						t.Fatalf("unpublished checkpoint: %s %v", out, err)
					}
				}
				devCalls := 0
				e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
					return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
						if req.Role == "developer" {
							devCalls++
						}
						return mock.Run(ctx, req)
					}}, nil
				}
				resumed, err := e.ExecuteWork(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				if !resumed.ValidationPassed || resumed.ClaimBudgetPause != nil {
					t.Fatalf("resume: %+v", resumed)
				}
				if role == "tester" && devCalls != 0 {
					t.Fatalf("proven implementation reran developer %d times", devCalls)
				}
				if role == "developer" && devCalls != 1 {
					t.Fatalf("partial implementation bypassed developer: %d", devCalls)
				}
				if !strings.Contains(res.ClaimBudgetPause.ContinueCommand, "issue work 1 --phase implement") {
					t.Fatal(res.ClaimBudgetPause.ContinueCommand)
				}
			})
		}
	}
}

func TestBudgetPauseStillRequiresFence(t *testing.T) {
	e, issue := budgetEngine(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.LeaseManager.SetNowFunc(func() time.Time { return now })
	st, err := e.LeaseManager.Acquire(t.Context(), issue.Number, "maintainer", "mock", "implement", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	now = st.Lease.ExpiresAt
	res := &WorkResult{IssueNumber: issue.Number}
	err = e.pauseWork(t.Context(), res, WorkOptions{IssueNumber: issue.Number, Owner: "maintainer"}, nil, "implement", false, false, "")
	if !errors.Is(err, lease.ErrClaimFence) || res.ClaimBudgetPause != nil {
		t.Fatalf("expired pause: %v %+v", err, res)
	}
}

func TestPreStageBudgetDoesNotStartPlanner(t *testing.T) {
	e, issue := budgetEngine(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	e.LeaseManager.SetNowFunc(func() time.Time { return now })
	st, err := e.LeaseManager.Acquire(t.Context(), issue.Number, "maintainer", "mock", "plan", 4*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	now = base.Add(3 * time.Hour)
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		t.Fatal("stage constructed a runner below reserve")
		return nil, nil
	}
	res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", ClaimID: st.Lease.ClaimID, AgentName: "mock", Phase: "plan", Headless: true})
	if err != nil || res.ClaimBudgetPause == nil || res.State.Lease.NextPhase != "plan" {
		t.Fatalf("pre-stage pause: %+v %v", res, err)
	}
	if len(res.State.Artifacts) != 0 {
		t.Fatalf("admission produced artifacts: %+v", res.State.Artifacts)
	}
}

func TestBudgetPauseDuringRequiredChecksDoesNotRecordFailure(t *testing.T) {
	e, issue := budgetEngine(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64
	e.LeaseManager.SetNowFunc(func() time.Time { return base.Add(time.Duration(elapsed.Load())) })
	timers := make(chan *workBudgetTimer, 100)
	e.LeaseManager.SetHeartbeatTimerFunc(func(time.Duration) lease.HeartbeatTimer {
		timer := &workBudgetTimer{make(chan time.Time, 1)}
		timers <- timer
		return timer
	})
	var agentCtx context.Context
	mock := &agent.MockRunner{}
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			agentCtx = ctx
			return mock.Run(ctx, req)
		}}, nil
	}
	opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true, OnProgress: func(event ProgressEvent) {
		if event.Stage != "required-checks" || event.Status != "started" || event.Check != "" {
			return
		}
		timer := <-timers
		for range 3 {
			if _, err := e.LeaseManager.Renew(t.Context(), issue.Number, "maintainer", 4*time.Hour); err != nil {
				t.Fatal(err)
			}
		}
		elapsed.Store(int64(4*time.Hour - 4*time.Hour/3))
		timer.ch <- base.Add(time.Duration(elapsed.Load()))
		<-agentCtx.Done()
	}}
	res, err := e.ExecuteWork(t.Context(), opts)
	if err != nil || res.ClaimBudgetPause == nil || res.ValidationPassed || res.Validation != nil {
		t.Fatalf("checks pause: %+v %v", res, err)
	}
	for _, a := range res.State.Artifacts {
		if a.Type == contract.ArtifactValidation || a.Verdict == "FAIL" {
			t.Fatalf("canceled check verdict: %+v", a)
		}
	}
}

func TestDirectReviewBudgetRefusalReturnsHandoffAndRecovery(t *testing.T) {
	e, issue := budgetEngine(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.LeaseManager.SetNowFunc(func() time.Time { return now })
	st, err := e.LeaseManager.Acquire(t.Context(), issue.Number, "maintainer", "mock", "review", 4*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Hour)
	wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		t.Fatal(err)
	}
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		t.Fatal("reviewer started below reserve")
		return nil, nil
	}
	reviewCtx, cancel := context.WithCancelCause(t.Context())
	reviewCtx = claimpause.WithParent(reviewCtx, t.Context())
	cancel(claimpause.ErrBudget)
	verdict, updated, err := e.RunAgentReview(reviewCtx, "", wu, nil, st, "maintainer", "mock", "headless", "", 123)
	if verdict != "" || err == nil || updated.Lease.Status != "handoff" || !strings.Contains(err.Error(), "swarm pr work 123 --phase review") {
		t.Fatalf("review admission: %q %+v %v", verdict, updated, err)
	}
}

func TestChangedTesterCheckpointCannotReuseImplementationEvidence(t *testing.T) {
	e, issue := budgetEngine(t)
	opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true}
	completed, err := e.ExecuteWork(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(completed.WorktreePath, "tester-edit.go"), []byte("package changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		t.Fatal(err)
	}
	head, err := workspace.Checkpoint(t.Context(), completed.WorktreePath, completed.BranchName, workspace.CheckpointOptions{WorkUnit: wu, Purpose: "implement"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.LeaseManager.RecordCheckpoint(t.Context(), issue.Number, "maintainer", completed.BranchName, head); err != nil {
		t.Fatal(err)
	}
	if _, err = e.LeaseManager.Handoff(t.Context(), issue.Number, "maintainer", "implement", "implement", "claim budget"); err != nil {
		t.Fatal(err)
	}
	devCalls, testerCalls := 0, 0
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(_ context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			if req.Role == "developer" {
				devCalls++
			}
			if req.Role == "tester" {
				testerCalls++
			}
			return &agent.RunResult{}, nil
		}}, nil
	}
	resumed, err := e.ExecuteWork(t.Context(), opts)
	if err == nil || !strings.Contains(err.Error(), "no implementation beyond starting checkpoint") || devCalls != 1 || testerCalls != 0 || resumed.ValidationPassed {
		t.Fatalf("stale evidence bypassed completion: %+v calls=%d/%d %v", resumed, devCalls, testerCalls, err)
	}
}

type handoffExpiryStore struct {
	coordination.Store
	expire func()
}

func (s *handoffExpiryStore) CompareAndSwap(ctx context.Context, issue int, revision string, data []byte) (string, error) {
	var state lease.IssueState
	if err := json.Unmarshal(data, &state); err != nil {
		return "", err
	}
	if state.Lease.Status == "handoff" {
		s.expire()
	}
	return s.Store.CompareAndSwap(ctx, issue, revision, data)
}

func TestBudgetCleanupExpiryDuringHandoffRemainsFenceError(t *testing.T) {
	e, issue := budgetEngine(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.LeaseManager.SetNowFunc(func() time.Time { return now })
	st, err := e.LeaseManager.Acquire(t.Context(), issue.Number, "maintainer", "mock", "implement", 4*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.LeaseManager.SetStore(e.repoRoot, &handoffExpiryStore{Store: e.LeaseManager.Store(), expire: func() { now = st.Lease.ExpiresAt }})
	res := &WorkResult{IssueNumber: issue.Number}
	err = e.pauseWork(t.Context(), res, WorkOptions{IssueNumber: issue.Number, Owner: "maintainer"}, nil, "implement", false, false, "")
	if !errors.Is(err, lease.ErrClaimFence) || res.ClaimBudgetPause != nil {
		t.Fatalf("expired handoff became a successful pause: %+v %v", res, err)
	}
}

// legacyBudgetGateway is an upstream coordinator that predates claim budgets:
// it fences handoffs through a real manager but stores no projection.
type legacyBudgetGateway struct {
	t     *testing.T
	coord *lease.Manager
	owner string
}

func (g *legacyBudgetGateway) Apply(ctx context.Context, r lease.ForkRequest) (*lease.IssueState, error) {
	if r.Operation != "handoff" {
		return nil, errors.New("unexpected " + r.Operation)
	}
	g.coord.BindClaim(r.IssueNumber, r.ClaimID)
	if _, err := g.coord.Handoff(ctx, r.IssueNumber, g.owner, r.Phase, r.NextPhase, r.Reason); err != nil {
		return nil, err
	}
	stripStoredClaimBudget(g.t, g.coord, r.IssueNumber)
	return g.coord.GetCurrentState(ctx, r.IssueNumber)
}

func stripStoredClaimBudget(t *testing.T, m *lease.Manager, n int) {
	t.Helper()
	raw, rev, err := m.Store().Load(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err = json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	delete(st, "claim_budget")
	if raw, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Store().CompareAndSwap(context.Background(), n, rev, raw); err != nil {
		t.Fatal(err)
	}
}

// legacyForkEngine claims issue for 4h with no renewals and returns a
// fork-mode engine whose coordinator sends no claim budget.
func legacyForkEngine(t *testing.T, e *Engine, issue int, phase string, now func() time.Time) (*Engine, *lease.IssueState) {
	t.Helper()
	e.LeaseManager.SetNowFunc(now)
	st, err := e.LeaseManager.Acquire(t.Context(), issue, "maintainer", "mock", phase, 4*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	stripStoredClaimBudget(t, e.LeaseManager, issue)
	worker := lease.NewManager(e.Tracker, e.Config.Security)
	worker.SetStore(e.repoRoot, e.LeaseManager.Store())
	worker.SetNowFunc(now)
	worker.SetWorkerGateway(&legacyBudgetGateway{t: t, coord: e.LeaseManager, owner: "maintainer"})
	worker.BindClaim(issue, st.Lease.ClaimID)
	return NewEngineWithManager(e.repoRoot, e.Tracker, worker, e.Config, true, ""), st
}

func TestForkStageAdmissionWithoutUpstreamBudgetUsesLeaseExpiry(t *testing.T) {
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			e, issue := budgetEngine(t)
			reserve, err := time.ParseDuration(e.Config.Lease.Duration)
			if err != nil {
				t.Fatal(err)
			}
			reserve /= 3
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			fork, st := legacyForkEngine(t, e, issue.Number, "plan", func() time.Time { return now })
			now = st.Lease.ExpiresAt.Add(-reserve).Add(-offset)
			err = fork.admitStage(t.Context(), issue.Number, "maintainer", "plan")
			if offset >= 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var paused *budgetAdmissionError
			if !errors.As(err, &paused) || paused.state.Lease.Status != "handoff" || paused.state.Lease.NextPhase != "plan" {
				t.Fatalf("admission: %v %+v", err, paused)
			}
			if !strings.Contains(err.Error(), "available before lease expiry") || strings.Contains(err.Error(), "renewals") {
				t.Fatalf("fallback reason: %v", err)
			}
		})
	}
}

func TestForkBudgetPauseWithoutUpstreamBudgetKeepsFence(t *testing.T) {
	e, issue := budgetEngine(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	fork, st := legacyForkEngine(t, e, issue.Number, "implement", func() time.Time { return now })
	opts := WorkOptions{IssueNumber: issue.Number, Owner: "maintainer"}

	now = st.Lease.ExpiresAt
	res := &WorkResult{IssueNumber: issue.Number}
	if err := fork.pauseWork(t.Context(), res, opts, nil, "implement", false, false, ""); !errors.Is(err, lease.ErrClaimFence) || res.ClaimBudgetPause != nil {
		t.Fatalf("expired pause: %v %+v", err, res)
	}

	now = base.Add(3 * time.Hour)
	if err := fork.pauseWork(t.Context(), res, opts, nil, "implement", false, false, ""); err != nil {
		t.Fatal(err)
	}
	p := res.ClaimBudgetPause
	if p == nil || p.Budget != nil || !strings.HasPrefix(p.Reason, "paused for claim budget: 1h0m0s available before lease expiry (reserve ") || !strings.HasSuffix(p.Reason, "upstream coordinator supplied no renewal budget") || res.State.Lease.Status != "handoff" {
		t.Fatalf("fallback pause: %+v %+v", p, res.State)
	}
	if strings.Contains(p.Reason, "renewals") {
		t.Fatalf("fallback pause claims renewal counts: %s", p.Reason)
	}
}

func TestSuppliedBudgetReasonUnchanged(t *testing.T) {
	t.Parallel()
	b := &lease.ClaimBudget{Available: time.Hour, Threshold: 20 * time.Minute, RenewalsUsed: 2, RenewalsRemaining: 1}
	if got, want := (&budgetAdmissionError{budget: b}).budgetReason(), "paused for claim budget: 1h0m0s available (reserve 20m0s), 2 renewals used, 1 remaining"; got != want {
		t.Fatalf("reason %q, want %q", got, want)
	}
}

func TestIssue466ValidationReusePreview(t *testing.T) {
	e, issue := budgetEngine(t)
	st, err := e.LeaseManager.GetCurrentState(t.Context(), issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	st.Lease.Owner, st.Lease.ClaimID, st.Lease.Status = "owner", "claim", "active"
	now := time.Now().UTC()
	st.Lease.ExpiresAt = now.Add(time.Hour)
	st.HeadSHA = st.BaseSHA
	wt := workspace.WorktreePath(e.repoRoot, issue.Number)
	if err := os.MkdirAll(filepath.Dir(wt), 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := gitfixture.Command(t, e.repoRoot, "worktree", "add", "-b", "preview", wt, st.HeadSHA).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	read := func(args ...string) []byte {
		t.Helper()
		out, err := gitfixture.Command(t, e.repoRoot, args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	tree := strings.TrimSpace(string(read("rev-parse", st.HeadSHA+"^{tree}")))
	policy := sha256.Sum256(read("show", st.BaseSHA+":.swarm/validation/required-checks.yaml"))
	report, err := json.Marshal(validation.ValidationReport{Passed: true})
	if err != nil {
		t.Fatal(err)
	}
	original := contract.Artifact{Type: contract.ArtifactValidation, Verdict: "PASS", CreatedAt: now,
		Metadata: map[string]string{"validation_trusted_origin": "coordinator", "validation_trusted_owner": "owner",
			"validation_claim": "claim", "validation_trusted_claim": "claim", "validation_base": st.BaseSHA,
			"validation_version": "v1.2.3", "validation_tree": tree, "validation_policy": hex.EncodeToString(policy[:]), "validation_report": string(report)}}
	cfg, err := harness.LoadConfigAt(e.repoRoot, st.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"same claim", "cross claim", "stale", "version", "base", "tree", "policy", "owner", "missing", "malformed", "changed candidate"} {
		t.Run(kind, func(t *testing.T) {
			a := original
			a.Metadata = map[string]string{}
			for k, v := range original.Metadata {
				a.Metadata[k] = v
			}
			version, owner := "v1.2.3", "owner"
			st.Lease.ClaimID = "claim"
			switch kind {
			case "cross claim":
				st.Lease.ClaimID = "new"
			case "stale":
				st.Lease.ClaimID = "new"
				a.CreatedAt = now.Add(-365 * 24 * time.Hour)
			case "version":
				version = "dev"
			case "base":
				a.Metadata["validation_base"] = "other"
			case "tree":
				a.Metadata["validation_tree"] = "other"
			case "policy":
				a.Metadata["validation_policy"] = "other"
			case "owner":
				owner = "other"
			case "malformed":
				a.Metadata["validation_report"] = "{"
			case "changed candidate":
				if err := os.WriteFile(filepath.Join(wt, "untracked.txt"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(filepath.Join(wt, "untracked.txt"))
			}
			st.Artifacts = []contract.Artifact{a}
			if kind == "missing" {
				st.Artifacts = nil
			}
			p := InspectContinuation(st, wu, WorkOptions{Phase: "all", Version: version}, owner, "claim", now)
			if p.ValidationReuse == "conditional" {
				inspectValidationReuse(t.Context(), e.repoRoot, st, cfg, p, now)
			}
			want := "unavailable"
			if kind == "same claim" {
				want = "eligible"
			}
			if kind == "cross claim" {
				want = "conditional"
			}
			if p.ValidationReuse != want {
				t.Fatalf("reuse: %+v want %s", p, want)
			}
			if kind == "cross claim" && p.AcceptanceEvidence != "tester required after claim replacement" {
				t.Fatal("cross-claim acceptance incorrectly reused")
			}
		})
	}
}

// claimLoadStore counts actual state reads and can install a successor after a
// detached admission snapshot has been captured, or during a CAS conflict.
type claimLoadStore struct {
	coordination.Store
	loads   atomic.Int64
	hook    func(context.Context, int, []byte, string, string)
	casHook func(context.Context, int, string, []byte) (string, error)
}

func claimLoadPath() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var names []string
	for {
		f, more := frames.Next()
		names = append(names, f.Function)
		if !more {
			break
		}
	}
	return strings.Join(names, "\n")
}
func (s *claimLoadStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	s.loads.Add(1)
	raw, rev, err := s.Store.Load(ctx, n)
	if err == nil && s.hook != nil {
		s.hook(ctx, n, append([]byte(nil), raw...), rev, claimLoadPath())
	}
	return raw, rev, err
}
func (s *claimLoadStore) CompareAndSwap(ctx context.Context, n int, rev string, raw []byte) (string, error) {
	if s.casHook != nil {
		return s.casHook(ctx, n, rev, raw)
	}
	return s.Store.CompareAndSwap(ctx, n, rev, raw)
}
func claimReviewFixture(t *testing.T) (*Engine, *contract.WorkUnit, *lease.IssueState, string) {
	t.Helper()
	e, issue := budgetEngine(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.LeaseManager.SetNowFunc(func() time.Time { return now })
	_, err := e.LeaseManager.Acquire(t.Context(), issue.Number, "maintainer", "mock", "plan", 4*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.LeaseManager.RecordPlanPair(t.Context(), issue.Number, "maintainer", contract.Artifact{Type: contract.ArtifactDesign, Producer: "fixture", Summary: "plan", Body: "plan"}, contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "fixture", Summary: "tests", Body: "tests"})
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		t.Fatal(err)
	}
	wt := workspace.WorktreePath(e.repoRoot, issue.Number)
	if err := os.MkdirAll(filepath.Dir(wt), 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := gitfixture.Command(t, e.repoRoot, "worktree", "add", "-b", "claim-review", wt, st.BaseSHA).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return e, wu, st, wt
}
func TestClaimAdmissionLoadCounts(t *testing.T) {
	for _, review := range []bool{false, true} {
		name := "admitStage"
		if review {
			name = "RunAgentReview"
		}
		t.Run(name, func(t *testing.T) {
			e, wu, st, wt := claimReviewFixture(t)
			store := &claimLoadStore{Store: e.LeaseManager.Store()}
			e.LeaseManager.SetStore(e.repoRoot, store)
			if !review {
				if err := e.admitStage(t.Context(), wu.IssueNumber, "maintainer", "review"); err != nil {
					t.Fatal(err)
				}
				if got := store.loads.Load(); got != 1 {
					t.Fatalf("admission loads = %d, want 1", got)
				}
				return
			}
			sentinel := errors.New("review launched")
			var launched atomic.Int64
			e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &budgetTestRunner{run: func(context.Context, agent.RunRequest) (*agent.RunResult, error) {
					launched.Add(1)
					state, _, loadErr := store.Store.Load(t.Context(), wu.IssueNumber)
					var phaseState lease.IssueState
					if loadErr != nil {
						t.Fatal(loadErr)
					}
					if err := json.Unmarshal(state, &phaseState); err != nil {
						t.Fatal(err)
					}
					if phaseState.Lease.Phase != "review" {
						t.Errorf("phase write missing: %s", phaseState.Lease.Phase)
					}

					if got := store.loads.Load(); got != 3 {
						t.Errorf("review startup loads = %d, want 3", got)
					}
					return nil, sentinel
				}}, nil
			}
			_, _, err := e.RunAgentReview(t.Context(), wt, wu, nil, st, "maintainer", "mock", "headless", "", 0)
			if launched.Load() != 1 || !errors.Is(err, sentinel) {
				t.Fatalf("launches=%d error=%v", launched.Load(), err)
			}
		})
	}
}
func TestClaimFenceAfterAdmission(t *testing.T) {
	for _, mode := range []string{"RunAgentReview", "UpdatePhase", "CASRetry"} {
		t.Run(mode, func(t *testing.T) {
			e, wu, st, wt := claimReviewFixture(t)
			underlying := e.LeaseManager.Store()
			store := &claimLoadStore{Store: underlying}
			var successor []byte
			var paths []string
			takeovers, attempts := 0, 0
			install := func(ctx context.Context, n int, raw []byte, rev string) {
				var next lease.IssueState
				if err := json.Unmarshal(raw, &next); err != nil {
					t.Fatal(err)
				}
				next.Lease.ClaimID = "successor-token"
				data, err := json.Marshal(next)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := underlying.CompareAndSwap(ctx, n, rev, data); err != nil {
					t.Fatal(err)
				}
				successor, _, err = underlying.Load(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				takeovers++
			}
			store.hook = func(ctx context.Context, n int, raw []byte, rev, path string) {
				paths = append(paths, path)
				if mode != "CASRetry" && takeovers == 0 && strings.Contains(path, ".admitStage") {
					install(ctx, n, raw, rev)
				}
			}
			store.casHook = func(ctx context.Context, n int, rev string, raw []byte) (string, error) {
				attempts++
				if mode != "CASRetry" {
					t.Error("worker attempted CAS after takeover")
					return "", coordination.ErrConflict
				}
				if attempts == 1 {
					old, current, err := underlying.Load(ctx, n)
					if err != nil {
						t.Fatal(err)
					}
					install(ctx, n, old, current)
					return "", coordination.ErrConflict
				}
				t.Error("worker retried CAS after takeover")
				return "", coordination.ErrConflict
			}
			e.LeaseManager.SetStore(e.repoRoot, store)
			var launches atomic.Int64
			e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &budgetTestRunner{run: func(context.Context, agent.RunRequest) (*agent.RunResult, error) {
					launches.Add(1)
					return nil, errors.New("unexpected launch")
				}}, nil
			}
			var err error
			if mode == "RunAgentReview" {
				_, _, err = e.RunAgentReview(t.Context(), wt, wu, nil, st, "maintainer", "mock", "headless", "", 0)
			} else {
				if err = e.admitStage(t.Context(), wu.IssueNumber, "maintainer", "review"); err != nil {
					t.Fatal(err)
				}
				_, err = e.LeaseManager.UpdatePhase(t.Context(), wu.IssueNumber, "maintainer", "review", "active")
			}
			if !errors.Is(err, lease.ErrClaimFence) || launches.Load() != 0 || takeovers != 1 {
				t.Fatalf("err=%v launches=%d takeovers=%d", err, launches.Load(), takeovers)
			}
			if len(paths) < 2 || !strings.Contains(paths[1], ".mutate") || !strings.Contains(paths[1], ".UpdatePhase") {
				t.Fatalf("next load must be fenced phase write; paths: %v", paths)
			}
			if mode != "CASRetry" && (len(paths) != 2 || attempts != 0) {
				t.Fatalf("loads=%d attempts=%d", len(paths), attempts)
			}
			if mode == "CASRetry" && (len(paths) != 3 || attempts != 1 || !strings.Contains(paths[2], ".mutate")) {
				t.Fatalf("retry loads=%d attempts=%d", len(paths), attempts)
			}
			after, _, loadErr := underlying.Load(t.Context(), wu.IssueNumber)
			if loadErr != nil || !bytes.Equal(after, successor) {
				t.Fatalf("successor changed: %v", loadErr)
			}
		})
	}
}
