package progress

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestActivityRoleObservedAge(t *testing.T) {
	a := NewActivity()
	now := time.Unix(100, 0)
	end := a.Role("reviewer", "claude", now)
	defer end()
	a.Writer(func() time.Time { return now }).Write([]byte("secret prompt ghp_token"))
	e := Event{Status: "running"}
	a.Enrich(&e, now.Add(12*time.Second))
	if e.Role != "reviewer" || e.Tool != "claude" || !e.ActivityKnown || e.ActivityAge != 12*time.Second {
		t.Fatal(e)
	}
	if strings.Contains(fmt.Sprint(a), "secret") {
		t.Fatal("retained output")
	}
	transition := Event{Status: "completed"}
	a.Enrich(&transition, now)
	if transition.Role != "" {
		t.Fatal(transition)
	}
}
func TestActivityRoleWithoutObservationIsUnknown(t *testing.T) {
	a := NewActivity()
	now := time.Now()
	old := a.Role("reviewer", "claude", now)
	a.Observe(now)
	end := a.Role("developer", "mock", now)
	old()
	old()
	e := Event{Status: "running"}
	a.Enrich(&e, now)
	if e.Role != "developer" || e.ActivityKnown {
		t.Fatal(e)
	}
	end()
	end()
	a.Enrich(&e, now)
	if e.Role != "" {
		t.Fatal(e)
	}
}
func TestActivityAllowlist(t *testing.T) {
	for _, bad := range []string{"reviewer --token=ghp_x", "../../etc", "\n\x1b[31m", "", "git push https://x:tok@h"} {
		a := NewActivity()
		now := time.Now()
		end := a.Role(bad, bad, now)
		ctx := WithActivity(t.Context(), a)
		stop := Begin(ctx, Operation(bad), now)
		e := Event{Status: "running"}
		a.Enrich(&e, now)
		if e.Role != "agent" || e.Tool != "harness" || e.Operation != "" {
			t.Fatal(e)
		}
		stop()
		end()
	}
	a := NewActivity()
	a.role = "hostile"
	a.tool = "hostile"
	e := Event{Status: "running"}
	a.Enrich(&e, time.Now())
	if e.Role != "" {
		t.Fatal(e)
	}
}
func TestActivityConcurrentOpsAndLateEnd(t *testing.T) {
	a := NewActivity()
	ctx := WithActivity(t.Context(), a)
	now := time.Now()
	first := Begin(ctx, OpStateRead, now)
	second := Begin(ctx, OpStateWrite, now)
	first()
	first()
	e := Event{Status: "running"}
	a.Enrich(&e, now.Add(time.Second))
	if e.Operation != string(OpStateWrite) {
		t.Fatal(e)
	}
	second()
	ends := []func(){}
	for range 5 {
		ends = append(ends, Begin(ctx, OpCheckpoint, now))
	}
	n := 0
	for _, op := range a.ops {
		if op.id != 0 {
			n++
		}
	}
	if n != 4 {
		t.Fatal(n)
	}
	for _, end := range ends {
		end()
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 100 {
				end := Begin(ctx, OpStateRead, now)
				a.Observe(now)
				a.Enrich(&Event{Status: "running"}, now)
				end()
				end()
			}
		})
	}
	wg.Wait()
	if ActivityFrom(context.Background()) != nil || ActivityFrom(WithActivity(ctx, nil)) != nil {
		t.Fatal("mask failed")
	}
	ActivityFrom(context.Background()).Role("reviewer", "mock", now)()
	ActivityFrom(context.Background()).Writer(time.Now).Write([]byte("discard"))
}
func activityWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("completion timed out")
	}
}
func TestActivityReporterCancellationCleanup(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelFirst), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			ticks := make(chan time.Time, 1)
			events := make(chan Event, 10)
			now := time.Unix(100, 0)
			p := New(ctx, 1, 0, func(e Event) { events <- e }, ticks, func() time.Time { return now }, nil)
			t.Cleanup(func() { cancel(); activityWait(t, p.Done()); p.Finish("canceled") })
			select {
			case <-events:
			case <-time.After(5 * time.Second):
				t.Fatal("missing start")
			}
			end := p.Activity().Role("reviewer", "mock", now)
			defer end()
			for range 2 {
				for range 10000 {
					p.Activity().Observe(now)
				}
				for range 1000 {
					Begin(WithActivity(ctx, p.Activity()), OpStateRead, now)()
				}
				ticks <- now
				select {
				case e := <-events:
					if e.Status != "running" || e.Role != "reviewer" {
						t.Fatal(e)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("missing tick")
				}
				if len(events) != 0 {
					t.Fatal("mutations emitted events")
				}
			}
			if cancelFirst {
				cancel()
				activityWait(t, p.Done())
			}
			finished := make(chan struct{})
			go func() { p.Finish("completed"); close(finished) }()
			activityWait(t, finished)
			activityWait(t, p.Done())
			select {
			case e := <-events:
				if e.Role != "" || e.Tool != "" || e.Operation != "" || e.ActivityKnown || e.ActivityAge != 0 || e.OperationElapsed != 0 {
					t.Fatal("activity on terminal event", e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("missing terminal event")
			}
			p.Activity().Observe(now)
			Begin(WithActivity(ctx, p.Activity()), OpStateRead, now)()
			e := Event{Status: "running"}
			p.Activity().Enrich(&e, now)
			ticks <- now
			if len(ticks) != 1 || len(events) != 0 {
				t.Fatal("events after completion")
			}
		})
	}
	if Interval != 15*time.Second {
		t.Fatal(Interval)
	}
}
