package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	if out, err := exec.Command("git", "-C", root, "commit", "-am", "test validation").CombinedOutput(); err != nil {
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
						if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
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
					out, err := exec.Command("git", "-C", e.repoRoot, "show", res.HeadSHA+":partial.go").CombinedOutput()
					if err != nil || string(out) != "package partial\n" {
						t.Fatalf("checkpoint lost edits: %s %v", out, err)
					}
				}
				if backend == "git" {
					out, err := exec.Command("git", "-C", e.repoRoot, "ls-remote", "origin", "refs/heads/"+res.BranchName).CombinedOutput()
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
	if err != nil || res.ClaimBudgetPause == nil || res.ValidationPassed {
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
