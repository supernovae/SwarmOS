package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func receiveProgress(t *testing.T, events <-chan ProgressEvent, stage, status string) ProgressEvent {
	t.Helper()
	select {
	case event := <-events:
		if event.Stage != stage || event.Status != status || event.IssueNumber != 42 || event.Elapsed < 0 || event.StageElapsed < 0 {
			t.Fatalf("unexpected progress: %+v, want %s %s", event, stage, status)
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatalf("missing %s %s", stage, status)
		return ProgressEvent{}
	}
}

func TestWorkProgressQuietStageHeartbeatAndShutdown(t *testing.T) {
	ticks := make(chan time.Time, 1)
	events := make(chan ProgressEvent, 20)
	p := newWorkProgress(t.Context(), 42, func(e ProgressEvent) { events <- e }, ticks)
	receiveProgress(t, events, "preflight", "started")
	p.next("plan")
	receiveProgress(t, events, "preflight", "completed")
	receiveProgress(t, events, "plan", "started")
	// A quiet child needs no output to produce periodic local feedback.
	ticks <- time.Now()
	receiveProgress(t, events, "plan", "running")
	p.finish(&WorkResult{PhaseCompleted: "plan"}, nil)
	receiveProgress(t, events, "plan", "completed")
	select {
	case <-p.done:
	default:
		t.Fatal("timer still running after finish")
	}
	ticks <- time.Now()
	select {
	case e := <-events:
		t.Fatalf("late callback: %+v", e)
	default:
	}
}

func TestWorkProgressTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *WorkResult
		err    error
		want   string
	}{
		{"failure", nil, errors.New("private diagnostics"), "failed"},
		{"canceled", nil, context.Canceled, "canceled"},
		{"pause", &WorkResult{PausedForHuman: true}, nil, "paused"},
		{"handoff", &WorkResult{HandedOffToPhase: "review"}, nil, "handed-off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			events := make(chan ProgressEvent, 10)
			p := newWorkProgress(ctx, 42, func(e ProgressEvent) { events <- e }, nil)
			receiveProgress(t, events, "preflight", "started")
			if tc.err == context.Canceled {
				cancel()
				select {
				case <-p.done:
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation did not stop timer")
				}
			}
			p.finish(tc.result, tc.err)
			receiveProgress(t, events, "preflight", tc.want)
		})
	}
}

func TestWorkProgressSerializesCallbacks(t *testing.T) {
	ticks := make(chan time.Time, 100)
	// Deliberately no lock: the race detector verifies the callback contract.
	var events []ProgressEvent
	p := newWorkProgress(t.Context(), 42, func(e ProgressEvent) { events = append(events, e) }, ticks)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for range 100 {
			ticks <- time.Now()
		}
	}()
	go func() {
		defer workers.Done()
		for range 100 {
			p.next("plan")
		}
	}()
	workers.Wait()
	p.finish(nil, nil)
	for i := 1; i < len(events); i++ {
		if events[i].Elapsed < events[i-1].Elapsed {
			t.Fatal("events reordered")
		}
	}
}

func TestWorkProgressWithoutObserver(t *testing.T) {
	p := startWorkProgress(t.Context(), 42, nil)
	p.next("plan")
	p.finish(nil, nil)
	if p.done != nil {
		t.Fatal("unobserved work started a timer")
	}
}

// setupProgressFixture uses the command reporter path, with a real timer only
// for reset/re-arm coverage. Its channel never drives test heartbeats.
func setupProgressFixture(t *testing.T) (*workProgress, chan ProgressEvent, func(time.Duration)) {
	t.Helper()
	ticks := make(chan time.Time)
	events := make(chan ProgressEvent, 30)
	reads := make(chan struct{}, 30)
	p := newSetupProgress(t.Context(), 0, func(e ProgressEvent) { events <- e }, ticks)
	now := time.Now()
	p.mu.Lock()
	p.shared = true
	p.timer = time.NewTimer(time.Hour)
	p.started = now
	p.now = func() time.Time { reads <- struct{}{}; return now }
	p.mu.Unlock()
	t.Cleanup(p.Close)
	return p, events, func(delta time.Duration) {
		p.mu.Lock()
		now = now.Add(delta)
		for len(reads) > 0 {
			<-reads
		}
		p.mu.Unlock()
		ticks <- now
		select {
		case <-reads:
		case <-time.After(5 * time.Second):
			t.Fatal("tick was not processed")
		}
		// The clock is read under the lock; acquiring it joins heartbeat processing.
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			t.Fatal("reporter closed before tick completed")
		}
	}
}

func TestSetupProgressOwnsHeartbeatAndRestoresStage(t *testing.T) {
	for _, stage := range []string{"plan", "implement", "review", "test"} {
		t.Run(stage, func(t *testing.T) {
			p, events, tick := setupProgressFixture(t)
			p.mu.Lock()
			p.issue = 42
			p.mu.Unlock()
			p.next(stage)
			receiveProgress(t, events, stage, "started")
			sentinel := errors.New("operation failure")
			err := p.ReportSetupPhase(t.Context(), 42, "bundle", func() error {
				receiveProgress(t, events, "bundle", "started")
				tick(15 * time.Second)
				receiveProgress(t, events, "bundle", "running")
				return sentinel
			})
			if err != sentinel {
				t.Fatalf("changed operation error: %v", err)
			}
			tick(15 * time.Second)
			receiveProgress(t, events, stage, "running")
			p.finish(nil, err)
			receiveProgress(t, events, stage, "failed")
			p.Close()
			if err := p.ReportSetupPhase(t.Context(), 42, "claim", func() error { return sentinel }); err != sentinel {
				t.Fatal(err)
			}
			select {
			case event := <-events:
				t.Fatalf("late event: %+v", event)
			default:
			}
		})
	}
}

func TestSetupProgressFreshDeadline(t *testing.T) {
	for _, phase := range []string{"claim", "workspace", "bundle"} {
		t.Run(phase, func(t *testing.T) {
			p, events, tick := setupProgressFixture(t)
			p.mu.Lock()
			p.issue = 42
			p.mu.Unlock()
			p.next("plan")
			receiveProgress(t, events, "plan", "started")
			tick(14 * time.Second)
			p.ReportSetupPhase(t.Context(), 42, phase, func() error {
				receiveProgress(t, events, phase, "started")
				tick(14 * time.Second)
				select {
				case e := <-events:
					t.Fatalf("early heartbeat: %+v", e)
				default:
				}
				tick(time.Second)
				if e := receiveProgress(t, events, phase, "running"); e.StageElapsed != 15*time.Second {
					t.Fatal(e)
				}
				tick(15 * time.Second)
				if e := receiveProgress(t, events, phase, "running"); e.StageElapsed != 30*time.Second {
					t.Fatal(e)
				}
				return nil
			})
			p.mu.Lock()
			remaining := p.deadline.Sub(p.now())
			p.mu.Unlock()
			if remaining != 15*time.Second {
				t.Fatalf("restored deadline: %s", remaining)
			}
			tick(14 * time.Second)
			select {
			case e := <-events:
				t.Fatalf("early restored heartbeat: %+v", e)
			default:
			}
			tick(time.Second)
			receiveProgress(t, events, "plan", "running")
			p.Close()
			select {
			case <-p.done:
			default:
				t.Fatal("heartbeat goroutine not joined")
			}
		})
	}
}

func TestSetupProgressCanceledSessionStillRunsOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var events []ProgressEvent
	p := NewProgressReporter(ctx, func(e ProgressEvent) { events = append(events, e) })
	cancel()
	<-p.done
	called := false
	p.ReportSetupPhase(t.Context(), 42, "claim", func() error { called = true; return nil })
	p.Close()
	if !called || len(events) != 0 {
		t.Fatalf("called=%v events=%v", called, events)
	}
}

func TestSetupProgressChildCancellationSuppressesHeartbeat(t *testing.T) {
	events := make(chan ProgressEvent, 10)
	p := NewProgressReporter(t.Context(), func(event ProgressEvent) { events <- event })
	defer p.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.ReportSetupPhase(ctx, 42, "bundle", func() error {
		receiveProgress(t, events, "bundle", "started")
		cancel()
		p.mu.Lock()
		p.deadline = time.Time{}
		p.mu.Unlock()
		p.heartbeat(t.Context())
		select {
		case event := <-events:
			t.Fatalf("heartbeat after phase cancellation: %+v", event)
		default:
		}
		return ctx.Err()
	})
}

func TestWorkProgressObservesChecks(t *testing.T) {
	expectCheck := func(t *testing.T, events <-chan ProgressEvent, check, status string) {
		t.Helper()
		if event := receiveProgress(t, events, "required-checks", status); event.Check != check {
			t.Fatalf("check %q, want %q", event.Check, check)
		}
	}
	t.Run("session", func(t *testing.T) {
		events := make(chan ProgressEvent, 30)
		p := newWorkProgress(t.Context(), 42, func(e ProgressEvent) { events <- e }, make(chan time.Time))
		receiveProgress(t, events, "preflight", "started")
		p.next("required-checks")
		receiveProgress(t, events, "preflight", "completed")
		receiveProgress(t, events, "required-checks", "started")
		for _, check := range []string{"test (changed packages)", "test"} {
			p.observeCheck(ProgressEvent{Check: check, Status: "started"})
			expectCheck(t, events, check, "started")
			p.observeCheck(ProgressEvent{Check: check, Status: "PASS"})
			expectCheck(t, events, check, "PASS")
		}
		p.finish(nil, nil)
		expectCheck(t, events, "", "completed")
	})
	t.Run("shared reporter", func(t *testing.T) {
		p, events, tick := setupProgressFixture(t)
		p.mu.Lock()
		p.issue = 42
		p.mu.Unlock()
		p.next("required-checks")
		receiveProgress(t, events, "required-checks", "started")
		p.observeCheck(ProgressEvent{Check: "test (changed packages)", Status: "started"})
		expectCheck(t, events, "test (changed packages)", "started")
		tick(15 * time.Second)
		expectCheck(t, events, "test (changed packages)", "running")
		p.observeCheck(ProgressEvent{Check: "test (changed packages)", Status: "FAIL"})
		expectCheck(t, events, "test (changed packages)", "FAIL")
		tick(15 * time.Second)
		expectCheck(t, events, "", "running")
		p.Close()
		p.observeCheck(ProgressEvent{Check: "test", Status: "started"})
		select {
		case event := <-events:
			t.Fatalf("event after close: %+v", event)
		default:
		}
	})
	t.Run("no observer", func(t *testing.T) {
		startWorkProgress(t.Context(), 42, nil).observeCheck(ProgressEvent{Check: "test", Status: "started"})
		(&workProgress{}).observeCheck(ProgressEvent{Check: "test", Status: "started"})
	})
}
