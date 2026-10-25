package progress

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestProgressLifecycle(t *testing.T) {
	if Interval != 15*time.Second {
		t.Fatal(Interval)
	}
	ticks := make(chan time.Time, 1)
	events := make(chan time.Duration, 4)
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	p := New(t.Context(), func(d time.Duration) { events <- d }, ticks, clock)
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
		p := New(ctx, func(time.Duration) {}, ticks, nil)
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
	p := New(t.Context(), nil, nil, nil)
	p.Do(func(time.Duration) { t.Fatal("nil observer") })
	p.Stop()
	if p.Done != nil {
		t.Fatal("nil observer started worker")
	}
}

func TestProgressSerializesCallbacks(t *testing.T) {
	ticks := make(chan time.Time, 100)
	var events []time.Duration
	p := New(t.Context(), func(d time.Duration) { events = append(events, d) }, ticks, nil)
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
