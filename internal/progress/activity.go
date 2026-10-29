package progress

import (
	"context"
	"io"
	"sync"
	"time"
)

// Operation names an allowlisted coordinator operation.
type Operation string

const (
	OpStateRead     Operation = "state read"
	OpStateWrite    Operation = "state write"
	OpTrackerUpdate Operation = "tracker update"
	OpCheckpoint    Operation = "checkpoint publish"
)

type opEntry struct {
	id      uint64
	op      Operation
	started time.Time
}

// Activity retains only allowlisted categories and timestamps, never output bytes.
type Activity struct {
	mu          sync.Mutex
	role, tool  string
	roleID, seq uint64
	last        time.Time
	observed    bool
	ops         [4]opEntry
}
type activityKey struct{}

func NewActivity() *Activity { return &Activity{} }

// WithActivity attaches a tracker; nil masks an inherited tracker.
func WithActivity(ctx context.Context, a *Activity) context.Context {
	return context.WithValue(ctx, activityKey{}, a)
}
func ActivityFrom(ctx context.Context) *Activity {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(activityKey{}).(*Activity)
	return a
}
func ValidRole(s string) bool {
	switch s {
	case "planner", "reviewer", "developer", "tester", "agent":
		return true
	}
	return false
}
func ValidTool(s string) bool {
	switch s {
	case "claude", "codex", "opencode", "omp", "mock", "harness":
		return true
	}
	return false
}
func ValidOperation(s string) bool {
	switch Operation(s) {
	case OpStateRead, OpStateWrite, OpTrackerUpdate, OpCheckpoint:
		return true
	}
	return false
}

// Role opens a role scope. Its idempotent end cannot clear a newer scope.
func (a *Activity) Role(role, tool string, now time.Time) func() {
	if a == nil {
		return func() {}
	}
	if !ValidRole(role) {
		role = "agent"
	}
	if !ValidTool(tool) {
		tool = "harness"
	}
	a.mu.Lock()
	a.seq++
	id := a.seq
	a.roleID = id
	a.role, a.tool, a.observed = role, tool, false
	a.last = time.Time{}
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.roleID == id {
			a.role, a.tool = "", ""
			a.observed = false
			a.roleID = 0
		}
	}
}
func (a *Activity) Observe(now time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.role != "" {
		a.last, a.observed = now, true
	}
}

type activityWriter struct {
	a   *Activity
	now func() time.Time
}

func (w activityWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.a.Observe(w.now())
	}
	return len(p), nil
}

// Writer observes nonempty writes without inspecting or retaining their bytes.
func (a *Activity) Writer(now func() time.Time) io.Writer {
	if a == nil {
		return io.Discard
	}
	return activityWriter{a, now}
}

// Begin tracks one of at most four overlapping coordinator operations.
// The newest active operation takes precedence over the role.
func Begin(ctx context.Context, op Operation, now time.Time) func() {
	a := ActivityFrom(ctx)
	if a == nil || !ValidOperation(string(op)) {
		return func() {}
	}
	a.mu.Lock()
	slot := -1
	for i := range a.ops {
		if a.ops[i].id == 0 {
			slot = i
			break
		}
	}
	if slot < 0 {
		a.mu.Unlock()
		return func() {}
	}
	a.seq++
	id := a.seq
	a.ops[slot] = opEntry{id, op, now}
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.ops[slot].id == id {
			a.ops[slot] = opEntry{}
		}
	}
}
func nonnegative(d time.Duration) time.Duration { return max(d, 0) }

// Enrich adds revalidated metadata to running heartbeats only.
func (a *Activity) Enrich(e *Event, now time.Time) {
	if a == nil || e == nil || e.Status != "running" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e.Role, e.Tool, e.Operation = "", "", ""
	e.ActivityKnown = false
	e.ActivityAge, e.OperationElapsed = 0, 0
	var active opEntry
	for _, op := range a.ops {
		if op.id > active.id {
			active = op
		}
	}
	if active.id != 0 {
		if ValidOperation(string(active.op)) {
			e.Operation = string(active.op)
			e.OperationElapsed = nonnegative(now.Sub(active.started))
		}
		return
	}
	if ValidRole(a.role) && ValidTool(a.tool) {
		e.Role, e.Tool = a.role, a.tool
		e.ActivityKnown = a.observed
		if a.observed {
			e.ActivityAge = nonnegative(now.Sub(a.last))
		}
	}
}
