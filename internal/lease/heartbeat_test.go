package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/claimpause"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/tracker"
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
	fire(8 * time.Hour / 3)
	<-delays
	current, err := coord.GetCurrentState(t.Context(), n)
	if err != nil || current.Lease.Renewals != 1 || !current.Lease.ExpiresAt.Equal(base.Add(20*time.Hour/3)) || current.ClaimBudget != nil {
		t.Fatalf("renewal: %+v %v", current, err)
	}
	// The coordinator refuses a second due renewal and the worker stops immediately.
	fire(16 * time.Hour / 3)
	select {
	case <-h.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("max renewal did not stop")
	}
	if !errors.Is(h.Err(), ErrMaxRenewals) {
		t.Fatalf("pause cause: %v", h.Err())
	}
	current, err = coord.GetCurrentState(t.Context(), n)
	if err != nil || current.Lease.Renewals != 1 || !current.Lease.ExpiresAt.Equal(base.Add(20*time.Hour/3)) || current.Lease.Status != "active" {
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

func TestHeartbeatStartupLoadsOnce(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint("stale=", stale), func(t *testing.T) {
			m, _, issue := testManager(t)
			if _, err := m.Acquire(t.Context(), issue.Number, "local-dev", "mock", "plan", time.Hour, 3); err != nil {
				t.Fatal(err)
			}
			if stale {
				m.BindClaim(issue.Number, "stale")
			}
			store := &checkedStateStore{Store: m.Store()}
			m.SetStore(m.repoRoot, store)
			scheduled := make(chan struct{}, 1)
			m.SetHeartbeatTimerFunc(func(time.Duration) HeartbeatTimer {
				scheduled <- struct{}{}
				return firedTimer{c: make(chan time.Time)}
			})
			h := m.StartHeartbeat(t.Context(), issue.Number, "local-dev", time.Hour)
			defer h.Stop()
			if stale {
				select {
				case <-h.Context().Done():
				case <-time.After(5 * time.Second):
					t.Fatal("stale startup did not stop")
				}
				if !errors.Is(h.Err(), ErrClaimFence) {
					t.Fatalf("startup error: %v", h.Err())
				}
				select {
				case <-scheduled:
					t.Fatal("stale startup scheduled timer")
				default:
				}
			} else {
				select {
				case <-scheduled:
				case <-time.After(5 * time.Second):
					t.Fatal("startup did not schedule timer")
				}
			}
			if got := store.loads.Load(); got != 1 {
				t.Fatalf("startup loads=%d, want 1", got)
			}
		})
	}
}

// heartbeatFaultStore arms faults after startup without racing the heartbeat.
type heartbeatFaultStore struct {
	coordination.Store
	mu          sync.Mutex
	fault       error
	block       *activityBlockingStore
	casFault    error
	commitFault bool
	writes      atomic.Int64
}

func (s *heartbeatFaultStore) arm(err error) { s.mu.Lock(); defer s.mu.Unlock(); s.fault = err }
func (s *heartbeatFaultStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	s.mu.Lock()
	err := s.fault
	block := s.block
	s.mu.Unlock()
	if block != nil {
		return block.Load(ctx, n)
	}
	if err != nil {
		return nil, "", err
	}
	return s.Store.Load(ctx, n)
}

func (s *heartbeatFaultStore) CompareAndSwap(ctx context.Context, n int, rev string, raw []byte) (string, error) {
	s.writes.Add(1)
	s.mu.Lock()
	err, commit := s.casFault, s.commitFault
	s.mu.Unlock()
	if err != nil && !commit {
		return "", err
	}
	next, writeErr := s.Store.CompareAndSwap(ctx, n, rev, raw)
	if writeErr != nil {
		return next, writeErr
	}
	return next, err
}

type heartbeatRoundTripper func(*http.Request) (*http.Response, error)

func (f heartbeatRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func heartbeatProducerError(t *testing.T) error {
	t.Helper()
	client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "dummy-token", Transport: heartbeatRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "example.invalid"}}
	})})
	if err != nil {
		t.Fatal(err)
	}
	tr := tracker.NewGitHubTrackerWithClient("o", "r", client)
	_, err = tr.GetIssue(t.Context(), 1)
	var op *net.OpError
	if !errors.As(err, &op) {
		t.Fatalf("producer error: %T %v", err, err)
	}
	return err
}

type heartbeatTick struct {
	timer *controlledHeartbeatTimer
	delay time.Duration
}

func heartbeatReceive(t *testing.T, ticks <-chan heartbeatTick) heartbeatTick {
	t.Helper()
	select {
	case tick := <-ticks:
		return tick
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not schedule")
		return heartbeatTick{}
	}
}
func TestHeartbeatTransportRecovery(t *testing.T) {
	fault := heartbeatProducerError(t)
	m, _, iss := testManager(t)
	base := time.Now()
	var elapsed atomic.Int64
	m.SetNowFunc(func() time.Time { return base.Add(time.Duration(elapsed.Load())) })
	if _, err := m.Acquire(t.Context(), iss.Number, "local-dev", "mock", "implement", 4*time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	store := &heartbeatFaultStore{Store: m.Store()}
	m.SetStore(m.repoRoot, store)
	ticks := make(chan heartbeatTick, 10)
	m.SetHeartbeatTimerFunc(func(d time.Duration) HeartbeatTimer {
		timer := &controlledHeartbeatTimer{make(chan time.Time, 1)}
		ticks <- heartbeatTick{timer, d}
		return timer
	})
	h := m.StartHeartbeat(t.Context(), iss.Number, "local-dev", 4*time.Hour)
	defer h.Stop()
	tick := heartbeatReceive(t, ticks)
	store.arm(fault)
	elapsed.Store(int64(time.Hour))
	tick.timer.ch <- base.Add(time.Hour)
	select {
	case <-h.Context().Done():
		t.Fatalf("transient read canceled work: %v", h.Err())
	case tick = <-ticks:
	case <-time.After(5 * time.Second):
		t.Fatal("no retry")
	}
	if tick.delay != time.Second {
		t.Fatalf("retry delay=%s", tick.delay)
	}
	store.arm(nil)
	elapsed.Add(int64(time.Second))
	tick.timer.ch <- base.Add(time.Duration(elapsed.Load()))
	heartbeatReceive(t, ticks)
	if h.Context().Err() != nil {
		t.Fatal(h.Err())
	}
	st, err := m.GetCurrentState(t.Context(), iss.Number)
	if err != nil || st.Lease.Renewals != 0 {
		t.Fatalf("early renewal: %+v %v", st, err)
	}
}

// heartbeatRig synchronizes each iteration through its next scheduled timer.
type heartbeatRig struct {
	m       *Manager
	n       int
	base    time.Time
	elapsed atomic.Int64
	store   *heartbeatFaultStore
	ticks   chan heartbeatTick
	h       *Heartbeat
}

func newHeartbeatRig(t *testing.T, d time.Duration, renewals int) *heartbeatRig {
	t.Helper()
	m, _, iss := testManager(t)
	r := &heartbeatRig{m: m, n: iss.Number, base: time.Now(), ticks: make(chan heartbeatTick, 20)}
	m.SetNowFunc(func() time.Time { return r.base.Add(time.Duration(r.elapsed.Load())) })
	if _, err := m.Acquire(t.Context(), r.n, "local-dev", "mock", "implement", d, renewals); err != nil {
		t.Fatal(err)
	}
	r.store = &heartbeatFaultStore{Store: m.Store()}
	m.SetStore(m.repoRoot, r.store)
	m.SetHeartbeatTimerFunc(func(d time.Duration) HeartbeatTimer {
		timer := &controlledHeartbeatTimer{make(chan time.Time, 1)}
		r.ticks <- heartbeatTick{timer, d}
		return timer
	})
	return r
}
func (r *heartbeatRig) start(t *testing.T, d time.Duration) heartbeatTick {
	t.Helper()
	r.h = r.m.StartHeartbeat(t.Context(), r.n, "local-dev", d)
	t.Cleanup(r.h.Stop)
	return heartbeatReceive(t, r.ticks)
}
func (r *heartbeatRig) fire(tick heartbeatTick, at time.Duration) {
	r.elapsed.Store(int64(at))
	tick.timer.ch <- r.base.Add(at)
}
func (r *heartbeatRig) state(t *testing.T) *IssueState {
	t.Helper()
	raw, _, err := r.store.Store.Load(t.Context(), r.n)
	if err != nil {
		t.Fatal(err)
	}
	var st IssueState
	if err = json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return &st
}
func heartbeatStopped(t *testing.T, h *Heartbeat) {
	t.Helper()
	select {
	case <-h.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not stop")
	}
}

func TestHeartbeatTransportSafetyBoundary(t *testing.T) {
	run := func(name string, fn func(*testing.T)) { t.Run(name, func(t *testing.T) { synctest.Test(t, fn) }) }
	fault := heartbeatProducerError(t)
	const cutoff = 4*time.Hour - 30*time.Second
	for _, offset := range []time.Duration{0, time.Nanosecond} {
		run(fmt.Sprint("cutoff+", offset), func(t *testing.T) {
			r := newHeartbeatRig(t, 4*time.Hour, 3)
			tick := r.start(t, 4*time.Hour)
			r.store.arm(fault)
			r.fire(tick, time.Hour)
			tick = heartbeatReceive(t, r.ticks)
			r.fire(tick, cutoff-time.Nanosecond)
			tick = heartbeatReceive(t, r.ticks)
			if tick.delay != time.Nanosecond || r.h.Context().Err() != nil {
				t.Fatalf("boundary delay=%v err=%v", tick.delay, r.h.Err())
			}
			r.fire(tick, cutoff+offset)
			heartbeatStopped(t, r.h)
			if !errors.Is(r.h.Err(), fault) || errors.Is(r.h.Err(), claimpause.ErrBudget) {
				t.Fatalf("cutoff cause: %v", r.h.Err())
			}
			if st := r.state(t); !st.Lease.ExpiresAt.Equal(r.base.Add(4*time.Hour)) || st.Lease.Renewals != 0 {
				t.Fatalf("outage extended claim: %+v", st.Lease)
			}
		})
	}
	run("recovery immediately before cutoff", func(t *testing.T) {
		r := newHeartbeatRig(t, 4*time.Hour, 3)
		tick := r.start(t, 4*time.Hour)
		r.store.arm(fault)
		r.fire(tick, time.Hour)
		tick = heartbeatReceive(t, r.ticks)
		r.store.arm(nil)
		r.fire(tick, cutoff-time.Nanosecond)
		heartbeatReceive(t, r.ticks)
		if r.h.Context().Err() != nil || r.state(t).Lease.Renewals != 1 {
			t.Fatalf("recovery failed: %v", r.h.Err())
		}
	})
	run("startup without checked expiry", func(t *testing.T) {
		r := newHeartbeatRig(t, 4*time.Hour, 3)
		r.store.arm(fault)
		r.h = r.m.StartHeartbeat(t.Context(), r.n, "local-dev", 4*time.Hour)
		defer r.h.Stop()
		heartbeatStopped(t, r.h)
		if !errors.Is(r.h.Err(), fault) {
			t.Fatal(r.h.Err())
		}
		select {
		case <-r.ticks:
			t.Fatal("startup scheduled retry")
		default:
		}
	})
	for _, parent := range []bool{false, true} {
		run(fmt.Sprint("stop retry parent=", parent), func(t *testing.T) {
			r := newHeartbeatRig(t, 4*time.Hour, 3)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r.h = r.m.StartHeartbeat(ctx, r.n, "local-dev", 4*time.Hour)
			defer r.h.Stop()
			tick := heartbeatReceive(t, r.ticks)
			r.store.arm(fault)
			r.fire(tick, time.Hour)
			heartbeatReceive(t, r.ticks)
			if parent {
				cancel()
			} else {
				r.h.Stop()
			}
			heartbeatStopped(t, r.h)
			if r.h.Err() != nil {
				t.Fatalf("intentional cancellation recorded failure: %v", r.h.Err())
			}
		})
	}
	run("blocked retry bounded by cached expiry", func(t *testing.T) {
		r := newHeartbeatRig(t, time.Second, 3)
		tick := r.start(t, time.Second)
		r.store.arm(fault)
		r.fire(tick, 800*time.Millisecond)
		tick = heartbeatReceive(t, r.ticks)
		block := &activityBlockingStore{Store: r.store.Store, entered: make(chan context.Context, 1), release: make(chan struct{})}
		// The store wrapper stays fixed; switching its blocking mode is synchronized.
		r.store.mu.Lock()
		r.store.fault = nil
		r.store.block = block
		r.store.mu.Unlock()
		r.fire(tick, 900*time.Millisecond)
		select {
		case ctx := <-block.entered:
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 20*time.Millisecond {
				t.Fatalf("unbounded attempt: %v %v", deadline, ok)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("read did not block")
		}
		heartbeatStopped(t, r.h)
		if !errors.Is(r.h.Err(), fault) {
			t.Fatalf("lost original outage: %v", r.h.Err())
		}
	})
}

func TestHeartbeatDefaultPolicyRenewalWindow(t *testing.T) {
	r := newHeartbeatRig(t, 4*time.Hour, 3)
	tick := r.start(t, 4*time.Hour)
	const window = 4 * time.Hour / 3
	for used := 0; used < 3; used++ {
		st := r.state(t)
		due := st.Lease.ExpiresAt.Sub(r.base) - window
		r.fire(tick, due-time.Nanosecond)
		tick = heartbeatReceive(t, r.ticks)
		if got := r.state(t).Lease.Renewals; got != used || tick.delay != time.Nanosecond {
			t.Fatalf("above window: renewals=%d delay=%v", got, tick.delay)
		}
		r.fire(tick, due)
		tick = heartbeatReceive(t, r.ticks)
		st = r.state(t)
		if st.Lease.Renewals != used+1 || !st.Lease.ExpiresAt.Equal(r.base.Add(due+4*time.Hour)) {
			t.Fatalf("at window: %+v", st.Lease)
		}
	}
	st := r.state(t)
	r.fire(tick, st.Lease.ExpiresAt.Sub(r.base)-window)
	heartbeatStopped(t, r.h)
	if !errors.Is(r.h.Err(), claimpause.ErrBudget) || r.state(t).Lease.Renewals != 3 || r.store.writes.Load() != 3 {
		t.Fatalf("final reserve: %v %+v", r.h.Err(), r.state(t).Lease)
	}
}

type heartbeatFaultTracker struct {
	tracker.Tracker
	mu    sync.Mutex
	fault error
}

func (f *heartbeatFaultTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	f.mu.Lock()
	err := f.fault
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Tracker.GetIssue(ctx, n)
}
func (f *heartbeatFaultTracker) arm(err error) { f.mu.Lock(); defer f.mu.Unlock(); f.fault = err }
func TestHeartbeatTransportApprovalAndRenewalRecovery(t *testing.T) {
	fault := heartbeatProducerError(t)
	for _, path := range []string{"approval", "renewal", "committed renewal"} {
		t.Run(path, func(t *testing.T) {
			r := newHeartbeatRig(t, 4*time.Hour, 3)
			tr := &heartbeatFaultTracker{Tracker: r.m.tracker}
			r.m.tracker = tr
			tick := r.start(t, 4*time.Hour)
			if path == "approval" {
				tr.arm(fault)
			} else {
				r.store.mu.Lock()
				r.store.casFault = fault
				r.store.commitFault = path == "committed renewal"
				r.store.mu.Unlock()
			}
			const due = 8 * time.Hour / 3
			r.fire(tick, due)
			tick = heartbeatReceive(t, r.ticks)
			if r.h.Context().Err() != nil || tick.delay != time.Second {
				t.Fatalf("%s canceled: %v", path, r.h.Err())
			}
			tr.arm(nil)
			r.store.mu.Lock()
			r.store.casFault = nil
			r.store.mu.Unlock()
			r.fire(tick, due+time.Second)
			heartbeatReceive(t, r.ticks)
			st := r.state(t)
			if st.Lease.Renewals != 1 {
				t.Fatalf("duplicate or missing renewal: %+v", st.Lease)
			}
			wantCalls := 1
			if path == "renewal" {
				wantCalls = 2
			}
			if r.store.writes.Load() != int64(wantCalls) {
				t.Fatalf("CAS requests=%d want %d", r.store.writes.Load(), wantCalls)
			}
		})
	}
}

func heartbeatRESTError(t *testing.T, cause error, status int, mutation bool) error {
	t.Helper()
	client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "dummy-token", Transport: heartbeatRoundTripper(func(req *http.Request) (*http.Response, error) {
		if cause != nil {
			return nil, cause
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"message":"fixture"}`)), Request: req}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	tr := tracker.NewGitHubTrackerWithClient("o", "r", client)
	if mutation {
		_, err = tr.CreateComment(t.Context(), 1, "fixture")
	} else {
		_, err = tr.GetIssue(t.Context(), 1)
	}
	if err == nil {
		t.Fatal("REST fixture did not fail")
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("REST transport cause lost: %v", err)
	}
	if cause == nil {
		var httpErr *api.HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != status {
			t.Fatalf("REST HTTP cause lost: %v", err)
		}
	}
	return err
}
func heartbeatExhausted(t *testing.T, op string, cause error) error {
	t.Helper()
	var policy gitcredential.RetryPolicy
	ctx := t.Context()
	again, err := policy.Next(ctx, gitcredential.RetryObservation{Operation: "GET", Err: cause, Status: 429, Header: http.Header{"Retry-After": []string{"61"}}})
	if again || err != nil {
		t.Fatalf("exhaustion fixture: %v %v", again, err)
	}
	result := policy.Error(ctx, op, cause)
	var exhausted *gitcredential.RetryExhaustedError
	if !errors.As(result, &exhausted) || exhausted.Attempts != 1 || !errors.Is(result, cause) {
		t.Fatalf("bad exhausted shape: %v", result)
	}
	return result
}
func heartbeatTransportRoutes(t *testing.T, err error) {
	t.Helper()
	for _, path := range []string{"read", "approval", "renewal"} {
		t.Run(path, func(t *testing.T) {
			r := newHeartbeatRig(t, 4*time.Hour, 3)
			tr := &heartbeatFaultTracker{Tracker: r.m.tracker}
			r.m.tracker = tr
			tick := r.start(t, 4*time.Hour)
			at := time.Hour
			switch path {
			case "read":
				r.store.arm(err)
			case "approval":
				tr.arm(err)
			case "renewal":
				at = 8 * time.Hour / 3
				r.store.mu.Lock()
				r.store.casFault = err
				r.store.mu.Unlock()
			}
			r.fire(tick, at)
			tick = heartbeatReceive(t, r.ticks)
			if tick.delay != time.Second || r.h.Context().Err() != nil {
				t.Fatalf("producer canceled heartbeat: %v", r.h.Err())
			}
			// A persistent failure retains the producer cause at the cached cutoff.
			r.fire(tick, 4*time.Hour-30*time.Second)
			heartbeatStopped(t, r.h)
			if !errors.Is(r.h.Err(), err) {
				t.Fatalf("cutoff lost producer cause: %v", r.h.Err())
			}
		})
	}

}

func TestHeartbeatTransportError(t *testing.T) {
	live := t.Context()
	dns := heartbeatProducerError(t)
	// A real shared retry budget preserves the production REST error chain.
	exhaustedREST := heartbeatExhausted(t, "GitHub REST GET", dns)
	positives := map[string]error{"DNS without timeout": dns, "exhausted REST GET": exhaustedREST}
	for _, cause := range []error{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, &net.OpError{Op: "read", Net: "tcp", Err: context.DeadlineExceeded}, io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.EPIPE} {
		for _, mutation := range []bool{false, true} {
			name := fmt.Sprintf("%v mutation=%v", cause, mutation)
			positives[name] = heartbeatRESTError(t, cause, 0, mutation)
		}
	}
	for _, code := range []int{500, 502, 503, 504} {
		for _, mutation := range []bool{false, true} {
			positives[fmt.Sprintf("HTTP %d mutation=%v", code, mutation)] = heartbeatRESTError(t, nil, code, mutation)
		}
	}
	for name, err := range positives {
		t.Run(name, func(t *testing.T) {
			if !heartbeatTransportError(live, fmt.Errorf("wrapped: %w", err)) {
				t.Fatalf("transport rejected: %T %v", err, err)
			}
			heartbeatTransportRoutes(t, err)
		})
	}
	negatives := map[string]error{
		"claim fence":                   fmt.Errorf("fenced: %w", errors.Join(ErrClaimFence, exhaustedREST)),
		"reverse joined fence":          errors.Join(exhaustedREST, ErrClaimFence),
		"maximum renewals":              fmt.Errorf("refused: %w", ErrMaxRenewals),
		"max joined network":            errors.Join(exhaustedREST, ErrMaxRenewals),
		"task approval":                 &TaskApprovalError{IssueNumber: 1, Err: exhaustedREST},
		"plan approval":                 &PlanApprovalError{Err: exhaustedREST},
		"human approval":                &HumanPlanApprovalError{Err: exhaustedREST},
		"approved policy":               fmt.Errorf("load approved policy: %w", exhaustedREST),
		"approved budget":               fmt.Errorf("load approved budget policy: %w", exhaustedREST),
		"unknown coordinator rejection": fmt.Errorf("fork coordinator rejected renew: %w", exhaustedREST),
		"conflict":                      errors.Join(coordination.ErrConflict, exhaustedREST),
		"unknown publication":           errors.Join(coordination.ErrPublicationUnknown, exhaustedREST),
		"interrupted":                   errors.Join(gitcredential.ErrInterrupted, dns),
		"cleanup incomplete":            errors.Join(gitcredential.ErrCleanupIncomplete, dns),
		"known result cleanup":          &gitcredential.KnownResultCleanupError{ExitCode: 0},
		"approval loss":                 ErrApprovalRequired, "untrusted issue": ErrUntrustedIssue, "store configuration": ErrStoreUnconfigured,
		"unknown network text":                errors.New("network outage timeout"),
		"local filesystem wrapping transport": &os.PathError{Op: "open", Path: "policy", Err: dns},
		"JSON marshaler wrapping transport":   &json.MarshalerError{Type: reflect.TypeOf(""), Err: dns},
		"unknown branch joined network":       errors.Join(errors.New("invalid policy"), dns),
		"unknown URL error":                   &url.Error{Op: "Get", URL: "u", Err: errors.New("invalid response")},
		"plain git exit":                      &exec.ExitError{},
		"canceled":                            context.Canceled,
	}
	for _, code := range []int{401, 403, 404, 422, 429} {
		err := heartbeatRESTError(t, nil, code, false)
		var status *api.HTTPError
		if !errors.As(err, &status) || status.StatusCode != code {
			t.Fatalf("HTTP shape: %v", err)
		}
		negatives[fmt.Sprint("HTTP ", code)] = err
		negatives[fmt.Sprint("joined HTTP ", code)] = errors.Join(dns, err)
		if code == 429 {
			negatives["exhausted 429"] = heartbeatExhausted(t, "GitHub REST GET", err)
		}
	}
	for name, err := range negatives {
		t.Run(name, func(t *testing.T) {
			if heartbeatTransportError(live, err) {
				t.Fatalf("fatal error retried: %v", err)
			}
			r := newHeartbeatRig(t, 4*time.Hour, 3)
			tick := r.start(t, 4*time.Hour)
			r.store.arm(err)
			r.fire(tick, time.Hour)
			heartbeatStopped(t, r.h)
			if !errors.Is(r.h.Err(), err) {
				t.Fatalf("fatal cause lost: %v", r.h.Err())
			}
			select {
			case <-r.ticks:
				t.Fatal("fatal scheduled retry")
			default:
			}
		})
	}

	canceled, cancel := context.WithCancel(live)
	cancel()
	if heartbeatTransportError(canceled, dns) {
		t.Fatal("canceled context retried")
	}
	expired, stop := context.WithDeadline(live, time.Now().Add(-time.Second))
	defer stop()
	if heartbeatTransportError(expired, context.DeadlineExceeded) {
		t.Fatal("attempt deadline retried")
	}
}
func TestHeartbeatFatalErrors(t *testing.T) {
	fault := heartbeatProducerError(t)
	for name, err := range map[string]error{
		"wrapped fence": fmt.Errorf("wrapped: %w", ErrClaimFence), "wrapped maximum renewals": fmt.Errorf("wrapped: %w", ErrMaxRenewals),
		"approval loss": &TaskApprovalError{}, "fence joined transport": errors.Join(fault, ErrClaimFence),
		"approved policy": fmt.Errorf("load approved policy: %w", fault), "unknown error": errors.New("invalid state JSON"),
		"authentication": heartbeatRESTError(t, nil, 401, false), "permission": heartbeatRESTError(t, nil, 403, false),
		"ambiguous mutation": errors.Join(coordination.ErrPublicationUnknown, fault), "cleanup": errors.Join(gitcredential.ErrCleanupIncomplete, fault),
	} {
		t.Run(name, func(t *testing.T) {
			r := newHeartbeatRig(t, 4*time.Hour, 3)
			tick := r.start(t, 4*time.Hour)
			r.store.arm(err)
			r.fire(tick, time.Hour)
			heartbeatStopped(t, r.h)
			if !errors.Is(r.h.Err(), err) {
				t.Fatalf("fatal cause lost: %v", r.h.Err())
			}
			select {
			case <-r.ticks:
				t.Fatal("fatal scheduled retry")
			default:
			}
		})
	}
	for _, replaced := range []bool{false, true} {
		t.Run(fmt.Sprint("expired/replaced=", replaced), func(t *testing.T) {
			r := newHeartbeatRig(t, 4*time.Hour, 3)
			tick := r.start(t, 4*time.Hour)
			at := 4 * time.Hour
			if replaced {
				r.m.BindClaim(r.n, "successor")
				at = time.Hour
			}
			r.fire(tick, at)
			heartbeatStopped(t, r.h)
			if !errors.Is(r.h.Err(), ErrClaimFence) {
				t.Fatal(r.h.Err())
			}
		})
	}
}

func TestHeartbeatTransportGitProducers(t *testing.T) {
	m, _, iss := testManager(t)
	if _, err := m.Acquire(t.Context(), iss.Number, "local-dev", "mock", "implement", 4*time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	raw, _, err := m.Store().Load(t.Context(), iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "origin.git")
	for _, args := range [][]string{{"init", "--bare", remote}, {"-C", m.repoRoot, "remote", "add", "origin", remote}} {
		if out, err := fixtureGitCommand(t, args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	gs := coordination.NewGitStore(m.repoRoot)
	rev, err := gs.CompareAndSwap(t.Context(), iss.Number, "", raw)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	mode := filepath.Join(bin, "mode")
	writeMode := func(value string) {
		t.Helper()
		if err := os.WriteFile(mode, []byte(value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The wrapper intercepts only bounded network verbs; all object and policy
	// operations still run Git against a genuine temporary repository.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := `#!/bin/sh
verb=''
for arg do
 case "$arg" in fetch|ls-remote|push) verb="$arg"; break;; esac
done
read mode < ` + quote(mode) + `
case "$mode:$verb" in
 fetch:ls-remote|auth:ls-remote) printf '%040d\trefs/heads/swarm-state/issue-` + fmt.Sprint(iss.Number) + `\n' 1; exit 0;;
 fetch:fetch) echo 'Could not resolve host: example.invalid' >&2; exit 128;;
 auth:fetch) echo 'Authentication failed; connection timed out' >&2; exit 128;;
 unknown:push) touch ` + quote(mode+".pushed") + `; echo 'connection reset by peer' >&2; exit 128;;
 reset:push) echo 'connection reset by peer' >&2; exit 128;;
 unknown:ls-remote) if test -f ` + quote(mode+".pushed") + `; then echo 'Authentication failed' >&2; exit 128; fi;;
esac
exec ` + quote(realGit) + ` "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeMode("fetch")
	start := time.Now()
	var fetchErr error
	synctest.Test(t, func(t *testing.T) { _, _, fetchErr = gs.Load(t.Context(), iss.Number) })
	var exhausted *gitcredential.RetryExhaustedError
	if !errors.As(fetchErr, &exhausted) || exhausted.Operation != "git fetch" || !heartbeatTransportError(t.Context(), fetchErr) {
		t.Fatalf("fetch shape: %T %v", fetchErr, fetchErr)
	}
	t.Logf("real Git fetch exhaustion: %s", time.Since(start))
	writeMode("auth")
	_, _, authErr := gs.Load(t.Context(), iss.Number)
	var exit *exec.ExitError
	if !errors.As(authErr, &exit) || errors.As(authErr, &exhausted) || heartbeatTransportError(t.Context(), authErr) {
		t.Fatalf("authentication shape: %v", authErr)
	}
	writeMode("reset")
	m.SetStore(m.repoRoot, gs)
	_, pushErr := m.Renew(t.Context(), iss.Number, "local-dev", 4*time.Hour)
	if !errors.As(pushErr, &exit) || errors.Is(pushErr, coordination.ErrPublicationUnknown) || !heartbeatTransportError(t.Context(), pushErr) {
		t.Fatalf("reconciled push shape: %v", pushErr)
	}
	// Inspection failure is ambiguous and must not enter heartbeat recovery.
	writeMode("unknown")
	_, unknownErr := gs.CompareAndSwap(t.Context(), iss.Number, rev, raw)
	if !errors.Is(unknownErr, coordination.ErrPublicationUnknown) || heartbeatTransportError(t.Context(), unknownErr) {
		t.Fatalf("unknown publication shape: %v", unknownErr)
	}
	// Restore ordinary Git before exercising producer errors on local heartbeats.
	writeMode("normal")
	for name, err := range map[string]error{"exhausted fetch": fetchErr, "reconciled push reset": pushErr} {
		t.Run(name, func(t *testing.T) {
			heartbeatTransportRoutes(t, err)
		})
	}
}

func TestHeartbeatTransportUncheckedReadDoesNotExtendExpiry(t *testing.T) {
	fault := heartbeatProducerError(t)
	r := newHeartbeatRig(t, 4*time.Hour, 3)
	tr := &heartbeatFaultTracker{Tracker: r.m.tracker}
	r.m.tracker = tr
	tick := r.start(t, 4*time.Hour)
	r.elapsed.Store(int64(time.Hour))
	if _, err := r.m.Renew(t.Context(), r.n, "local-dev", 4*time.Hour); err != nil {
		t.Fatal(err)
	}
	// The new expiry is visible to GetCurrentState, but approval cannot be checked.
	tr.arm(fault)
	r.fire(tick, 2*time.Hour)
	tick = heartbeatReceive(t, r.ticks)
	r.fire(tick, 4*time.Hour-30*time.Second)
	heartbeatStopped(t, r.h)
	if !errors.Is(r.h.Err(), fault) {
		t.Fatalf("unchecked expiry granted authority: %v", r.h.Err())
	}
}
func TestHeartbeatShorterRequestedDuration(t *testing.T) {
	r := newHeartbeatRig(t, time.Hour, 3)
	tick := r.start(t, time.Hour)
	r.fire(tick, 40*time.Minute-time.Nanosecond)
	tick = heartbeatReceive(t, r.ticks)
	if r.state(t).Lease.Renewals != 0 || tick.delay != time.Nanosecond {
		t.Fatal("short lease renewed early")
	}
	r.fire(tick, 40*time.Minute+time.Nanosecond)
	heartbeatReceive(t, r.ticks)
	if st := r.state(t); st.Lease.Renewals != 1 || !st.Lease.ExpiresAt.Equal(r.base.Add(100*time.Minute+time.Nanosecond)) {
		t.Fatalf("short renewal: %+v", st.Lease)
	}
}

func TestHeartbeatStartupWaitsForTick(t *testing.T) {
	for _, tc := range []struct {
		name           string
		duration, left time.Duration
	}{
		{"window", 4 * time.Hour, 4 * time.Hour / 3}, {"minute", 4 * time.Hour, time.Minute},
		{"short", 12 * time.Second, 2 * time.Second}, {"subsecond", time.Second, time.Second/12 + 100*time.Millisecond},
	} {
		for _, stop := range []bool{true, false} {
			t.Run(fmt.Sprint(tc.name, "/stop=", stop), func(t *testing.T) {
				r := newHeartbeatRig(t, tc.duration, 3)
				at := tc.duration - tc.left
				r.elapsed.Store(int64(at))
				before := r.state(t)
				tick := r.start(t, tc.duration)
				remaining := tc.left - min(30*time.Second, tc.duration/12)
				if r.store.writes.Load() != 0 || r.state(t).Lease.Renewals != 0 || !r.state(t).Lease.ExpiresAt.Equal(before.Lease.ExpiresAt) {
					t.Fatal("startup renewed before first tick")
				}
				if tick.delay <= 0 || tick.delay >= remaining {
					t.Fatalf("delay %v outside (0,%v)", tick.delay, remaining)
				}
				if tc.name == "minute" && tick.delay != 15*time.Second {
					t.Fatalf("delay=%v", tick.delay)
				}
				if stop {
					r.h.Stop()
					if r.store.writes.Load() != 0 {
						t.Fatal("Stop wrote")
					}
					return
				}
				r.fire(tick, at+tick.delay)
				next := heartbeatReceive(t, r.ticks)
				if r.store.writes.Load() != 1 || r.state(t).Lease.Renewals != 1 || r.h.Context().Err() != nil || next.delay <= 0 {
					t.Fatalf("due tick: writes=%d err=%v delay=%v", r.store.writes.Load(), r.h.Err(), next.delay)
				}
			})
		}
	}
}

func TestHeartbeatStartupSafety(t *testing.T) {
	for _, remaining := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond, 2 * time.Nanosecond} {
		t.Run(remaining.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newHeartbeatRig(t, 4*time.Hour, 3)
				r.elapsed.Store(int64(4*time.Hour - 30*time.Second - remaining))
				r.h = r.m.StartHeartbeat(t.Context(), r.n, "local-dev", 4*time.Hour)
				defer r.h.Stop()
				if remaining <= time.Nanosecond {
					heartbeatStopped(t, r.h)
					if !errors.Is(context.Cause(r.h.Context()), ErrClaimFence) {
						t.Fatal(r.h.Err())
					}
					select {
					case <-r.ticks:
						t.Fatal("cutoff scheduled timer")
					default:
					}
				} else {
					tick := heartbeatReceive(t, r.ticks)
					if tick.delay != time.Nanosecond {
						t.Fatalf("delay=%v", tick.delay)
					}
				}
				if r.store.writes.Load() != 0 {
					t.Fatal("startup wrote")
				}
			})
		})
	}
	t.Run("late timer", func(t *testing.T) {
		r := newHeartbeatRig(t, 4*time.Hour, 3)
		r.elapsed.Store(int64(4*time.Hour - time.Minute))
		tick := r.start(t, 4*time.Hour)
		r.store.arm(errors.New("unexpected read after cutoff"))
		r.fire(tick, 4*time.Hour-30*time.Second)
		heartbeatStopped(t, r.h)
		if !errors.Is(r.h.Err(), ErrClaimFence) || r.store.writes.Load() != 0 {
			t.Fatalf("late timer: %v", r.h.Err())
		}
	})
	t.Run("exhausted reserve", func(t *testing.T) {
		r := newHeartbeatRig(t, 4*time.Hour, 0)
		r.elapsed.Store(int64(3 * time.Hour))
		r.h = r.m.StartHeartbeat(t.Context(), r.n, "local-dev", 4*time.Hour)
		defer r.h.Stop()
		heartbeatStopped(t, r.h)
		if !errors.Is(r.h.Err(), claimpause.ErrBudget) || r.store.writes.Load() != 0 {
			t.Fatalf("reserve: %v", r.h.Err())
		}
	})
}
