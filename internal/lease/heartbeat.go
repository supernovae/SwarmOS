package lease

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/claimpause"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/progress"
)

// Heartbeat maintains a fenced claim while long-running work holds it. Fatal
// errors and expiry-bounded transport failures cancel Context.
type Heartbeat struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

// StartHeartbeat checks the bound claim every third of duration (at least once
// per second), renewing only in the last third. Transport recovery is bounded
// by the last checked expiry; each retry starts with a fresh authoritative read.
func (m *Manager) StartHeartbeat(parent context.Context, issue int, owner string, duration time.Duration) *Heartbeat {
	ctx, cancel := context.WithCancelCause(parent)
	ctx = claimpause.WithParent(ctx, parent)
	h := &Heartbeat{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	interval := max(duration/3, time.Second)
	margin := min(30*time.Second, duration/12)
	go func() {
		defer close(h.done)
		rctx := progress.WithActivity(ctx, nil)
		st, err := m.CheckClaimState(rctx, issue, owner)
		if err != nil {
			if ctx.Err() == nil {
				h.fail(err)
			}
			return
		}
		var outage error
		first := true
		initialWait := true
		for {
			if ctx.Err() != nil {
				return
			}
			attemptStarted := time.Now()
			cutoff := st.Lease.ExpiresAt.Add(-margin)
			remaining := cutoff.Sub(m.nowFn())
			if remaining <= 0 || (initialWait && remaining <= time.Nanosecond) {
				if outage != nil {
					h.fail(outage)
				} else {
					h.fail(ErrClaimFence)
				}
				return
			}
			// Translate the manager clock's remaining duration to a real deadline. This
			// also keeps injected clocks independent of the runtime's wall clock.
			// Anchor before reading the manager clock so setup cannot extend the cutoff.
			attempt, stop := context.WithDeadline(rctx, attemptStarted.Add(remaining))
			checkAttempt := func() error {
				if err := attempt.Err(); err != nil {
					return err
				}
				if !cutoff.After(m.nowFn()) {
					return context.DeadlineExceeded
				}
				return nil
			}
			if !first {
				var checked *IssueState
				// Reload and validate independently, retaining only the checked snapshot.
				_, err = m.GetCurrentState(attempt, issue)
				if err == nil {
					err = checkAttempt()
				}
				if err == nil {
					checked, err = m.CheckClaimState(attempt, issue, owner)
					if err == nil {
						err = checkAttempt()
					}
				}
				if err == nil {
					st = checked
				}
			}
			first = false
			var b *ClaimBudget
			if err == nil {
				b, err = m.WorkBudgetContext(attempt, st, duration)
				if err == nil {
					err = checkAttempt()
				}
			}
			due := st.Lease.ExpiresAt.Sub(m.nowFn()) <= duration/3
			if err == nil && b != nil && !b.LeaseExpiryOnly() && b.Available <= b.Threshold {
				stop()
				h.fail(claimpause.ErrBudget)
				return
			}
			if err == nil && !initialWait && due && (b == nil || b.LeaseExpiryOnly() || b.RenewalsRemaining > 0) {
				var renewed *IssueState
				renewed, err = m.Renew(attempt, issue, owner, duration)
				if err == nil {
					err = checkAttempt()
				}
				if err == nil {
					st = renewed
					// Preserve approval validation and activity masking after publication.
					var checked *IssueState
					checked, err = m.CheckClaimState(attempt, issue, owner)
					if err == nil {
						err = checkAttempt()
					}
					if err == nil {
						st = checked
						b, err = m.WorkBudgetContext(attempt, st, duration)
						if err == nil {
							err = checkAttempt()
						}
					}
				}
			}
			retryable := heartbeatTransportError(attempt, err)
			attemptErr := attempt.Err()
			stop()
			if ctx.Err() != nil {
				return
			}
			remaining = st.Lease.ExpiresAt.Add(-margin).Sub(m.nowFn())
			delay := interval
			if err != nil {
				// A deadline from our own bounded attempt is terminal. Preserve the first
				// transport failure when a later recovery call blocks until the cutoff.
				if !retryable || remaining <= 0 || attemptErr != nil {
					if outage != nil && heartbeatTransportError(rctx, err) && (remaining <= 0 || attemptErr != nil) {
						err = outage
					}
					h.fail(err)
					return
				}
				if outage == nil {
					outage = err
				}
				delay = min(time.Second, remaining)
			} else {
				outage = nil
				if remaining <= 0 {
					h.fail(ErrClaimFence)
					return
				}
				if b != nil && !(initialWait && due && b.LeaseExpiryOnly()) {
					if b.Available <= b.Threshold {
						h.fail(claimpause.ErrBudget)
						return
					}
					delay = min(delay, b.Available-b.Threshold)
				}
				if b == nil || b.LeaseExpiryOnly() || b.RenewalsRemaining > 0 {
					if initialWait && due {
						delay = min(delay, max(time.Nanosecond, remaining/2))
					} else {
						delay = min(delay, st.Lease.ExpiresAt.Sub(m.nowFn())-duration/3)
					}
				}
				delay = min(delay, remaining)
				initialWait = false
			}
			timer := m.heartbeatTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C():
			}
			// Never perform another call after a recovery timer reaches the cutoff.
			if outage != nil && !st.Lease.ExpiresAt.Add(-margin).After(m.nowFn()) {
				h.fail(outage)
				return
			}
		}
	}()
	return h
}

// heartbeatTransportError admits only transport evidence, after inspecting all
// branches for fatal authority, policy and ambiguous-publication errors.
func heartbeatTransportError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	for _, fatal := range []error{context.Canceled, ErrClaimFence, ErrMaxRenewals, ErrNoActiveLease, ErrApprovalRequired, ErrUntrustedIssue, ErrStoreUnconfigured, ErrHarnessGrant, coordination.ErrConflict, coordination.ErrPublicationUnknown, coordination.ErrNotFound, gitcredential.ErrInterrupted, gitcredential.ErrCleanupIncomplete} {
		if errors.Is(err, fatal) {
			return false
		}
	}
	var approval *TaskApprovalError
	var planApproval *PlanApprovalError
	var humanApproval *HumanPlanApprovalError
	var cleanup *gitcredential.KnownResultCleanupError
	if errors.As(err, &approval) || errors.As(err, &planApproval) || errors.As(err, &humanApproval) || errors.As(err, &cleanup) {
		return false
	}
	fatal := false
	heartbeatErrorTree(err, func(e error) {
		switch e.(type) {
		case *os.PathError, *json.SyntaxError, *json.UnmarshalTypeError, *json.MarshalerError:
			fatal = true
		}
		if h, ok := e.(*api.HTTPError); ok && (h.StatusCode < 500 || h.StatusCode > 599) {
			fatal = true
		}
		text := e.Error()
		for _, prefix := range []string{"load approved policy:", "load approved budget policy:", "fork coordinator rejected", "coordinator rejected"} {
			if strings.HasPrefix(text, prefix) {
				fatal = true
			}
		}
	})
	if fatal {
		return false
	}
	return heartbeatTransportCause(err)
}

func heartbeatTransportCause(err error) bool {
	if err == nil {
		return false
	}
	// All joined branches need transport evidence; an unrelated policy or parse
	// failure must not gain retry permission from a neighboring network cause.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !heartbeatTransportCause(child) {
				return false
			}
		}
		return true
	}
	switch e := err.(type) {
	case *api.HTTPError:
		return e.StatusCode >= 500 && e.StatusCode <= 599
	case *url.Error:
		return heartbeatTransportCause(e.Err)
	case *gitcredential.RetryExhaustedError:
		switch e.Operation {
		case "git fetch", "git ls-remote", "git push":
			return true
		}
	}
	if _, ok := err.(net.Error); ok {
		return true
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF || err == syscall.ECONNRESET || err == syscall.EPIPE {
		return true
	}
	var exit *exec.ExitError
	if strings.HasPrefix(err.Error(), "git state push:") && errors.As(err, &exit) {
		return gitcredential.RetryReason(gitcredential.RetryObservation{Operation: "GET", Err: exit, Diagnostic: err.Error()}) != ""
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return heartbeatTransportCause(wrapped.Unwrap())
	}
	return false
}

func heartbeatErrorTree(err error, visit func(error)) {
	if err == nil {
		return
	}
	visit(err)
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range e.Unwrap() {
			heartbeatErrorTree(child, visit)
		}
	case interface{ Unwrap() error }:
		heartbeatErrorTree(e.Unwrap(), visit)
	}
}

// Context is canceled when the heartbeat stops, loses authority, or cannot recover safely.
func (h *Heartbeat) Context() context.Context { return h.ctx }

// Err reports the failure that canceled Context, if any.
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
