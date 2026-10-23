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
