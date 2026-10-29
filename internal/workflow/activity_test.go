package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/progress"
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
	for _, cancelFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelFirst), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			ticks := make(chan time.Time, 1)
			events := make(chan ProgressEvent, 10)
			p := newSetupProgress(ctx, 1, func(e ProgressEvent) { events <- e }, ticks)
			t.Cleanup(func() { cancel(); activityWait(t, p.done); p.Close() })
			select {
			case e := <-events:
				if e.Role != "" || e.Operation != "" {
					t.Fatal(e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("missing start")
			}
			a := p.activityTracker()
			end := a.Role("reviewer", "mock", time.Now())
			defer end()
			for range 2 {
				for range 10000 {
					a.Observe(time.Now())
				}
				for range 1000 {
					progress.Begin(progress.WithActivity(ctx, a), progress.OpStateRead, time.Now())()
				}
				ticks <- time.Now()
				select {
				case e := <-events:
					if e.Status != "running" || e.Role != "reviewer" || !e.ActivityKnown {
						t.Fatal(e)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("missing tick")
				}
				if len(events) != 0 {
					t.Fatal("extra events")
				}
			}
			if cancelFirst {
				cancel()
				activityWait(t, p.done)
			}
			finished := make(chan struct{})
			go func() { p.Close(); close(finished) }()
			activityWait(t, finished)
			activityWait(t, p.done)
			a.Observe(time.Now())
			progress.Begin(progress.WithActivity(ctx, a), progress.OpStateRead, time.Now())()
			e := progress.Event{Status: "running"}
			a.Enrich(&e, time.Now())
			ticks <- time.Now()
			if len(ticks) != 1 || len(events) != 0 {
				t.Fatal("late events")
			}
		})
	}
}
