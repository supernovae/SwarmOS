package lease

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/claimpause"
	"github.com/supernovae/swarmos/internal/contract"
)

func TestClaimBudgetProjection(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := contract.LeaseConfig{Duration: "3h", Renewable: true, MaxRenewals: 2}
	l := contract.Lease{ClaimedAt: now, ExpiresAt: now.Add(3 * time.Hour), MaxRenewals: 2, Status: "active"}
	for _, tc := range []struct {
		name      string
		used      int
		advance   time.Duration
		status    string
		available time.Duration
		remaining int
	}{
		{"renewable", 0, time.Hour, "active", 8 * time.Hour, 2},
		{"final expiry", 2, time.Hour, "active", 2 * time.Hour, 0},
		{"reserve", 2, 2 * time.Hour, "active", time.Hour, 0},
		{"expired", 2, 10 * time.Hour, "active", 0, 0},
		{"handoff", 1, time.Hour, "handoff", 0, 1},
		{"released", 1, time.Hour, "released", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ll := l
			ll.Renewals = tc.used
			ll.Status = tc.status
			b, err := projectBudget(ll, cfg, now.Add(tc.advance))
			if err != nil {
				t.Fatal(err)
			}
			if b.Available != tc.available || b.RenewalsRemaining != tc.remaining || b.Threshold != time.Hour || b.Deadline != now.Add(9*time.Hour) || b.Remaining < 0 {
				t.Fatalf("budget: %+v", b)
			}
		})
	}
	for _, cfg := range []contract.LeaseConfig{{Duration: "0s"}, {Duration: "bad"}, {Duration: "1h", MaxRenewals: -1}, {Duration: "1h", MaxRenewals: math.MaxInt}, {Duration: "2562047h", MaxRenewals: 2}} {
		if _, _, err := policyBudget(cfg); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	cfg.MaxRenewals = 0
	l.MaxRenewals = 0
	b, err := projectBudget(l, cfg, now)
	if err != nil || b.Deadline != now.Add(3*time.Hour) || b.RenewalsRemaining != 0 {
		t.Fatalf("zero renewals: %+v %v", b, err)
	}
}

func TestTrustedClaimBudgetCeiling(t *testing.T) {
	for _, requested := range []time.Duration{time.Minute, 100 * time.Hour} {
		t.Run(requested.String(), func(t *testing.T) {
			ctx := t.Context()
			m, _, iss := testManager(t)
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			m.SetNowFunc(func() time.Time { return now })
			st, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", requested, 100)
			if err != nil {
				t.Fatal(err)
			}
			ceiling := now.Add(16 * time.Hour)
			if st.Lease.MaxRenewals != 3 || st.Lease.ExpiresAt.After(ceiling) {
				t.Fatalf("unbounded acquisition: %+v", st.Lease)
			}
			// Candidate edits cannot change policy, including the renewal count.
			if err := os.WriteFile(filepath.Join(m.repoRoot, ".swarm/config.yaml"), []byte("lease:\n  duration: 999h\n  max_renewals: 999\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				now = now.Add(time.Second)
				st, err = m.Renew(ctx, iss.Number, "local-dev", 100*time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				if !st.Lease.ExpiresAt.Equal(ceiling) {
					t.Fatalf("expiry %s != %s", st.Lease.ExpiresAt, ceiling)
				}
			}
			before, err := m.GetCurrentState(ctx, iss.Number)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.Renew(ctx, iss.Number, "local-dev", time.Hour); !errors.Is(err, ErrMaxRenewals) {
				t.Fatalf("exhausted: %v", err)
			}
			after, _ := m.GetCurrentState(ctx, iss.Number)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected renewal mutated state")
			}
			now = ceiling
			if _, err = m.Renew(ctx, iss.Number, "local-dev", time.Hour); !errors.Is(err, ErrClaimFence) {
				t.Fatalf("deadline: %v", err)
			}
		})
	}
}

type controlledHeartbeatTimer struct{ ch chan time.Time }

func (t *controlledHeartbeatTimer) C() <-chan time.Time { return t.ch }
func (t *controlledHeartbeatTimer) Stop() bool          { return true }

func TestHeartbeatFinalRenewalPausesWithoutExtraRenewal(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "heartbeat", true: "manual"}[manual], func(t *testing.T) {
			m, _, iss := testManager(t)
			ctx := context.Background()
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var elapsed atomic.Int64
			m.SetNowFunc(func() time.Time { return base.Add(time.Duration(elapsed.Load())) })
			st, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "implement", 4*time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			timers := make(chan *controlledHeartbeatTimer, 10)
			m.SetHeartbeatTimerFunc(func(time.Duration) HeartbeatTimer {
				tt := &controlledHeartbeatTimer{make(chan time.Time, 1)}
				timers <- tt
				return tt
			})
			h := m.StartHeartbeat(ctx, iss.Number, "local-dev", 4*time.Hour)
			defer h.Stop()
			first := <-timers
			elapsed.Store(int64(time.Hour))
			if manual {
				if _, err = m.Renew(ctx, iss.Number, "local-dev", 4*time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			first.ch <- base.Add(time.Hour)
			second := <-timers
			current, _ := m.GetCurrentState(ctx, iss.Number)
			if current.Lease.Renewals != 1 {
				t.Fatalf("final renewal: %+v", current.Lease)
			}
			elapsed.Store(int64(current.Lease.ExpiresAt.Sub(st.Lease.ClaimedAt) - 4*time.Hour/3))
			second.ch <- base.Add(time.Duration(elapsed.Load()))
			<-h.Context().Done()
			if !errors.Is(h.Err(), claimpause.ErrBudget) || context.Cause(h.Context()) != claimpause.ErrBudget {
				t.Fatalf("pause cause: %v", h.Err())
			}
			current, _ = m.GetCurrentState(ctx, iss.Number)
			if current.Lease.Renewals != 1 || !current.Lease.ExpiresAt.After(m.nowFn()) {
				t.Fatalf("pause lost valid claim: %+v", current.Lease)
			}
		})
	}
}

func TestForkBudgetHandoffKeepsOnlySameContributorsCheckpoint(t *testing.T) {
	m, lt, iss := testManager(t)
	ctx := t.Context()
	st, err := m.AcquireFork(ctx, iss.Number, "local-dev", "mock", "implement", "local-dev/repo", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	oldClaim := st.Lease.ClaimID
	branch := "swarm/issue-1-" + oldClaim
	if _, err = m.RecordCheckpoint(ctx, iss.Number, "local-dev", branch, st.BaseSHA); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Handoff(ctx, iss.Number, "local-dev", "implement", "implement", "claim budget"); err != nil {
		t.Fatal(err)
	}
	next, err := m.AcquireFork(ctx, iss.Number, "local-dev", "mock", "implement", "local-dev/repo", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if next.HeadSHA != st.BaseSHA || next.ResumeCheckpointBranch != branch || next.Branch == branch || next.Lease.ClaimID == oldClaim {
		t.Fatalf("lost resume identity: %+v", next)
	}
	m.BindClaim(iss.Number, oldClaim)
	if _, err = m.Handoff(ctx, iss.Number, "local-dev", "implement", "implement"); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("old claim accepted: %v", err)
	}
	m.BindClaim(iss.Number, next.Lease.ClaimID)
	if _, err = m.Handoff(ctx, iss.Number, "local-dev", "implement", "implement", "claim budget"); err != nil {
		t.Fatal(err)
	}
	other := NewManager(lt.WithUser("other", "NONE"), m.security)
	other.SetStore(m.repoRoot, m.Store())
	fresh, err := other.AcquireFork(ctx, iss.Number, "other", "mock", "implement", "other/repo", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.HeadSHA != "" || fresh.ResumeCheckpointBranch != "" {
		t.Fatalf("inherited another contributor checkpoint: %+v", fresh)
	}
}

func TestHeartbeatZeroRenewalsWatchesExpiry(t *testing.T) {
	m, _, iss := testManager(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64
	m.SetNowFunc(func() time.Time { return base.Add(time.Duration(elapsed.Load())) })
	if _, err := m.Acquire(t.Context(), iss.Number, "local-dev", "mock", "implement", 4*time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	timers := make(chan *controlledHeartbeatTimer, 10)
	m.SetHeartbeatTimerFunc(func(time.Duration) HeartbeatTimer {
		timer := &controlledHeartbeatTimer{make(chan time.Time, 1)}
		timers <- timer
		return timer
	})
	h := m.StartHeartbeat(t.Context(), iss.Number, "local-dev", 4*time.Hour)
	defer h.Stop()
	timer := <-timers
	elapsed.Store(int64(4*time.Hour - 4*time.Hour/3))
	timer.ch <- base.Add(time.Duration(elapsed.Load()))
	<-h.Context().Done()
	if !errors.Is(h.Err(), claimpause.ErrBudget) {
		t.Fatal(h.Err())
	}
	st, err := m.GetCurrentState(t.Context(), iss.Number)
	if err != nil || st.Lease.Renewals != 0 {
		t.Fatalf("unexpected renewal: %+v %v", st, err)
	}
}
