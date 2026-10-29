package pr

import (
	"context"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/progress"
	"sync"
	"testing"
	"time"
)

type progressBlockStore struct {
	coordination.Store
	entered, release chan struct{}
	once             sync.Once
}

func (s *progressBlockStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return s.Store.Load(ctx, n)
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
}
func TestPRSubmitProgressShowsOperation(t *testing.T)   { testPRProgressOperation(t, true) }
func TestPRValidateProgressShowsOperation(t *testing.T) { testPRProgressOperation(t, false) }
func testPRProgressOperation(t *testing.T, submit bool) {
	s, _, number, _ := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time, 1)
	events := make(chan progress.Event, 20)
	now := time.Now().Add(time.Minute)
	p := progress.New(ctx, 0, number, func(e progress.Event) { events <- e }, ticks, func() time.Time { return now }, nil)
	s.Progress = p
	store := &progressBlockStore{Store: s.Manager.Store(), entered: make(chan struct{}), release: make(chan struct{})}
	s.Manager.SetStore(s.Root, store)
	done := make(chan struct{})
	var once sync.Once
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("completion timeout")
		}
	}
	t.Cleanup(func() {
		cancel()
		once.Do(func() { close(store.release) })
		wait(done)
		p.Finish("canceled")
		wait(p.Done())
	})
	go func() {
		defer close(done)
		if submit {
			_, _ = s.Submit(ctx, number)
		} else {
			_, _ = s.Validate(ctx, number)
		}
	}()
	wait(store.entered)
	ticks <- now
	for {
		select {
		case e := <-events:
			if e.Status != "running" {
				continue
			}
			if e.PRNumber != number || e.Operation != "state read" || e.Role != "" {
				t.Fatal(e)
			}
			cancel()
			wait(done)
			p.Finish("canceled")
			wait(p.Done())
			return
		case <-time.After(5 * time.Second):
			t.Fatal("missing heartbeat")
		}
	}
}
