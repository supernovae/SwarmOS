package lease

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/coordination"
)

// blockingLoadStore holds the armed Load open until its context ends; skip
// lets earlier loads pass so a test can target the initial or the reload read.
type blockingLoadStore struct {
	coordination.Store
	mu      sync.Mutex
	armed   bool
	skip    int
	entered chan struct{}
}

func (s *blockingLoadStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	s.mu.Lock()
	armed := s.armed && s.skip == 0
	if s.skip > 0 {
		s.skip--
	} else {
		s.armed = false
	}
	s.mu.Unlock()
	if armed {
		close(s.entered)
		<-ctx.Done()
		return nil, "", ctx.Err()
	}
	return s.Store.Load(ctx, n)
}

type firedTimer struct{ c chan time.Time }

func (t firedTimer) C() <-chan time.Time { return t.c }
func (t firedTimer) Stop() bool          { return true }

// Regression: Stop while the heartbeat was loading state (initially or before a
// renewal) was recorded as a failure ("context canceled"), so a deliberate stop
// looked like a lost claim and made CI flaky.
func TestHeartbeatStopDuringLoadIsNotAFailure(t *testing.T) {
	for name, skip := range map[string]int{"initial load": 0, "pre-renewal reload": 1} {
		t.Run(name, func(t *testing.T) { testHeartbeatStopDuringLoad(t, skip) })
	}
}

func testHeartbeatStopDuringLoad(t *testing.T, skip int) {
	m, _, iss := testManager(t)
	ctx := context.Background()
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	store := &blockingLoadStore{Store: m.store, armed: true, skip: skip, entered: make(chan struct{})}
	m.SetStore("", store)
	m.SetHeartbeatTimerFunc(func(time.Duration) HeartbeatTimer {
		c := make(chan time.Time, 1)
		c <- time.Now()
		return firedTimer{c: c}
	})
	h := m.StartHeartbeat(ctx, iss.Number, "local-dev", time.Hour)
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat never loaded state")
	}
	h.Stop()
	if err := h.Err(); err != nil {
		t.Fatalf("stopped heartbeat reported %v", err)
	}
}
