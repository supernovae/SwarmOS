package fork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

// retryWaits is the backoff between attempts of an idempotent tracker read
// and between reconciliation scans after an ambiguous request post.
var retryWaits = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}

// reconcileLookback bounds every reconciliation scan from the local time just
// before the first post, allowing for several minutes of clock skew.
const reconcileLookback = 10 * time.Minute

// isTransientError reports whether a tracker call failed with an HTTP 5xx or a
// transport timeout while the caller's context is still live. Cancellation and
// the caller's own deadline are never transient.
func isTransientError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr != nil {
		return httpErr.StatusCode >= 500 && httpErr.StatusCode <= 599
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (g *Gateway) sleep(ctx context.Context, d time.Duration) error {
	if g.wait != nil {
		return g.wait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// listComments reads issue comments, retrying transient failures with bounded
// backoff. Comment listing is idempotent; writes never go through here.
func (g *Gateway) listComments(ctx context.Context, issueNumber int, since time.Time) ([]*tracker.Comment, error) {
	for attempt := 0; ; attempt++ {
		var comments []*tracker.Comment
		var err error
		if incremental, ok := g.Tracker.(interface {
			ListCommentsSince(context.Context, int, time.Time) ([]*tracker.Comment, error)
		}); ok {
			comments, err = incremental.ListCommentsSince(ctx, issueNumber, since)
		} else {
			comments, err = g.Tracker.ListComments(ctx, issueNumber)
		}
		if err == nil || !isTransientError(ctx, err) {
			return comments, err
		}
		if attempt >= len(retryWaits) {
			return nil, fmt.Errorf("after %d attempts: %w", attempt+1, err)
		}
		if waitErr := g.sleep(ctx, retryWaits[attempt]); waitErr != nil {
			return nil, fmt.Errorf("%w (retry interrupted: %w)", err, waitErr)
		}
	}
}

// postRequest creates the request comment. When GitHub reports a 5xx or
// timeout, the write may still have been stored, so it looks for the comment
// carrying this request's nonce before re-posting, and re-posts at most once.
func (g *Gateway) postRequest(ctx context.Context, req lease.ForkRequest, nonce, body string, startedAt time.Time) (*tracker.Comment, error) {
	since := startedAt.Add(-reconcileLookback)
	for attempt := 1; ; attempt++ {
		comment, err := g.Tracker.CreateComment(ctx, req.IssueNumber, body)
		if err == nil {
			if comment == nil || comment.ID <= 0 {
				return nil, fmt.Errorf("github tracker returned invalid comment ID for fork request")
			}
			return comment, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("fork request (nonce %s) posting outcome unknown: %w", nonce, err)
		}
		if !isTransientError(ctx, err) {
			if attempt == 1 {
				return nil, fmt.Errorf("post fork request comment: %w", err)
			}
			return nil, fmt.Errorf("re-post fork request comment (nonce %s): %w", nonce, err)
		}
		found, scanErr := g.findPostedRequest(ctx, req, nonce, since)
		if scanErr != nil {
			return nil, fmt.Errorf("fork request (nonce %s) posting outcome unknown after %v; not posting again: %w", nonce, err, scanErr)
		}
		if found != nil {
			return found, nil
		}
		if attempt == 2 {
			return nil, fmt.Errorf("fork request (nonce %s) posting outcome unknown: re-post failed and no matching comment was found; not posting again: %w", nonce, err)
		}
	}
}

// findPostedRequest scans recent comments up to three times for the request
// comment. A nil comment and nil error mean every scan completed without a
// match; a failed or over-budget scan is never treated as absence.
func (g *Gateway) findPostedRequest(ctx context.Context, req lease.ForkRequest, nonce string, since time.Time) (*tracker.Comment, error) {
	want, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	for scan := 0; ; scan++ {
		comments, err := g.listComments(ctx, req.IssueNumber, since)
		if err != nil {
			return nil, fmt.Errorf("scan for request comment: %w", err)
		}
		found, err := matchRequestComment(ctx, comments, req, nonce, want)
		if err != nil || found != nil {
			return found, err
		}
		if scan >= len(retryWaits) {
			return nil, nil
		}
		if err := g.sleep(ctx, retryWaits[scan]); err != nil {
			return nil, err
		}
	}
}

func matchRequestComment(ctx context.Context, comments []*tracker.Comment, req lease.ForkRequest, nonce string, want []byte) (*tracker.Comment, error) {
	if len(comments) > 1000 {
		return nil, fmt.Errorf("fork comment scan exceeds 1000 records")
	}
	var found *tracker.Comment
	size := 0
	for _, c := range comments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c == nil {
			continue
		}
		size += len(c.Body)
		if size > 8<<20 {
			return nil, fmt.Errorf("fork comment scan exceeds byte budget")
		}
		if c.ID <= 0 || (c.IssueNum != 0 && c.IssueNum != req.IssueNumber) || !IsRequestComment(c.Body) {
			continue
		}
		if !c.UpdatedAt.IsZero() && !c.CreatedAt.Equal(c.UpdatedAt) {
			continue
		}
		if requestCommentNonce(c.Body) != nonce {
			continue
		}
		parsed, err := ParseRequestComment(c.Body)
		if err != nil {
			continue
		}
		got, err := json.Marshal(parsed)
		if err != nil || !bytes.Equal(got, want) {
			continue
		}
		if found != nil && found.ID != c.ID {
			return nil, fmt.Errorf("multiple comments match fork request nonce %s (%d and %d)", nonce, found.ID, c.ID)
		}
		found = c
	}
	return found, nil
}

// legacyCoordinatorRejection reports a trusted result that could not name the
// operation because the coordinator failed to parse the request, as older
// coordinators do for fields they do not know.
func legacyCoordinatorRejection(req lease.ForkRequest, reqID int64, msg string) error {
	return fmt.Errorf("fork coordinator could not read request %d (%s): %s; the upstream repository may need to update its SwarmOS Fork Coordinator workflow", reqID, req.Operation, msg)
}
