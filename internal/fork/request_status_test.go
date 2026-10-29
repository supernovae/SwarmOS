package fork

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

type statusTracker struct {
	tracker.Tracker
	comments []*tracker.Comment
	read     func(context.Context, time.Time) ([]*tracker.Comment, error)
	posts    int
}

func (s *statusTracker) ListCommentsSince(ctx context.Context, _ int, since time.Time) ([]*tracker.Comment, error) {
	if s.read != nil {
		return s.read(ctx, since)
	}
	return s.comments, nil
}
func (s *statusTracker) CreateComment(context.Context, int, string) (*tracker.Comment, error) {
	s.posts++
	return s.comments[0], nil
}
func statusOriginal(t *testing.T) *tracker.Comment {
	t.Helper()
	body, err := FormatRequestComment(lease.ForkRequest{Operation: "release", IssueNumber: 1, ClaimID: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	return &tracker.Comment{ID: 100, IssueNum: 1, Body: body, CreatedAt: time.Now().Add(-time.Hour)}
}
func TestRequestStatusOriginalValidation(t *testing.T) {
	for _, name := range []string{"missing", "duplicate", "edited", "cross-issue", "payload-issue", "unsupported", "timestamp", "malformed", "oversized", "records", "bytes"} {
		t.Run(name, func(t *testing.T) {
			c := statusOriginal(t)
			comments := []*tracker.Comment{c}
			switch name {
			case "missing":
				comments = nil
			case "duplicate":
				comments = append(comments, c)
			case "edited":
				c.UpdatedAt = c.CreatedAt.Add(time.Second)
			case "cross-issue":
				c.IssueNum = 2
			case "payload-issue":
				c.Body = strings.ReplaceAll(c.Body, `"issue_number": 1`, `"issue_number": 2`)
			case "unsupported":
				c.Body = strings.ReplaceAll(c.Body, "release", "unknown")
			case "timestamp":
				c.CreatedAt = time.Time{}
			case "malformed":
				c.Body = RequestMarker + "secret"
			case "oversized":
				c.Body = strings.Repeat("s", MaxForkCommentBytes+1)
			case "records":
				comments = make([]*tracker.Comment, 1001)
			case "bytes":
				comments = append(comments, &tracker.Comment{Body: strings.Repeat("s", 8<<20)})
			}
			g := Gateway{Tracker: &statusTracker{comments: comments}, Store: emptyPollingStore{}}
			_, _, err := g.RequestStatus(t.Context(), 1, 100)
			if err == nil || !strings.Contains(err.Error(), "request-status 1 100") || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
		})
	}
}
func TestRequestStatusLateOriginalReadOnly(t *testing.T) {
	c := statusOriginal(t)
	tr := &statusTracker{comments: []*tracker.Comment{c}}
	g := Gateway{Tracker: tr, Store: emptyPollingStore{}, PollTimeout: time.Millisecond, Dispatcher: func(context.Context, int, *tracker.Comment) error { t.Fatal("dispatch during recovery"); return nil }}
	// The original mutation posts once, then expires. Recovery must not post again.
	g.Dispatcher = nil
	_, err := g.Apply(t.Context(), lease.ForkRequest{Operation: "release", IssueNumber: 1, ClaimID: "secret"})
	if !errors.Is(err, context.DeadlineExceeded) || tr.posts != 1 {
		t.Fatalf("%v posts=%d", err, tr.posts)
	}
	state := lease.IssueState{LastForkRequestID: 100}
	state.Lease.Status = "released"
	g.Store = progressStore{state: state}
	tr.read = func(_ context.Context, since time.Time) ([]*tracker.Comment, error) {
		if since.IsZero() {
			return tr.comments, nil
		}
		if !since.Equal(c.CreatedAt.Add(-time.Second)) {
			t.Fatal(since)
		}
		return nil, nil
	}
	g.Dispatcher = func(context.Context, int, *tracker.Comment) error { t.Fatal("status dispatched"); return nil }
	var events []ProgressEvent
	g.OnProgress = func(e ProgressEvent) { events = append(events, e) }
	for range 2 {
		_, op, err := g.RequestStatus(t.Context(), 1, 100)
		if err != nil || op != "release" {
			t.Fatal(op, err)
		}
	}
	if tr.posts != 1 || len(events) != 4 || events[0].Status != "waiting" || events[1].Status != "accepted" {
		t.Fatal(tr.posts, events)
	}
}
func TestRequestStatusVerifiedOutcomes(t *testing.T) {
	for _, name := range []string{"success", "rejected", "legacy", "human", "spoof", "edited", "wrong-request", "wrong-issue", "wrong-operation", "old", "malformed", "absent", "superseded", "head-mismatch"} {
		t.Run(name, func(t *testing.T) {
			c := statusOriginal(t)
			r := ForkResult{Version: 1, RequestID: 100, IssueNumber: 1, Operation: "release", Success: true}
			result := &tracker.Comment{ID: 101, Author: ActionsBotLogin, AuthorType: "Bot", CreatedAt: c.CreatedAt, UpdatedAt: c.CreatedAt}
			st := lease.IssueState{LastForkRequestID: 100}
			st.Lease.Status = "released"
			switch name {
			case "rejected":
				r.Success = false
				r.Error = lease.ErrClaimFence.Error()
			case "legacy":
				r.Success = false
				r.Operation = ""
				r.Error = lease.ErrClaimFence.Error()
			case "human":
				result.Author = "human"
				st.LastForkRequestID = 0
			case "spoof":
				result.AuthorType = "User"
				st.LastForkRequestID = 0
			case "edited":
				result.UpdatedAt = result.CreatedAt.Add(time.Second)
				st.LastForkRequestID = 0
			case "wrong-request":
				r.RequestID = 99
				st.LastForkRequestID = 0
			case "wrong-issue":
				r.IssueNumber = 2
				st.LastForkRequestID = 0
			case "wrong-operation":
				r.Operation = "renew"
				st.LastForkRequestID = 0
			case "old":
				result.ID = 99
				st.LastForkRequestID = 0
			case "malformed":
				st.LastForkRequestID = 0
			case "absent":
				st.LastForkRequestID = 0
			case "superseded":
				st.LastForkRequestID = 102
			case "head-mismatch":
				c.Body, _ = FormatRequestComment(lease.ForkRequest{Operation: "record-checkpoint", IssueNumber: 1, HeadSHA: "expected"})
				r.Operation = "record-checkpoint"
				st.HeadSHA = "other"
			}
			result.Body, _ = FormatResultComment(r)
			if name == "malformed" {
				result.Body = ResultMarker + "bad"
			}
			g := Gateway{Tracker: &statusTracker{comments: []*tracker.Comment{c, result}}, Store: progressStore{state: st}, PollTimeout: time.Millisecond}
			_, _, err := g.RequestStatus(t.Context(), 1, 100)
			if name == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("false success")
			}
			if name == "rejected" && !errors.Is(err, lease.ErrClaimFence) {
				t.Fatal(err)
			}
		})
	}
}
func TestRequestStatusDeadlineIncludesLookup(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			tr := &statusTracker{read: func(ctx context.Context, _ time.Time) ([]*tracker.Comment, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 90*time.Second {
					t.Fatal("unbounded")
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
			defer cancel()
			g := Gateway{Tracker: tr, Store: emptyPollingStore{}, PollTimeout: timeout}
			_, _, err := g.RequestStatus(ctx, 1, 100)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

func TestRequestStatusCancellationAndHeartbeat(t *testing.T) {
	c := statusOriginal(t)
	ticks := make(chan time.Time)
	events := make(chan ProgressEvent, 8)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tr := &statusTracker{read: func(ctx context.Context, since time.Time) ([]*tracker.Comment, error) {
		if since.IsZero() {
			return []*tracker.Comment{c}, nil
		}
		ticks <- time.Time{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	g := Gateway{Tracker: tr, Store: emptyPollingStore{}, progressTicks: ticks, OnProgress: func(e ProgressEvent) { events <- e }}
	done := make(chan error, 1)
	go func() { _, _, err := g.RequestStatus(ctx, 1, 100); done <- err }()
	for range 2 {
		select {
		case e := <-events:
			if e.Status != "waiting" || e.RequestID != 100 {
				t.Fatal(e)
			}
		case <-time.After(time.Second):
			t.Fatal("missing heartbeat")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "request-status 1 100") {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reporter did not stop")
	}
	select {
	case e := <-events:
		t.Fatal("late progress", e)
	default:
	}
}

func TestRequestStatusDefaultBudget(t *testing.T) {
	tr := &statusTracker{read: func(ctx context.Context, _ time.Time) ([]*tracker.Comment, error) {
		deadline, ok := ctx.Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining < 89*time.Second || remaining > 90*time.Second {
			t.Fatalf("default budget: %v %v", ok, remaining)
		}
		return nil, context.Canceled
	}}
	g := Gateway{Tracker: tr, Store: emptyPollingStore{}}
	_, _, err := g.RequestStatus(context.Background(), 1, 100)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestGatewayDispatcherDeadlineRecovery(t *testing.T) {
	tr := &statusTracker{comments: []*tracker.Comment{statusOriginal(t)}, read: func(ctx context.Context, _ time.Time) ([]*tracker.Comment, error) { return nil, ctx.Err() }}
	g := Gateway{Tracker: tr, Store: emptyPollingStore{}, PollTimeout: time.Millisecond, Dispatcher: func(ctx context.Context, _ int, _ *tracker.Comment) error { <-ctx.Done(); return ctx.Err() }}
	_, err := g.Apply(t.Context(), lease.ForkRequest{Operation: "release", IssueNumber: 1, ClaimID: "secret"})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "request-status 1 100") {
		t.Fatal(err)
	}
}
