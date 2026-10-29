package workflow

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/progress"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type activityEngineStore struct {
	coordination.Store
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *activityEngineStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	if s.armed.Load() && progress.ActivityFrom(ctx) != nil {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	return s.Store.Load(ctx, n)
}
func TestSlowReviewThenStalledStateFetchHeartbeats(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, observe := range []bool{false, true} {
			for _, canceled := range []bool{false, true} {
				t.Run(fmt.Sprintf("legacy=%v/observe=%v/cancel=%v", legacy, observe, canceled), func(t *testing.T) {
					e, issue := budgetEngine(t)
					opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "plan", Headless: true}
					planned, err := e.ExecuteWork(t.Context(), opts)
					if err != nil {
						t.Fatal(err)
					}
					opts.ClaimID = planned.State.Lease.ClaimID
					opts.Phase = "review"
					ctx, cancel := context.WithCancel(t.Context())
					base := time.Now()
					var seconds atomic.Int64
					clock := func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
					ticks := make(chan time.Time, 1)
					events := make(chan ProgressEvent, 100)
					opts.OnProgress = func(ev ProgressEvent) { events <- ev }
					var reporter *workProgress
					if legacy {
						reporter = newSetupProgress(ctx, 0, opts.OnProgress, ticks)
						reporter.now = clock
						reporter.started = base
						opts.SetupProgress = reporter
					} else {
						e.progressTicks = ticks
						e.progressNow = clock
					}
					store := &activityEngineStore{Store: e.LeaseManager.Store(), entered: make(chan struct{}), release: make(chan struct{})}
					e.LeaseManager.SetStore(e.repoRoot, store)
					entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
					var roleOnce, loadOnce sync.Once
					var workErr error
					t.Cleanup(func() {
						cancel()
						roleOnce.Do(func() { close(release) })
						loadOnce.Do(func() { close(store.release) })
						activityWait(t, done)
						if reporter != nil {
							reporter.Close()
						}
					})
					e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
						return &budgetTestRunner{run: func(runctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
							result, err := (&agent.MockRunner{}).Run(runctx, req)
							if err != nil {
								return result, err
							}
							end := progress.ActivityFrom(runctx).Role(req.Role, "mock", clock())
							defer end()
							if observe {
								progress.ActivityFrom(runctx).Observe(clock())
							}
							close(entered)
							select {
							case <-release:
							case <-runctx.Done():
								return nil, runctx.Err()
							}
							store.armed.Store(true)
							return result, nil
						}}, nil
					}
					go func() { defer close(done); _, workErr = e.ExecuteWork(ctx, opts) }()
					running := func() ProgressEvent {
						t.Helper()
						for {
							select {
							case ev := <-events:
								if ev.Status == "running" {
									return ev
								}
							case <-done:
								t.Fatalf("worker ended early: %v", workErr)
								return ProgressEvent{}
							case <-time.After(5 * time.Second):
								t.Fatal("heartbeat timeout")
								return ProgressEvent{}
							}
						}
					}
					activityWait(t, entered)
					seconds.Store(12)
					ticks <- clock()
					ev := running()
					if ev.Stage != "review" || ev.Elapsed != 12*time.Second || ev.Role != "reviewer" || ev.ActivityKnown != observe || (observe && ev.ActivityAge != 12*time.Second) {
						t.Fatal(ev)
					}
					roleOnce.Do(func() { close(release) })
					activityWait(t, store.entered)
					previous := time.Duration(0)
					for _, sec := range []int64{30, 45} {
						seconds.Store(sec)
						ticks <- clock()
						ev = running()
						if ev.Stage != "review" || ev.Elapsed != time.Duration(sec)*time.Second || ev.Operation != "state read" || ev.Role != "" || ev.OperationElapsed <= previous {
							t.Fatal(ev)
						}
						previous = ev.OperationElapsed
					}
					if canceled {
						cancel()
					} else {
						loadOnce.Do(func() { close(store.release) })
					}
					activityWait(t, done)
					if canceled && !errors.Is(workErr, context.Canceled) {
						t.Fatalf("cancellation: %v", workErr)
					}
					if !canceled && workErr != nil {
						t.Fatal(workErr)
					}
					if reporter != nil {
						reporter.Close()
						activityWait(t, reporter.done)
					}
					for len(events) > 0 {
						if ev := <-events; ev.Status == "running" {
							t.Fatal("late heartbeat", ev)
						}
					}
					ticks <- clock()
					if len(ticks) != 1 {
						t.Fatal("tick consumed after finish")
					}
				})
			}
		}
	}
}

func TestTesterRetryProgressUsesExistingSession(t *testing.T) {
	e, issue := budgetEngine(t)
	first, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []ProgressEvent
	ticks := make(chan time.Time)
	e.progressTicks = ticks
	retry, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", ClaimID: first.State.Lease.ClaimID, AgentName: "mock", Phase: "test", Headless: true, OnProgress: func(ev ProgressEvent) { mu.Lock(); events = append(events, ev); mu.Unlock() }})
	if err != nil || !retry.ValidationPassed {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	mu.Lock()
	defer mu.Unlock()
	testIndex, checkIndex, finishes := -1, -1, 0
	for i, ev := range events {
		if ev.Stage == "implement" || ev.Role == "developer" {
			t.Fatalf("developer progress: %+v", ev)
		}
		if ev.Stage == "test" && testIndex < 0 {
			testIndex = i
		}
		if ev.Stage == "required-checks" && checkIndex < 0 {
			checkIndex = i
		}
		if ev.Status == "completed" && ev.Stage == "required-checks" && ev.Check == "" {
			finishes++
		}
	}
	if len(events) == 0 || events[0].Stage != "preflight" || testIndex <= 0 || checkIndex <= testIndex || finishes != 1 {
		t.Fatalf("events: %+v", events)
	}
}
