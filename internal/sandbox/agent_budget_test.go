package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBudgetTimer struct {
	clock    *fakeBudgetClock
	due      time.Time
	callback func()
	stopped  bool
}

func (t *fakeBudgetTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	old := t.stopped
	t.stopped = true
	return !old
}

type fakeBudgetClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeBudgetTimer
}

func (f *fakeBudgetClock) clock() budgetClock {
	return budgetClock{
		func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
		func(d time.Duration, callback func()) budgetTimer {
			f.mu.Lock()
			defer f.mu.Unlock()
			timer := &fakeBudgetTimer{clock: f, due: f.now.Add(d), callback: callback}
			f.timers = append(f.timers, timer)
			return timer
		}}
}
func (f *fakeBudgetClock) advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	f.mu.Unlock()
	for {
		f.mu.Lock()
		var next *fakeBudgetTimer
		for _, timer := range f.timers {
			if !timer.stopped && !timer.due.After(target) && (next == nil || timer.due.Before(next.due)) {
				next = timer
			}
		}
		if next == nil {
			f.now = target
			f.mu.Unlock()
			return
		}
		f.now = next.due
		next.stopped = true
		f.mu.Unlock()
		next.callback()
	}
}
func TestAgentBudgetIdleAndWarning(t *testing.T) {
	f := &fakeBudgetClock{now: time.Now()}
	var out bytes.Buffer
	ctx, b := newAgentBudget(context.Background(), WorkerOptions{Role: "developer", Warning: &out, Limits: contract.AgentLimits{Active: time.Minute, Wall: 4 * time.Hour, ExcludeIdle: true}}, f.clock())
	defer b.close()
	f.advance(30 * time.Second)
	b.pause()
	f.advance(time.Hour)
	if ctx.Err() != nil || out.Len() != 0 {
		t.Fatalf("idle counted: %v %s", ctx.Err(), out.String())
	}
	b.resume()
	f.advance(24 * time.Second)
	if strings.Count(out.String(), "developer") != 1 || !strings.Contains(out.String(), "6s remaining") {
		t.Fatal(out.String())
	}
	b.pause()
	f.advance(time.Hour)
	b.resume()
	f.advance(6 * time.Second)
	if !b.timedOut() || !errors.Is(b.timeoutError(), context.DeadlineExceeded) {
		t.Fatal("working timeout missing")
	}
	if !strings.Contains(b.timeoutError().Error(), "1m0s") {
		t.Fatal(b.timeoutError())
	}
	if strings.Count(out.String(), "deadline") != 1 {
		t.Fatal(out.String())
	}
}
func TestAgentWallBudgetWhileIdle(t *testing.T) {
	f := &fakeBudgetClock{now: time.Now()}
	var out bytes.Buffer
	ctx, b := newAgentBudget(context.Background(), WorkerOptions{Role: "reviewer", Warning: &out, Limits: contract.AgentLimits{Active: time.Hour, Wall: time.Minute, ExcludeIdle: true}}, f.clock())
	defer b.close()
	b.pause()
	f.advance(time.Minute)
	if ctx.Err() == nil || !b.timedOut() || strings.Count(out.String(), "reviewer") != 1 {
		t.Fatalf("%v %s", ctx.Err(), out.String())
	}
}
func TestAgentBudgetCompletionAndValidation(t *testing.T) {
	for _, validation := range []bool{false, true} {
		f := &fakeBudgetClock{now: time.Now()}
		var out bytes.Buffer
		parent, cancel := context.WithCancel(context.Background())
		ctx, b := newAgentBudget(parent, WorkerOptions{DiscardChanges: validation, Warning: &out, Timeout: time.Second}, f.clock())
		if validation && (b != nil || ctx != parent || len(f.timers) != 0) {
			t.Fatal("validation got an agent budget")
		}
		cancel()
		b.close()
		f.advance(10 * time.Hour)
		if out.Len() != 0 || b.timedOut() {
			t.Fatal("warning/timeout after cancellation")
		}
	}
}

func TestBudgetFinishPreventsTimeoutDuringImport(t *testing.T) {
	f := &fakeBudgetClock{now: time.Now()}
	ctx, b := newAgentBudget(context.Background(), WorkerOptions{Timeout: time.Second}, f.clock())
	defer b.close()
	b.finish()
	f.advance(time.Hour)
	if b.timedOut() || ctx.Err() != nil {
		t.Fatal("execution budget expired during import")
	}
}
func TestBudgetStaleActiveCallbacksCannotExpirePausedRun(t *testing.T) {
	f := &fakeBudgetClock{now: time.Now()}
	ctx, b := newAgentBudget(context.Background(), WorkerOptions{Limits: contract.AgentLimits{Active: time.Minute, Wall: time.Hour, ExcludeIdle: true}}, f.clock())
	defer b.close()
	// Simulate a timer that was already dispatched when Stop was called.
	callbacks := append([]*fakeBudgetTimer(nil), f.timers...)
	b.pause()
	for _, timer := range callbacks[2:] {
		timer.callback()
	}
	if ctx.Err() != nil || b.warned {
		t.Fatal("stale active timer fired while paused")
	}
}

func TestLongAgentBudgetWarnsTwoMinutesBeforeDeadline(t *testing.T) {
	f := &fakeBudgetClock{now: time.Now()}
	var out bytes.Buffer
	_, b := newAgentBudget(context.Background(), WorkerOptions{Role: "planner", Warning: &out, Limits: contract.AgentLimits{Wall: time.Hour}}, f.clock())
	defer b.close()
	f.advance(58*time.Minute - time.Second)
	if out.Len() != 0 {
		t.Fatal("early warning")
	}
	f.advance(time.Second)
	if !strings.Contains(out.String(), "planner") || !strings.Contains(out.String(), "2m0s remaining") {
		t.Fatal(out.String())
	}
	f.advance(2 * time.Minute)
	if strings.Count(out.String(), "deadline") != 1 || !b.timedOut() {
		t.Fatal(out.String())
	}
}

// All four native turn loops use nativeInstruction; only this coordinator-owned
// input boundary may pause the working budget.
func TestNativeInstructionPausesBudget(t *testing.T) {
	f := &fakeBudgetClock{now: time.Now()}
	ctx, b := newAgentBudget(context.Background(), WorkerOptions{Limits: contract.AgentLimits{Active: time.Minute, Wall: 4 * time.Hour, ExcludeIdle: true}}, f.clock())
	defer b.close()
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		line, err := nativeInstruction(ctx, input)
		if err == nil && line != "continue" {
			err = fmt.Errorf("unexpected line %q", line)
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		paused := !b.running
		b.mu.Unlock()
		if paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("input wait did not pause budget")
		}
		time.Sleep(time.Millisecond)
	}
	f.advance(time.Hour)
	if ctx.Err() != nil {
		t.Fatal("operator idle counted")
	}
	if _, err := writer.Write([]byte("continue\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	active, running := b.active, b.running
	b.mu.Unlock()
	if active != time.Minute || !running {
		t.Fatalf("budget failed to resume: %s %v", active, running)
	}
}

func TestBudgetCannotPauseAfterExhaustingWorkingTime(t *testing.T) {
	f := &fakeBudgetClock{now: time.Now()}
	ctx, b := newAgentBudget(context.Background(), WorkerOptions{Limits: contract.AgentLimits{Active: time.Minute, Wall: time.Hour, ExcludeIdle: true}}, f.clock())
	defer b.close()
	// A delayed timer callback must not let an exhausted turn enter idle state.
	f.mu.Lock()
	f.now = f.now.Add(time.Minute)
	f.mu.Unlock()
	b.pause()
	if ctx.Err() == nil || !b.timedOut() {
		t.Fatal("exhausted budget paused without expiring")
	}
}
