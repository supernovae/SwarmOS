// Package claimpause carries an in-process coordinator signal. It is never
// serialized into an agent request or accepted from agent output.
package claimpause

import (
	"context"
	"errors"
	"time"
)

var ErrBudget = errors.New("paused for claim budget")

type parentKey struct{}

func WithParent(ctx, parent context.Context) context.Context {
	return context.WithValue(ctx, parentKey{}, parent)
}

// Cleanup permits bounded preservation only for an intentional budget pause.
func Cleanup(ctx context.Context, role string) (context.Context, context.CancelFunc, bool) {
	parent, ok := ctx.Value(parentKey{}).(context.Context)
	if !ok || parent.Err() != nil || context.Cause(ctx) != ErrBudget || (role != "developer" && role != "tester") {
		return nil, nil, false
	}
	cleanup, cancel := context.WithTimeout(parent, 30*time.Second)
	return cleanup, cancel, true
}

// PreservationError prevents a failed cleanup/import from becoming a pause.
type PreservationError struct{ Err error }

func (e *PreservationError) Error() string {
	return "claim-budget preservation failed: " + e.Err.Error()
}
func (e *PreservationError) Unwrap() error { return e.Err }

// CoordinatorContext returns the live parent for a coordinator budget handoff.
// It does not permit snapshot import; Cleanup separately checks writable roles.
func CoordinatorContext(ctx context.Context) context.Context {
	if context.Cause(ctx) == ErrBudget {
		if parent, ok := ctx.Value(parentKey{}).(context.Context); ok {
			return parent
		}
	}
	return ctx
}
