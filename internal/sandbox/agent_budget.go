package sandbox

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

type agentBudgetKey struct{}
type budgetTimer interface{ Stop() bool }
type budgetClock struct {
	now   func() time.Time
	after func(time.Duration, func()) budgetTimer
}

var realBudgetClock = budgetClock{time.Now, func(d time.Duration, f func()) budgetTimer { return time.AfterFunc(d, f) }}

// AgentTimeoutError contains only coordinator-generated timeout diagnostics.
// Callers may display it while withholding arbitrary harness failure output.
type AgentTimeoutError struct {
	cause      error
	diagnostic string
}

func (e *AgentTimeoutError) Error() string { return fmt.Sprintf("%v; %s", e.cause, e.diagnostic) }
func (e *AgentTimeoutError) Unwrap() error { return e.cause }

type agentTimeout struct {
	role, kind string
	limit      time.Duration
}

func (e *agentTimeout) Error() string {
	return fmt.Sprintf("%s agent %s timeout after configured limit %s", e.role, e.kind, e.limit)
}
func (e *agentTimeout) Unwrap() error { return context.DeadlineExceeded }

type agentBudget struct {
	mu                       sync.Mutex
	ctx                      context.Context
	cancel                   context.CancelCauseFunc
	clock                    budgetClock
	role                     string
	warning                  io.Writer
	active, wall             time.Duration
	activeLimit              time.Duration
	wallDeadline             time.Time
	generation               uint64
	started                  time.Time
	running, warned, closed  bool
	activeTimers, wallTimers []budgetTimer
	timeout                  *agentTimeout
}

func startAgentBudget(ctx context.Context, opts WorkerOptions) (context.Context, *agentBudget) {
	return newAgentBudget(ctx, opts, realBudgetClock)
}
func newAgentBudget(parent context.Context, opts WorkerOptions, clock budgetClock) (context.Context, *agentBudget) {
	if opts.DiscardChanges {
		return parent, nil
	}
	limits := opts.Limits
	if limits.Wall <= 0 {
		limits, _ = contract.ResolveAgentLimits(contract.ExecutionConfig{}, opts.Role, false, opts.Timeout)
	}
	if opts.Timeout > 0 {
		limits.Wall = min(limits.Wall, opts.Timeout)
		if limits.Active > 0 {
			limits.Active = min(limits.Active, opts.Timeout)
		}
	}
	ctx, cancel := context.WithCancelCause(parent)
	b := &agentBudget{ctx: ctx, cancel: cancel, clock: clock, role: opts.Role, warning: opts.Warning, active: limits.Active, wall: limits.Wall}
	if !limits.ExcludeIdle {
		b.active = 0
	}
	b.activeLimit = b.active
	b.wallDeadline = clock.now().Add(b.wall)
	ctx = context.WithValue(ctx, agentBudgetKey{}, b)
	ctx = agentBudgetContext{Context: ctx, budget: b}
	b.mu.Lock()
	b.wallTimers = b.schedule(b.wall, "wall", 0)
	// Warn for a shorter coordinator deadline as well; cancellation remains owned by parent.
	if deadline, ok := parent.Deadline(); ok && deadline.Sub(clock.now()) < b.wall {
		remaining := deadline.Sub(clock.now())
		b.wallTimers = append(b.wallTimers, clock.after(max(0, remaining-contract.AgentWarningLead(remaining)), func() { b.warn(contract.AgentWarningLead(remaining)) }))
	}
	b.resumeLocked()
	b.mu.Unlock()
	return ctx, b
}

type agentBudgetContext struct {
	context.Context
	budget *agentBudget
}

func (c agentBudgetContext) Deadline() (time.Time, bool) {
	b := c.budget
	b.mu.Lock()
	deadline := b.wallDeadline
	if b.running {
		if active := b.started.Add(b.active); active.Before(deadline) {
			deadline = active
		}
	}
	b.mu.Unlock()
	if parent, ok := c.Context.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	return deadline, true
}
func (b *agentBudget) schedule(remaining time.Duration, kind string, generation uint64) []budgetTimer {
	limit := remaining
	if kind == "working-time" {
		limit = b.activeLimit
	}
	valid := func() bool {
		return !b.closed && b.ctx.Err() == nil && (kind != "working-time" || (b.running && generation == b.generation))
	}
	return []budgetTimer{
		b.clock.after(max(0, remaining-contract.AgentWarningLead(limit)), func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if valid() {
				b.warnLocked(contract.AgentWarningLead(limit))
			}
		}),
		b.clock.after(max(0, remaining), func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if !valid() {
				return
			}
			b.timeout = &agentTimeout{b.role, kind, limit}
			b.cancel(b.timeout)
		}),
	}
}
func (b *agentBudget) warn(remaining time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.warnLocked(remaining)
}
func (b *agentBudget) warnLocked(remaining time.Duration) {
	if b.closed || b.warned || b.ctx.Err() != nil {
		return
	}
	b.warned = true
	if b.warning != nil {
		fmt.Fprintf(b.warning, "SwarmOS: %s agent deadline approaching (%s remaining)\n", b.role, remaining)
	}
}
func stopBudgetTimers(timers []budgetTimer) {
	for _, t := range timers {
		t.Stop()
	}
}
func (b *agentBudget) pause() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		return
	}
	b.active -= b.clock.now().Sub(b.started)
	b.running = false
	b.generation++
	stopBudgetTimers(b.activeTimers)
	if b.active <= 0 && !b.closed && b.ctx.Err() == nil {
		b.timeout = &agentTimeout{b.role, "working-time", b.activeLimit}
		b.cancel(b.timeout)
	}
}
func (b *agentBudget) resumeLocked() {
	if b.closed || b.ctx.Err() != nil || b.activeLimit <= 0 {
		return
	}
	b.started, b.running = b.clock.now(), true
	b.generation++
	b.activeTimers = b.schedule(max(0, b.active), "working-time", b.generation)
}
func (b *agentBudget) resume() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		b.resumeLocked()
	}
}

// finish freezes the execution outcome before any successful output import.
func (b *agentBudget) finish() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	stopBudgetTimers(b.activeTimers)
	stopBudgetTimers(b.wallTimers)
}
func (b *agentBudget) close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	stopBudgetTimers(b.activeTimers)
	stopBudgetTimers(b.wallTimers)
	b.cancel(context.Canceled)
}
func (b *agentBudget) timedOut() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.timeout != nil
}
func (b *agentBudget) timeoutError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.timeout
}
