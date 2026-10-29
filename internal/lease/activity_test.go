package lease

import (
	"context"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/tracker"
	"sync"
	"testing"
	"time"
)

type activityBlockingStore struct {
	coordination.Store
	entered chan context.Context
	release chan struct{}
	cas     bool
}

func (s *activityBlockingStore) block(ctx context.Context) error {
	select {
	case s.entered <- ctx:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *activityBlockingStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	if !s.cas {
		if err := s.block(ctx); err != nil {
			return nil, "", err
		}
	}
	return s.Store.Load(ctx, n)
}
func (s *activityBlockingStore) CompareAndSwap(ctx context.Context, n int, rev string, b []byte) (string, error) {
	if s.cas {
		if err := s.block(ctx); err != nil {
			return "", err
		}
	}
	return s.Store.CompareAndSwap(ctx, n, rev, b)
}

type activityTimer struct{ ch chan time.Time }

func (t *activityTimer) C() <-chan time.Time { return t.ch }
func (t *activityTimer) Stop() bool          { return true }
func activityWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("completion timed out")
	}
}
func TestRenewalIOIsNotLabeled(t *testing.T) {
	for _, cas := range []bool{false, true} {
		t.Run(map[bool]string{false: "load", true: "cas"}[cas], func(t *testing.T) {
			m, _, iss := testManager(t)
			if _, err := m.Acquire(t.Context(), iss.Number, "local-dev", "mock", "implement", time.Hour, 2); err != nil {
				t.Fatal(err)
			}
			store := &activityBlockingStore{Store: m.Store(), entered: make(chan context.Context, 10), release: make(chan struct{}), cas: cas}
			m.SetStore(m.repoRoot, store)
			timer := &activityTimer{make(chan time.Time, 1)}
			m.SetHeartbeatTimerFunc(func(time.Duration) HeartbeatTimer { return timer })
			a := progress.NewActivity()
			ctx, cancel := context.WithCancel(progress.WithActivity(t.Context(), a))
			var h *Heartbeat
			var once sync.Once
			t.Cleanup(func() {
				cancel()
				once.Do(func() { close(store.release) })
				if h != nil {
					activityWait(t, h.done)
				}
			})
			h = m.StartHeartbeat(ctx, iss.Number, "local-dev", time.Hour)
			if cas {
				timer.ch <- time.Now()
			}
			select {
			case rctx := <-store.entered:
				if progress.ActivityFrom(rctx) != nil {
					t.Fatal("renewal tracker inherited")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("renewal did not block")
			}
			e := progress.Event{Status: "running"}
			a.Enrich(&e, time.Now())
			if e.Operation != "" {
				t.Fatal(e)
			}
			if !cas {
				done := make(chan struct{})
				go func() { defer close(done); _, _ = m.GetCurrentState(h.Context(), iss.Number) }()
				t.Cleanup(func() { cancel(); activityWait(t, done) })
				select {
				case foreground := <-store.entered:
					if progress.ActivityFrom(foreground) != a {
						t.Fatal("foreground tracker lost")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("foreground did not block")
				}
				a.Enrich(&e, time.Now())
				if e.Operation != "state read" {
					t.Fatal(e)
				}
			}
			stopped := make(chan struct{})
			go func() { h.Stop(); close(stopped) }()
			activityWait(t, stopped)
			if h.Err() != nil {
				t.Fatal(h.Err())
			}
		})
	}
}

// Each call owns its release channel. Tests acknowledge one exact semantic seam
// at a time; cancellation always releases a blocked call.
type renewalCall struct {
	kind    string
	ctx     context.Context
	release chan struct{}
}
type renewalGates struct{ calls chan renewalCall }

func (g *renewalGates) block(ctx context.Context, kind string) error {
	c := renewalCall{kind: kind, ctx: ctx, release: make(chan struct{})}
	select {
	case g.calls <- c:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-c.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type renewalStore struct {
	coordination.Store
	gates *renewalGates
}

func (s renewalStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	if err := s.gates.block(ctx, "load"); err != nil {
		return nil, "", err
	}
	return s.Store.Load(ctx, n)
}
func (s renewalStore) CompareAndSwap(ctx context.Context, n int, rev string, b []byte) (string, error) {
	if err := s.gates.block(ctx, "cas"); err != nil {
		return "", err
	}
	return s.Store.CompareAndSwap(ctx, n, rev, b)
}

type renewalTracker struct {
	tracker.Tracker
	gates *renewalGates
}

func (s renewalTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	if err := s.gates.block(ctx, "tracker"); err != nil {
		return nil, err
	}
	return s.Tracker.GetIssue(ctx, n)
}
func TestRenewalRepeatedIOAndProjectionMasking(t *testing.T) {
	for _, stopAt := range []string{"initial-read", "initial-check", "reload", "recheck", "renew-read", "renew-check", "cas", "projection", "normal"} {
		t.Run(stopAt, func(t *testing.T) {
			m, _, iss := testManager(t)
			if _, err := m.Acquire(t.Context(), iss.Number, "local-dev", "mock", "implement", time.Hour, 3); err != nil {
				t.Fatal(err)
			}
			gates := &renewalGates{calls: make(chan renewalCall)}
			m.store = renewalStore{m.store, gates}
			m.tracker = renewalTracker{m.tracker, gates}
			ready := make(chan *activityTimer, 1)
			m.SetHeartbeatTimerFunc(func(time.Duration) HeartbeatTimer {
				timer := &activityTimer{make(chan time.Time, 1)}
				ready <- timer
				return timer
			})
			a := progress.NewActivity()
			ctx, cancel := context.WithCancel(progress.WithActivity(t.Context(), a))
			h := m.StartHeartbeat(ctx, iss.Number, "local-dev", time.Hour)
			t.Cleanup(func() { cancel(); activityWait(t, h.done) })
			check := func(label, kind string) bool {
				t.Helper()
				var c renewalCall
				select {
				case c = <-gates.calls:
				case <-time.After(5 * time.Second):
					t.Fatalf("missing %s", label)
				}
				if c.kind != kind || progress.ActivityFrom(c.ctx) != nil {
					t.Fatalf("%s: kind=%s tracker=%v", label, c.kind, progress.ActivityFrom(c.ctx))
				}
				e := progress.Event{Status: "running"}
				a.Enrich(&e, time.Now())
				if e.Operation != "" {
					t.Fatal(label, e)
				}
				if label == stopAt {
					stopped := make(chan struct{})
					go func() { h.Stop(); close(stopped) }()
					activityWait(t, stopped)
					if h.Err() != nil {
						t.Fatal(h.Err())
					}
					return false
				}
				close(c.release)
				return true
			}
			if !check("initial-read", "load") || !check("initial-check", "load") || !check("initial-check-tracker", "tracker") {
				return
			}
			for range 2 {
				var timer *activityTimer
				select {
				case timer = <-ready:
				case <-time.After(5 * time.Second):
					t.Fatal("missing timer")
				}
				timer.ch <- time.Now()
				for _, step := range [][2]string{{"reload", "load"}, {"recheck", "load"}, {"recheck-tracker", "tracker"}, {"renew-read", "load"}, {"renew-check", "tracker"}, {"cas", "cas"}, {"projection", "tracker"}, {"next-check", "load"}, {"next-check-tracker", "tracker"}} {
					if !check(step[0], step[1]) {
						return
					}
				}
			}
			// The next timer acknowledges successful completion of both renewal cycles.
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("renewals did not complete")
			}
			stopped := make(chan struct{})
			go func() { h.Stop(); close(stopped) }()
			activityWait(t, stopped)
			if h.Err() != nil {
				t.Fatal(h.Err())
			}
		})
	}
}

func TestRenewalMaxRenewalsReloadMasking(t *testing.T) {
	base := time.Now()
	worker, coord, n, _ := legacyForkManager(t, func() time.Time { return base })
	// Consume the sole renewal before starting the legacy worker. Its expiry-only
	// budget still permits a renewal attempt, and the coordinator rejects it.
	if _, err := coord.Renew(t.Context(), n, "local-dev", 4*time.Hour); err != nil {
		t.Fatal(err)
	}
	stripClaimBudget(t, coord, n)
	gates := &renewalGates{calls: make(chan renewalCall)}
	worker.store = renewalStore{worker.store, gates}
	ready := make(chan *activityTimer, 1)
	worker.SetHeartbeatTimerFunc(func(time.Duration) HeartbeatTimer {
		timer := &activityTimer{make(chan time.Time, 1)}
		ready <- timer
		return timer
	})
	a := progress.NewActivity()
	ctx, cancel := context.WithCancel(progress.WithActivity(t.Context(), a))
	h := worker.StartHeartbeat(ctx, n, "local-dev", 4*time.Hour)
	t.Cleanup(func() { cancel(); activityWait(t, h.done) })
	read := func(release bool) {
		t.Helper()
		select {
		case c := <-gates.calls:
			if progress.ActivityFrom(c.ctx) != nil {
				t.Fatal("reload inherited tracker")
			}
			e := progress.Event{Status: "running"}
			a.Enrich(&e, time.Now())
			if e.Operation != "" {
				t.Fatal(e)
			}
			if release {
				close(c.release)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing renewal read")
		}
	}
	read(true)
	read(true)
	select {
	case timer := <-ready:
		timer.ch <- base
	case <-time.After(5 * time.Second):
		t.Fatal("missing timer")
	}
	read(true)
	read(true)
	// Gateway renewal returned ErrMaxRenewals; this is its explicit reload.
	read(false)
	stopped := make(chan struct{})
	go func() { h.Stop(); close(stopped) }()
	activityWait(t, stopped)
	if h.Err() != nil {
		t.Fatal(h.Err())
	}
}
