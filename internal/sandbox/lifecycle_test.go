package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTelemetryNativeWorkspaceObservesScratchBeforeCleanup(t *testing.T) {
	for _, runErr := range []error{nil, errors.New("harness failed")} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		var exited bool
		var observed string
		lifecycle := &Lifecycle{ObserveScratch: func(home string) {
			if !exited {
				t.Error("scratch observed before the harness exited")
			}
			data, err := os.ReadFile(filepath.Join(home, "session.jsonl"))
			if err != nil {
				t.Errorf("scratch removed before observation: %v", err)
			}
			observed = string(data)
		}}
		var scratchDir string
		err := withNativeWorkspace(context.Background(), dir, WorkerOptions{Lifecycle: lifecycle}, func(_ context.Context, work, scratch string) error {
			scratchDir = scratch
			if err := os.WriteFile(filepath.Join(scratch, "session.jsonl"), []byte("log"), 0o600); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(work, "keep.txt"), []byte("changed"), 0o600); err != nil {
				return err
			}
			exited = true
			return runErr
		})
		if !errors.Is(err, runErr) {
			t.Fatalf("observer changed the run outcome: %v", err)
		}
		if observed != "log" {
			t.Fatalf("observer saw %q", observed)
		}
		if _, statErr := os.Stat(scratchDir); !os.IsNotExist(statErr) {
			t.Fatalf("scratch survived cleanup: %v", statErr)
		}
		if lifecycle.Setup == nil || lifecycle.Snapshot == nil || lifecycle.Preflight != nil {
			t.Fatalf("lifecycle scopes = setup %v snapshot %v preflight %v", lifecycle.Setup, lifecycle.Snapshot, lifecycle.Preflight)
		}
		if data, _ := os.ReadFile(filepath.Join(dir, "keep.txt")); runErr == nil && string(data) != "changed" {
			t.Fatal("observer interfered with import")
		}
	}
}

func TestTelemetryNilLifecycleIsInert(t *testing.T) {
	var l *Lifecycle
	l.addSetup(time.Now())
	l.observe(t.TempDir())
	if err := withNativeWorkspace(context.Background(), t.TempDir(), WorkerOptions{}, func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
