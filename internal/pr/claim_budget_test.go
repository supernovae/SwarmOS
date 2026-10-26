package pr

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workflow"
)

type pausePRRunner struct {
	agent.MockRunner
	run func(context.Context, agent.RunRequest) (*agent.RunResult, error)
}

func (r *pausePRRunner) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	return r.run(ctx, req)
}

type prBudgetTimer struct{ ch chan time.Time }

func (t *prBudgetTimer) C() <-chan time.Time { return t.ch }
func (t *prBudgetTimer) Stop() bool          { return true }

func TestPRClaimBudgetResumesUnpublishedCheckpoint(t *testing.T) {
	for _, role := range []string{"developer", "tester"} {
		t.Run(role, func(t *testing.T) {
			s, issue, number, published := fixture(t)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var elapsed atomic.Int64
			s.Now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
			s.Manager.SetNowFunc(s.Now)
			timers := make(chan *prBudgetTimer, 100)
			s.Manager.SetHeartbeatTimerFunc(func(time.Duration) lease.HeartbeatTimer {
				timer := &prBudgetTimer{make(chan time.Time, 1)}
				timers <- timer
				return timer
			})
			mock := &agent.MockRunner{}
			s.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &pausePRRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
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
						if _, err := s.Manager.Renew(t.Context(), issue, "alice", 4*time.Hour); err != nil {
							return nil, err
						}
					}
					elapsed.Store(int64(4*time.Hour - 4*time.Hour/3))
					timer.ch <- base.Add(time.Duration(elapsed.Load()))
					<-ctx.Done()
					return nil, ctx.Err()
				}}, nil
			}
			opts := Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "all", Headless: true}}
			paused, err := s.Work(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if paused.Work == nil || paused.Work.ClaimBudgetPause == nil || paused.State.PRRevision == nil || paused.State.PRRevision.LocalHeadSHA == published || paused.State.PRRevision.RemoteHeadSHA != published {
				t.Fatalf("pause: %+v", paused)
			}
			saved := paused.State.PRRevision.LocalHeadSHA
			remote, err := s.Tracker.GetPullRequest(t.Context(), number)
			if err != nil || remote.HeadSHA != published {
				t.Fatalf("pause published PR: %+v %v", remote, err)
			}
			devCalls := 0
			s.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &pausePRRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					if req.Role == "developer" {
						devCalls++
					}
					if role == "developer" && req.Role == "developer" {
						data, err := os.ReadFile(filepath.Join(req.WorktreePath, "partial.go"))
						if err != nil || string(data) != "package partial\n" {
							t.Fatalf("resume lost saved work: %q %v", data, err)
						}
					}
					return mock.Run(ctx, req)
				}}, nil
			}
			resumed, err := s.Work(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if !resumed.Work.ValidationPassed || resumed.State.PRRevision.RemoteHeadSHA != published || (role == "developer" && resumed.State.PRRevision.LocalHeadSHA == saved) || (role == "tester" && (devCalls != 0 || resumed.State.PRRevision.LocalHeadSHA != saved)) {
				t.Fatalf("resume: %+v", resumed)
			}
			remote, err = s.Tracker.GetPullRequest(t.Context(), number)
			if err != nil || remote.HeadSHA != published {
				t.Fatalf("resume published PR: %+v %v", remote, err)
			}

		})
	}
}
