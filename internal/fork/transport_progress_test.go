package fork

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

type progressStore struct {
	emptyPollingStore
	state lease.IssueState
	err   error
}

func (s progressStore) Load(context.Context, int) ([]byte, string, error) {
	b, e := json.Marshal(s.state)
	if s.err != nil {
		e = s.err
	}
	return b, "", e
}

type progressTracker struct {
	pollingTracker
	postError error
	id        int64
}

func (p *progressTracker) CreateComment(context.Context, int, string) (*tracker.Comment, error) {
	return &tracker.Comment{ID: p.id}, p.postError
}

func TestGatewayProgressOutcomes(t *testing.T) {
	for _, name := range []string{"confirmed", "fallback", "rejected", "unconfirmed", "superseded", "poll-error", "timeout", "canceled", "store-error", "edited", "untrusted", "mismatched", "post-error", "invalid-id"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var events []ProgressEvent
			now := time.Now()
			state := lease.IssueState{LastForkRequestID: 100}
			state.Lease.Status = "released"
			req := lease.ForkRequest{Operation: "release", IssueNumber: 1, ClaimID: "secret\n\x1btoken"}
			res := ForkResult{Version: 1, RequestID: 100, IssueNumber: 1, Operation: "release", Success: true}
			if name == "rejected" {
				res.Success = false
				res.Error = lease.ErrClaimFence.Error() + ": secret\n\x1btoken"
			}
			if name == "mismatched" {
				res.Operation = "renew"
			}
			body, _ := FormatResultComment(res)
			comment := &tracker.Comment{ID: 101, Author: ActionsBotLogin, Body: body, CreatedAt: now, UpdatedAt: now}
			if name == "edited" {
				comment.UpdatedAt = now.Add(time.Second)
			}
			if name == "untrusted" {
				comment.Author = "attacker"
			}
			if name == "unconfirmed" {
				state.LastForkRequestID = 99
			}
			if name == "superseded" {
				state.LastForkRequestID = 101
			}
			if name == "timeout" || name == "canceled" || name == "edited" || name == "untrusted" || name == "mismatched" {
				state.LastForkRequestID = 0
			}
			tr := &progressTracker{id: 100}
			tr.list = func(context.Context) ([]*tracker.Comment, error) {
				if len(events) == 0 || events[0].Status != "posted" {
					t.Fatal("poll before posted")
				}
				now = now.Add(3 * time.Second)
				switch name {
				case "poll-error":
					return nil, errors.New("poll failure")
				case "canceled":
					cancel()
					return nil, context.Canceled
				case "fallback", "timeout":
					return nil, nil
				}
				return []*tracker.Comment{comment}, nil
			}
			if name == "post-error" {
				tr.postError = errors.New("post failure")
			}
			if name == "invalid-id" {
				tr.id = 0
			}
			store := progressStore{state: state}
			if name == "store-error" {
				store.err = errors.New("store failure")
			}
			g := Gateway{Tracker: tr, Store: store, PollTimeout: 10 * time.Millisecond, progressNow: func() time.Time { return now }, OnProgress: func(e ProgressEvent) { events = append(events, e) }}
			g.Dispatcher = func(context.Context, int, *tracker.Comment) error {
				if len(events) != 1 || events[0].Status != "posted" {
					t.Fatal("dispatch before posted")
				}
				return nil
			}
			_, err := g.Apply(ctx, req)
			want := ""
			switch name {
			case "confirmed", "fallback":
				want = "accepted"
				if err != nil {
					t.Fatal(err)
				}
			case "rejected":
				want = "rejected"
				if !errors.Is(err, lease.ErrClaimFence) {
					t.Fatalf("lost sentinel: %v", err)
				}
				if err == nil {
					t.Fatal("missing rejection")
				}
			}
			if name == "timeout" {
				for _, event := range events {
					if event.Status == "accepted" || event.Status == "rejected" {
						t.Fatalf("timeout reported an outcome: %+v", events)
					}
				}
				expected := "no response from the upstream fork coordinator to request 100 (release) within 10ms; check the upstream repository's SwarmOS Fork Coordinator Actions run for that request: context deadline exceeded"
				if err == nil || err.Error() != expected || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			}
			if name == "post-error" || name == "invalid-id" {
				if len(events) != 0 || err == nil {
					t.Fatalf("events=%v err=%v", events, err)
				}
				return
			}
			count := 1
			if want != "" {
				count++
			}
			if len(events) != count {
				t.Fatalf("events=%+v err=%v", events, err)
			}
			if want != "" && (events[1].Status != want || events[1].Elapsed != 3*time.Second) {
				t.Fatal(events)
			}
		})
	}
}

func TestGatewayProgressHeartbeatDuringDispatch(t *testing.T) {
	ticks := make(chan time.Time)
	events := make(chan ProgressEvent, 8)
	var mu sync.Mutex
	now := time.Now()
	g := Gateway{Tracker: &pollingTracker{list: func(context.Context) ([]*tracker.Comment, error) { return nil, errors.New("stop") }}, Store: emptyPollingStore{}, progressTicks: ticks, progressNow: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }, OnProgress: func(e ProgressEvent) { events <- e }}
	g.Dispatcher = func(context.Context, int, *tracker.Comment) error {
		e := <-events
		if e.Status != "posted" {
			t.Error(e)
		}
		for i := 1; i <= 2; i++ {
			mu.Lock()
			now = now.Add(15 * time.Second)
			mu.Unlock()
			ticks <- time.Time{}
			e = <-events
			if e.Status != "waiting" || e.Elapsed != time.Duration(i)*15*time.Second || e.Operation != "release" || e.RequestID != 100 {
				t.Error(e)
			}
		}
		return nil
	}
	_, err := g.Apply(t.Context(), lease.ForkRequest{Operation: "release", IssueNumber: 1, ClaimID: "private"})
	if err == nil || !strings.Contains(err.Error(), "stop") {
		t.Fatal(err)
	}
	select {
	case e := <-events:
		t.Fatal("late event", e)
	default:
	}
}

func TestGatewayProgressHeartbeatDuringPoll(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticks := make(chan time.Time)
	events := make(chan ProgressEvent, 8)
	polling, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	now := time.Now()
	tr := &pollingTracker{list: func(ctx context.Context) ([]*tracker.Comment, error) {
		close(polling)
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	state := lease.IssueState{LastForkRequestID: 100}
	state.Lease.Status = "released"
	g := Gateway{Tracker: tr, Store: progressStore{state: state}, progressTicks: ticks,
		progressNow: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		OnProgress:  func(e ProgressEvent) { events <- e },
	}
	done := make(chan error, 1)
	go func() {
		_, err := g.Apply(ctx, lease.ForkRequest{Operation: "release", IssueNumber: 1, ClaimID: "private"})
		done <- err
	}()
	next := func(status string, elapsed time.Duration) {
		t.Helper()
		select {
		case e := <-events:
			if e.Status != status || e.Elapsed != elapsed || e.Operation != "release" || e.RequestID != 100 {
				t.Fatalf("unexpected progress: %+v", e)
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for progress")
		}
	}
	next("posted", 0)
	select {
	case <-polling:
	case <-ctx.Done():
		t.Fatal("poll did not start")
	}
	for i := 1; i <= 2; i++ {
		mu.Lock()
		now = now.Add(15 * time.Second)
		mu.Unlock()
		select {
		case ticks <- time.Time{}:
		case <-ctx.Done():
			t.Fatal("reporter did not receive tick")
		}
		next("waiting", time.Duration(i)*15*time.Second)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Apply did not finish")
	}
	next("accepted", 30*time.Second)
	select {
	case e := <-events:
		t.Fatal("late event", e)
	default:
	}
}
