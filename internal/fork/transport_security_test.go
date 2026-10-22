package fork

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

type pollingTracker struct {
	tracker.Tracker
	list  func(context.Context) ([]*tracker.Comment, error)
	calls int
}

func (p *pollingTracker) CreateComment(context.Context, int, string) (*tracker.Comment, error) {
	return &tracker.Comment{ID: 100, CreatedAt: time.Now()}, nil
}
func (p *pollingTracker) ListComments(ctx context.Context, _ int) ([]*tracker.Comment, error) {
	p.calls++
	return p.list(ctx)
}

type emptyPollingStore struct{ coordination.Store }

func (emptyPollingStore) Load(context.Context, int) ([]byte, string, error) {
	return nil, "", coordination.ErrNotFound
}

func TestGatewayDeadlineCancelsInFlightScan(t *testing.T) {
	tr := &pollingTracker{list: func(ctx context.Context) ([]*tracker.Comment, error) { <-ctx.Done(); return nil, ctx.Err() }}
	g := &Gateway{Tracker: tr, Store: emptyPollingStore{}, ForkRepo: "worker/repo", PollTimeout: 20 * time.Millisecond}
	start := time.Now()
	_, err := g.Apply(context.Background(), lease.ForkRequest{Operation: "acquire", IssueNumber: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if time.Since(start) > time.Second || tr.calls != 1 {
		t.Fatalf("scan exceeded deadline or re-polled after timeout: %d calls", tr.calls)
	}
}

func TestGatewayIgnoresMismatchedOrEditedResults(t *testing.T) {
	now := time.Now()
	for name, change := range map[string]func(*ForkResult, *tracker.Comment){
		"wrong issue":     func(r *ForkResult, _ *tracker.Comment) { r.IssueNumber = 2 },
		"wrong operation": func(r *ForkResult, _ *tracker.Comment) { r.Operation = "release" },
		"old comment":     func(_ *ForkResult, c *tracker.Comment) { c.ID = 99 },
		"edited":          func(_ *ForkResult, c *tracker.Comment) { c.UpdatedAt = now.Add(time.Second) },
		"untrusted actor": func(_ *ForkResult, c *tracker.Comment) { c.Author = "attacker" },
	} {
		t.Run(name, func(t *testing.T) {
			res := ForkResult{Version: 1, RequestID: 100, IssueNumber: 1, Operation: "acquire", Success: false, Error: "forged rejection"}
			c := &tracker.Comment{ID: 101, Author: ActionsBotLogin, AuthorType: "Bot", CreatedAt: now, UpdatedAt: now}
			change(&res, c)
			c.Body, _ = FormatResultComment(res)
			tr := &pollingTracker{list: func(context.Context) ([]*tracker.Comment, error) { return []*tracker.Comment{c}, nil }}
			g := &Gateway{Tracker: tr, Store: emptyPollingStore{}}
			since := now.Add(-time.Second)
			if _, done, err := g.checkRequestOutcome(context.Background(), lease.ForkRequest{IssueNumber: 1, Operation: "acquire"}, 100, &since); err != nil || done {
				t.Fatalf("accepted mismatched result: done=%v err=%v", done, err)
			}
		})
	}
}

func TestForkJSONRejectsTrailingMalformedData(t *testing.T) {
	req, _ := FormatRequestComment(lease.ForkRequest{Version: 1, Operation: "acquire", IssueNumber: 1})
	res, _ := FormatResultComment(ForkResult{Version: 1, RequestID: 1})
	for _, suffix := range []string{" garbage", " {}"} {
		if _, err := ParseRequestComment(req + suffix); err == nil {
			t.Fatal("accepted trailing request content")
		}
		if _, err := ParseResultComment(res + suffix); err == nil {
			t.Fatal("accepted trailing result content")
		}
	}
}
