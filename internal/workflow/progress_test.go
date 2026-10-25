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

func TestSetupProgressOwnsHeartbeatAndRestoresStage(t *testing.T) {
	for _, stage := range []string{"plan", "implement", "review", "test"} {
		t.Run(stage, func(t *testing.T) {
			ticks := make(chan time.Time)
			events := make(chan ProgressEvent, 30)
			p := newWorkProgress(t.Context(), 42, func(e ProgressEvent) { events <- e }, ticks)
			p.shared = true
			defer p.Close()
			receiveProgress(t, events, "preflight", "started")
			p.next(stage)
			receiveProgress(t, events, "preflight", "completed")
			receiveProgress(t, events, stage, "started")
			sentinel := errors.New("operation failure")
			err := p.ReportSetupPhase(t.Context(), 42, "bundle", func() error {
				receiveProgress(t, events, "bundle", "started")
				ticks <- time.Now()
				receiveProgress(t, events, "bundle", "running")
				return sentinel
			})
			if err != sentinel {
				t.Fatalf("changed operation error: %v", err)
			}
			ticks <- time.Now()
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
	ticks := make(chan time.Time)
	events := make(chan ProgressEvent, 30)
	p := newWorkProgress(t.Context(), 42, func(e ProgressEvent) { events <- e }, ticks)
	defer p.Close()
	receiveProgress(t, events, "preflight", "started")
	// Stop the real timer: injected ticks and clock drive deadlines deterministically.
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	now := time.Now()
	p.mu.Lock()
	p.timer = timer
	p.now = func() time.Time { return now }
	p.mu.Unlock()
	p.next("plan")
	receiveProgress(t, events, "preflight", "completed")
	receiveProgress(t, events, "plan", "started")
	p.mu.Lock()
	now = now.Add(14 * time.Second)
	p.mu.Unlock()
	p.ReportSetupPhase(t.Context(), 42, "bundle", func() error {
		receiveProgress(t, events, "bundle", "started")
		p.mu.Lock()
		now = now.Add(time.Second)
		p.mu.Unlock()
		p.heartbeat(t.Context()) // Old plan deadline cannot cause an early heartbeat.
		select {
		case e := <-events:
			t.Fatalf("early heartbeat: %+v", e)
		default:
		}
		p.mu.Lock()
		now = now.Add(14 * time.Second)
		p.mu.Unlock()
		p.heartbeat(t.Context())
		event := receiveProgress(t, events, "bundle", "running")
		if event.StageElapsed != 15*time.Second {
			t.Fatalf("phase elapsed: %s", event.StageElapsed)
		}
		return nil
	})
	p.mu.Lock()
	remaining := p.deadline.Sub(now)
	p.mu.Unlock()
	if remaining != 15*time.Second {
		t.Fatalf("restored deadline: %s", remaining)
	}
	p.Close()
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
