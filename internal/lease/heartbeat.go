package lease

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/supernovae/swarmos/internal/claimpause"
)

// Heartbeat renews a fenced claim while long-running work holds it. A failed
// renewal cancels Context so the work stops before it can outlive its lease.
type Heartbeat struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

// StartHeartbeat renews the bound claim every third of duration (at least once
// per second) until Stop is called or a renewal fails.
func (m *Manager) StartHeartbeat(parent context.Context, issue int, owner string, duration time.Duration) *Heartbeat {
	ctx, cancel := context.WithCancelCause(parent)
	ctx = claimpause.WithParent(ctx, parent)
	h := &Heartbeat{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	interval := max(duration/3, time.Second)
	go func() {
		defer close(h.done)
		st, err := m.GetCurrentState(ctx, issue)
		for {
			if err == nil {
				err = m.CheckClaim(ctx, issue, owner)
			}
			var b *ClaimBudget
			if err == nil {
				b, err = m.Budget(st)
			}
			if err != nil {
				h.fail(err)
				return
			}
			delay := interval
			if b != nil {
				if b.Available <= b.Threshold {
					h.fail(claimpause.ErrBudget)
					return
				}
				delay = min(delay, b.Available-b.Threshold)
			}
			timer := m.heartbeatTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C():
			}
			// Reload before renewing so a manual final renewal is observed.
			st, err = m.GetCurrentState(ctx, issue)
			if err == nil {
				err = m.CheckClaim(ctx, issue, owner)
			}
			if err == nil {
				b, err = m.Budget(st)
			}
			if err != nil {
				h.fail(err)
				return
			}
			if b != nil && b.Available <= b.Threshold {
				h.fail(claimpause.ErrBudget)
				return
			}
			if b != nil && b.RenewalsRemaining == 0 {
				continue
			}
			renewed, renewErr := m.Renew(ctx, issue, owner, duration)
			if renewErr == nil {
				st = renewed
			} else if errors.Is(renewErr, ErrMaxRenewals) {
				st, err = m.GetCurrentState(ctx, issue)
			} else {
				err = renewErr
			}
			if err != nil {
				if ctx.Err() == nil {
					h.fail(err)
				}
				return
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
	h.cancel(context.Canceled)
	<-h.done
}

func (h *Heartbeat) fail(err error) {
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
	h.cancel(err)
}

type HeartbeatTimer interface {
	C() <-chan time.Time
	Stop() bool
}
type realHeartbeatTimer struct{ *time.Timer }

func (t realHeartbeatTimer) C() <-chan time.Time { return t.Timer.C }
func (m *Manager) heartbeatTimer(d time.Duration) HeartbeatTimer {
	if m.heartbeatTimerFn != nil {
		return m.heartbeatTimerFn(d)
	}
	return realHeartbeatTimer{time.NewTimer(d)}
}

// SetHeartbeatTimerFunc installs a coordinator timer seam before execution.
func (m *Manager) SetHeartbeatTimerFunc(fn func(time.Duration) HeartbeatTimer) {
	m.heartbeatTimerFn = fn
}
