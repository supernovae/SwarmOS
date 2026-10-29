package coordination

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIssue324LockedState(t *testing.T) {
	s := NewFileStore(t.TempDir())
	ctx := context.Background()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.WithLockedState(ctx, 1, func(b []byte, rev string, write func([]byte) error) error {
			if len(b) != 0 || rev != "" {
				return errors.New("unexpected initial state")
			}
			close(entered)
			<-release
			return write([]byte("first"))
		})
	}()
	<-entered
	saved := make(chan error, 1)
	go func() { _, err := s.CompareAndSwap(ctx, 1, "", []byte("racer")); saved <- err }()
	select {
	case err := <-saved:
		t.Fatalf("CAS bypassed lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; !errors.Is(err, ErrConflict) {
		t.Fatalf("racer: %v", err)
	}
	if err := s.WithLockedState(ctx, 1, func(_ []byte, _ string, _ func([]byte) error) error { return errors.New("stop") }); err == nil {
		t.Fatal("callback error lost")
	}
	if _, _, err := s.Load(ctx, 1); err != nil {
		t.Fatal("lock leaked:", err)
	}
}
