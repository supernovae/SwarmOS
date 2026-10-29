package lease

import (
	"context"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/progress"
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
