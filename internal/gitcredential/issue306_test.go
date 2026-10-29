package gitcredential

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNetworkRetryRunnerDeadlineUnexpiredCaller(t *testing.T) {
	networkBudgets(t)
	repo, tr := networkFixture(t)
	tr.Set("receive", "hang")
	run := captureRun(t, tr)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := Network(ctx, repo, true, "push", "origin", "HEAD:refs/heads/swarm-state/issue-306")
	if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrInterrupted) {
		t.Fatalf("runner deadline confused with caller: caller=%v result=%v", ctx.Err(), err)
	}
	if tr.Receives() != 1 {
		t.Fatalf("receives %d", tr.Receives())
	}
	tr.Release()
	tr.Join()
	assertSettled(t, *run)
}
