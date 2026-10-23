package lease

import (
	"context"
	"sync"
	"time"
)

// Heartbeat renews a fenced claim while long-running work holds it. A failed
// renewal cancels Context so the work stops before it can outlive its lease.
type Heartbeat struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

// StartHeartbeat renews the bound claim every third of duration (at least once
// per second) until Stop is called or a renewal fails.
func (m *Manager) StartHeartbeat(parent context.Context, issue int, owner string, duration time.Duration) *Heartbeat {
	ctx, cancel := context.WithCancel(parent)
	h := &Heartbeat{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	interval := max(duration/3, time.Second)
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := m.Renew(ctx, issue, owner, duration); err != nil {
					if ctx.Err() == nil {
						h.mu.Lock()
						h.err = err
						h.mu.Unlock()
					}
					cancel()
					return
				}
			}
		}
	}()
	return h
}

// Context is canceled when the heartbeat stops or a renewal fails.
func (h *Heartbeat) Context() context.Context { return h.ctx }

// Err reports the renewal failure that canceled Context, if any.
func (h *Heartbeat) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// Stop ends renewal and waits for the renewal goroutine to exit.
func (h *Heartbeat) Stop() {
	h.cancel()
	<-h.done
}
