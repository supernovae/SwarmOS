package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/claimpause"
)

// legacyGateway is an upstream coordinator that predates claim budgets: it
// fences renewals through a real manager but never stores a projection.
type legacyGateway struct {
	t     *testing.T
	coord *Manager
	owner string
}

func (g *legacyGateway) Apply(ctx context.Context, r ForkRequest) (*IssueState, error) {
	if r.Operation != "renew" {
		return nil, fmt.Errorf("unexpected %s", r.Operation)
	}
	d, err := time.ParseDuration(r.Duration)
	if err != nil {
		return nil, err
	}
	g.coord.BindClaim(r.IssueNumber, r.ClaimID)
	if _, err = g.coord.Renew(ctx, r.IssueNumber, g.owner, d); err != nil {
		return nil, err
	}
	stripClaimBudget(g.t, g.coord, r.IssueNumber)
	return g.coord.GetCurrentState(ctx, r.IssueNumber)
}

func stripClaimBudget(t *testing.T, m *Manager, n int) {
	t.Helper()
	raw, rev, err := m.Store().Load(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err = json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	delete(st, "claim_budget")
	if raw, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Store().CompareAndSwap(context.Background(), n, rev, raw); err != nil {
		t.Fatal(err)
	}
}

// legacyForkManager returns a fork-mode worker whose coordinator sends no
// claim budget, bound to a fresh 4h claim allowing one renewal.
func legacyForkManager(t *testing.T, now func() time.Time) (worker, coord *Manager, n int, st *IssueState) {
	t.Helper()
	coord, lt, iss := testManager(t)
	coord.SetNowFunc(now)
	st, err := coord.Acquire(t.Context(), iss.Number, "local-dev", "mock", "implement", 4*time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	stripClaimBudget(t, coord, iss.Number)
	worker = NewManager(lt, coord.security)
	worker.SetStore(coord.repoRoot, coord.Store())
	worker.SetNowFunc(now)
	worker.SetWorkerGateway(&legacyGateway{t: t, coord: coord, owner: "local-dev"})
	worker.BindClaim(iss.Number, st.Lease.ClaimID)
	return worker, coord, iss.Number, st
}

func TestWorkBudgetWithoutUpstreamBudgetUsesLeaseExpiry(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	worker, coord, n, claimed := legacyForkManager(t, func() time.Time { return now })
	st, err := worker.GetCurrentState(t.Context(), n)
	if err != nil || st.ClaimBudget != nil {
		t.Fatalf("legacy state: %+v %v", st, err)
	}
	if b, err := worker.Budget(st); b != nil || err != nil {
		t.Fatalf("missing upstream budget is unavailable, got %+v %v", b, err)
	}
	if _, err := worker.WorkBudget(st, 0); err == nil {
		t.Fatal("fallback accepted a zero lease duration")
	}
	expiry := st.Lease.ExpiresAt
	for _, tc := range []struct {
		name      string
		advance   time.Duration
		status    string
		active    bool
		available time.Duration
	}{
		{"fresh", 0, "active", true, 4 * time.Hour},
		{"reserve", 4*time.Hour - 4*time.Hour/3, "active", true, 4 * time.Hour / 3},
		{"expired", 5 * time.Hour, "active", false, 0},
		{"handoff", time.Hour, "handoff", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now = base.Add(tc.advance)
			s := *st
			s.Lease.Status = tc.status
			b, err := worker.WorkBudget(&s, 4*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if !b.LeaseExpiryOnly() || b.Active != tc.active || b.Available != tc.available || !b.Deadline.Equal(expiry) || b.Threshold != 4*time.Hour/3 || b.Available > max(0, expiry.Sub(now)) {
				t.Fatalf("fallback budget: %+v", b)
			}
		})
	}

	// A coordinator-supplied budget is used unchanged.
	now = base
	supplied, err := coord.GetCurrentState(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	supplied.ClaimBudget = claimed.ClaimBudget
	want, err := worker.Budget(supplied)
	if err != nil || want == nil {
		t.Fatalf("supplied budget: %+v %v", want, err)
	}
	got, err := worker.WorkBudget(supplied, time.Minute)
	if err != nil || *got != *want || got.LeaseExpiryOnly() {
		t.Fatalf("supplied budget changed: %+v != %+v (%v)", got, want, err)
	}
	if b, err := worker.WorkBudget(&IssueState{}, 4*time.Hour); b != nil || err != nil {
		t.Fatalf("unclaimed state: %+v %v", b, err)
	}
}

func TestHeartbeatWithoutUpstreamBudgetRenewsUntilLeaseExpiryReserve(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64
	now := func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	worker, coord, n, _ := legacyForkManager(t, now)
	timers := make(chan *controlledHeartbeatTimer, 10)
	delays := make(chan time.Duration, 10)
	worker.SetHeartbeatTimerFunc(func(d time.Duration) HeartbeatTimer {
		timer := &controlledHeartbeatTimer{make(chan time.Time, 1)}
		delays <- d
		timers <- timer
		return timer
	})
	h := worker.StartHeartbeat(t.Context(), n, "local-dev", 4*time.Hour)
	defer h.Stop()
	fire := func(at time.Duration) {
		t.Helper()
		timer := <-timers
		elapsed.Store(int64(at))
		timer.ch <- now()
	}
	if d := <-delays; d != 4*time.Hour/3 {
		t.Fatalf("first delay %s", d)
	}
	// The coordinator renews; no placeholder counter suppresses the request.
	fire(time.Hour)
	<-delays
	current, err := coord.GetCurrentState(t.Context(), n)
	if err != nil || current.Lease.Renewals != 1 || !current.Lease.ExpiresAt.Equal(base.Add(5*time.Hour)) || current.ClaimBudget != nil {
		t.Fatalf("renewal: %+v %v", current, err)
	}
	// The coordinator refuses a second renewal; nothing extends the claim.
	fire(2 * time.Hour)
	if d := <-delays; d != 4*time.Hour/3 {
		t.Fatalf("delay after refusal %s", d)
	}
	fire(5*time.Hour - 4*time.Hour/3)
	<-h.Context().Done()
	if !errors.Is(h.Err(), claimpause.ErrBudget) {
		t.Fatalf("pause cause: %v", h.Err())
	}
	current, err = coord.GetCurrentState(t.Context(), n)
	if err != nil || current.Lease.Renewals != 1 || !current.Lease.ExpiresAt.Equal(base.Add(5*time.Hour)) || current.Lease.Status != "active" {
		t.Fatalf("wind-down changed the claim: %+v %v", current, err)
	}
}

func TestHeartbeatWithoutUpstreamBudgetKeepsClaimFence(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	worker, _, n, _ := legacyForkManager(t, func() time.Time { return base })
	worker.BindClaim(n, "replaced")
	h := worker.StartHeartbeat(t.Context(), n, "local-dev", 4*time.Hour)
	defer h.Stop()
	<-h.Context().Done()
	if !errors.Is(h.Err(), ErrClaimFence) {
		t.Fatalf("stale claim: %v", h.Err())
	}
}
