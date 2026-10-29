package workflow

import (
	"context"
	"github.com/supernovae/swarmos/internal/progress"
	"testing"
	"time"
)

func activityWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("completion timed out")
	}
}
func TestActivityDoesNotIncreaseEventRate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time, 1)
	events := make(chan ProgressEvent, 10)
	p := newSetupProgress(ctx, 1, func(e ProgressEvent) { events <- e }, ticks)
	t.Cleanup(func() { cancel(); p.Close() })
	<-events
	a := p.activityTracker()
	end := a.Role("reviewer", "mock", time.Now())
	defer end()
	for range 10000 {
		a.Observe(time.Now())
	}
	for range 1000 {
		progress.Begin(progress.WithActivity(ctx, a), progress.OpStateRead, time.Now())()
	}
	ticks <- time.Now()
	select {
	case e := <-events:
		if e.Status != "running" {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing tick")
	}
	if len(events) != 0 {
		t.Fatal("extra events")
	}
	cancel()
	activityWait(t, p.done)
	finished := make(chan struct{})
	go func() { p.Close(); close(finished) }()
	activityWait(t, finished)
	ticks <- time.Now()
	a.Observe(time.Now())
	if len(ticks) != 1 || len(events) != 0 {
		t.Fatal("late events")
	}
}
