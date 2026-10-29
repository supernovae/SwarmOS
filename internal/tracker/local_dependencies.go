package tracker

import "context"

// Native projections are unsupported locally; these no-ops never persist state.
func (l *LocalTracker) ListBlockedBy(context.Context, int) ([]BlockedByReference, error) {
	return nil, nil
}
func (l *LocalTracker) LinkBlockedBy(context.Context, int, int) error    { return nil }
func (l *LocalTracker) OrderSubIssues(context.Context, int, []int) error { return nil }
