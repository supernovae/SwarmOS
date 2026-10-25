package progress

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckHeartbeatAndShutdown(t *testing.T) {
	ticks := make(chan time.Time)
	events := make(chan Event, 30)
	var seconds atomic.Int64
	clock := func() time.Time { return time.Unix(seconds.Load(), 0) }
	resets := 0
	p := New(t.Context(), 7, 9, func(e Event) { events <- e }, ticks, clock, func() { resets++ })
	receive := func(check, status string, elapsed time.Duration) {
		t.Helper()
		select {
		case e := <-events:
			if e.Check != check || e.Status != status || e.StageElapsed != elapsed {
				t.Fatalf("event %+v, want %q %s %s", e, check, status, elapsed)
			}
		case <-time.After(time.Second):
			t.Fatal("missing event")
		}
	}
	receive("", "started", 0)
	p.Next("validation")
	receive("", "completed", 0)
	receive("", "started", 0)
	seconds.Store(3)
	p.Observe(Event{Check: "trusted", Status: "started"})
	receive("trusted", "started", 0)
	for _, sec := range []int64{15, 30, 45} {
		seconds.Store(3 + sec)
		ticks <- clock()
		receive("trusted", "running", time.Duration(sec)*time.Second)
	}
	p.Observe(Event{Check: "trusted", Status: "PASS"})
	receive("trusted", "PASS", 45*time.Second)
	ticks <- clock()
	receive("", "running", 48*time.Second)
	p.Observe(Event{Check: "second", Status: "started"})
	receive("second", "started", 0)
	// A queued tick from the previous check cannot heartbeat the next one.
	ticks <- time.Unix(33, 0)
	seconds.Store(63)
	ticks <- clock()
	receive("second", "running", 15*time.Second)
	p.Observe(Event{Check: "second", Status: "FAIL"})
	receive("second", "FAIL", 15*time.Second)
	p.Finish("failed")
	receive("", "failed", 63*time.Second)
	p.Next("late")
	p.Observe(Event{Check: "late", Status: "started"})
	p.Finish("completed")
	select {
	case e := <-events:
		t.Fatalf("late callback %+v", e)
	default:
	}
	if resets != 6 {
		t.Fatalf("timer reset count %d", resets)
	}
}

func TestObserverCanInspectStateAndCancellationJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var p *Session
	p = New(ctx, 1, 0, func(e Event) {
		if p != nil {
			_ = p.Snapshot()
		}
	}, nil, time.Now, nil)
	p.Next("validation")
	cancel()
	select {
	case <-p.Done():
	case <-time.After(time.Second):
		t.Fatal("timer survived cancellation")
	}
	p.Finish("canceled")
}
func TestNilObserverStartsNoTimer(t *testing.T) {
	p := Start(t.Context(), 1, 0, nil)
	p.Next("validation")
	p.Observe(Event{})
	p.Finish("completed")
	if p.Done() != nil {
		t.Fatal("quiet session started timer")
	}
}

func TestProgressLifecycle(t *testing.T) {
	if Interval != 15*time.Second {
		t.Fatal(Interval)
	}
	ticks := make(chan time.Time, 1)
	events := make(chan time.Duration, 4)
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	p := NewReporter(t.Context(), func(d time.Duration) { events <- d }, ticks, clock)
	select {
	case <-events:
		t.Fatal("immediate heartbeat")
	default:
	}
	for _, elapsed := range []time.Duration{15 * time.Second, 30 * time.Second} {
		mu.Lock()
		now = now.Add(15 * time.Second)
		mu.Unlock()
		ticks <- time.Time{}
		select {
		case got := <-events:
			if got != elapsed {
				t.Fatal(got)
			}
		case <-time.After(time.Second):
			t.Fatal("missing heartbeat")
		}
	}
	p.Stop()
	p.Stop()
	ticks <- time.Time{}
	select {
	case <-events:
		t.Fatal("late heartbeat")
	default:
	}
}

func TestProgressShutdown(t *testing.T) {
	for _, cancelWorker := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		ticks := make(chan time.Time)
		p := NewReporter(ctx, func(time.Duration) {}, ticks, nil)
		if cancelWorker {
			cancel()
		} else {
			close(ticks)
		}
		select {
		case <-p.Done:
		case <-time.After(time.Second):
			t.Fatal("worker still running")
		}
		p.Stop()
		cancel()
	}
	p := NewReporter(t.Context(), nil, nil, nil)
	p.Do(func(time.Duration) { t.Fatal("nil observer") })
	p.Stop()
	if p.Done != nil {
		t.Fatal("nil observer started worker")
	}
}

func TestProgressSerializesCallbacks(t *testing.T) {
	ticks := make(chan time.Time, 100)
	var events []time.Duration
	p := NewReporter(t.Context(), func(d time.Duration) { events = append(events, d) }, ticks, nil)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for range 100 {
				p.Do(func(d time.Duration) { events = append(events, d) })
				select {
				case ticks <- time.Time{}:
				default:
				}
			}
		})
	}
	wg.Wait()
	p.Stop()
	p.Do(func(d time.Duration) { events = append(events, d) })
	for i := 1; i < len(events); i++ {
		if events[i] < events[i-1] {
			t.Fatal("reordered events")
		}
	}
}
