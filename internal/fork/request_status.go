package fork

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/tracker"
)

func unresolvedRequest(issue int, id int64, err error) error {
	return fmt.Errorf("outcome of fork request %d on issue #%d remains unresolved; query the original request with `swarm --fork fork-coordinator request-status %d %d`: %w", id, issue, issue, id, err)
}

func requestContextError(issue int, id int64, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return unresolvedRequest(issue, id, err)
	}
	return err
}

// RequestStatus observes one original request without reposting or resuming work.
// The returned operation comes from the stored request, never caller authority.
func (g *Gateway) RequestStatus(ctx context.Context, issue int, id int64) (*lease.IssueState, string, error) {
	if issue <= 0 || id <= 0 {
		return nil, "", fmt.Errorf("issue and request ID must be positive")
	}
	if g == nil || g.Tracker == nil || g.Store == nil {
		return nil, "", fmt.Errorf("fork worker gateway is not configured")
	}
	timeout := g.PollTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, original, err := g.originalRequest(ctx, issue, id)
	if err != nil {
		return nil, "", unresolvedRequest(issue, id, err)
	}
	p := &requestProgress{emit: g.OnProgress, operation: req.Operation, id: id}
	if g.OnProgress != nil {
		p.send("waiting", 0)
		p.reporter = progress.NewReporter(ctx, func(elapsed time.Duration) { p.send("waiting", elapsed) }, g.progressTicks, g.progressNow)
	}
	defer p.stop()
	since := original.CreatedAt.Add(-time.Second)
	interval := g.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, req.Operation, unresolvedRequest(issue, id, err)
		}
		st, done, err := g.checkRequestOutcomeReporting(ctx, *req, id, &since, func() { p.finish("rejected") })
		if err != nil {
			return nil, req.Operation, requestContextError(issue, id, err)
		}
		if done {
			p.finish("accepted")
			return st, req.Operation, nil
		}
		if err := g.sleep(ctx, interval); err != nil {
			return nil, req.Operation, unresolvedRequest(issue, id, err)
		}
		interval = min(interval*2, 5*time.Second)
	}
}

func (g *Gateway) originalRequest(ctx context.Context, issue int, id int64) (*lease.ForkRequest, *tracker.Comment, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	comments, err := g.listComments(ctx, issue, time.Time{})
	if err != nil {
		return nil, nil, fmt.Errorf("look up original request: %w", err)
	}
	if len(comments) > 1000 {
		return nil, nil, fmt.Errorf("original request scan exceeds 1000 records")
	}
	var original *tracker.Comment
	size := 0
	for _, c := range comments {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if c == nil {
			continue
		}
		size += len(c.Body)
		if size > 8<<20 {
			return nil, nil, fmt.Errorf("original request scan exceeds 8 MiB byte budget")
		}
		if c.ID != id {
			continue
		}
		if original != nil {
			return nil, nil, fmt.Errorf("duplicate original request ID")
		}
		original = c
	}
	if original == nil {
		return nil, nil, fmt.Errorf("original request not found within bounded issue scan")
	}
	if original.CreatedAt.IsZero() || (!original.UpdatedAt.IsZero() && !original.UpdatedAt.Equal(original.CreatedAt)) || (original.IssueNum != 0 && original.IssueNum != issue) {
		return nil, nil, fmt.Errorf("original request has missing timestamp, edits or mismatched issue")
	}
	req, err := ParseRequestComment(original.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid original request")
	}
	if req.IssueNumber != issue || !slices.Contains(Operations, req.Operation) {
		return nil, nil, fmt.Errorf("original request has mismatched issue or unsupported operation")
	}
	return req, original, nil
}
