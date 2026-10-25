package fork

import (
	"context"
	"testing"
	"time"
)

func TestForkProgressCancellationAndNilObserver(t *testing.T) {
	p := startRequestProgress(t.Context(), "renew", 100, nil, nil, nil)
	p.finish("accepted")
	p.stop()
	if p.reporter != nil {
		t.Fatal("unobserved request started worker")
	}
	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time, 1)
	events := make(chan ProgressEvent, 4)
	p = startRequestProgress(ctx, "renew", 100, func(e ProgressEvent) { events <- e }, ticks, nil)
	if e := <-events; e.Status != "posted" {
		t.Fatal(e)
	}
	cancel()
	select {
	case <-p.reporter.Done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop worker")
	}
	p.stop()
	ticks <- time.Time{}
	select {
	case e := <-events:
		t.Fatal("late event", e)
	default:
	}
}

func TestForkProgressTerminalFollowsHeartbeat(t *testing.T) {
	ticks := make(chan time.Time, 1)
	events := make(chan ProgressEvent, 8)
	start := time.Now()
	var elapsed time.Duration
	// The channel handshake orders clock changes before each read.
	p := startRequestProgress(t.Context(), "renew", 100, func(e ProgressEvent) { events <- e }, ticks, func() time.Time { return start.Add(elapsed) })
	<-events
	elapsed = 15 * time.Second
	ticks <- time.Time{}
	if e := <-events; e.Status != "waiting" || e.Elapsed != elapsed {
		t.Fatal(e)
	}
	elapsed = 21 * time.Second
	p.finish("accepted")
	if e := <-events; e.Status != "accepted" || e.Elapsed != elapsed {
		t.Fatal(e)
	}
	ticks <- time.Time{}
	p.stop()
	select {
	case e := <-events:
		t.Fatal("event after terminal", e)
	default:
	}
}
