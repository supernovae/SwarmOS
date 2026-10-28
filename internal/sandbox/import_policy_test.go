package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/claimpause"
)

func TestWritableImportPreflight(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"developer", "tester"} {
		for _, paused := range []bool{false, true} {
			t.Run(role+map[bool]string{false: "/normal", true: "/pause"}[paused], func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				for _, name := range []string{"source", "deleted", "mode"} {
					writeWorkerFixture(t, dir, name, "before")
				}
				before, err := os.Stat(filepath.Join(dir, "mode"))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(context.Canceled)
				ctx = claimpause.WithParent(ctx, t.Context())
				rejection := errors.New("protected snapshot rejected")
				checked := false
				opts := WorkerOptions{Role: role, CheckImport: func(_ context.Context, paths, deleted []string) error {
					checked = true
					sort.Strings(paths)
					if !reflect.DeepEqual(paths, []string{".swarm/active/PLAN.md", "added", "deleted", "mode", "source"}) {
						t.Fatalf("changes: %v", paths)
					}
					if !reflect.DeepEqual(deleted, []string{"deleted"}) {
						t.Fatalf("deleted: %v", deleted)
					}
					return rejection
				}}
				err = withNativeWorkspace(ctx, dir, opts, func(_ context.Context, work, _ string) error {
					writeWorkerFixture(t, work, "source", "after")
					writeWorkerFixture(t, work, "added", "new")
					writeWorkerFixture(t, work, ".swarm/active/PLAN.md", "rewrite")
					if err := os.Remove(filepath.Join(work, "deleted")); err != nil {
						return err
					}
					if err := os.Chmod(filepath.Join(work, "mode"), before.Mode().Perm()^0100); err != nil {
						return err
					}
					if paused {
						cancel(claimpause.ErrBudget)
						return context.Canceled
					}
					return nil
				})
				if !checked || !errors.Is(err, rejection) || errors.Is(err, claimpause.ErrBudget) {
					t.Fatalf("preflight: %v checked=%t", err, checked)
				}
				for _, name := range []string{"source", "deleted", "mode"} {
					data, err := os.ReadFile(filepath.Join(dir, name))
					if err != nil || string(data) != "before" {
						t.Fatalf("partial import %s: %q %v", name, data, err)
					}
				}
				after, err := os.Stat(filepath.Join(dir, "mode"))
				if err != nil || after.Mode() != before.Mode() {
					t.Fatalf("mode changed: %v", err)
				}
				entries, _ := os.ReadDir(dir)
				if len(entries) != 3 {
					t.Fatalf("partial additions: %v", entries)
				}
			})
		}
	}
}

func TestWritableImportRejectsSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkerFixture(t, dir, ".swarm/active/PLAN.md", "approved")
	writeWorkerFixture(t, dir, "source", "before")
	err := withNativeWorkspace(t.Context(), dir, WorkerOptions{Role: "developer"}, func(_ context.Context, work, _ string) error {
		writeWorkerFixture(t, work, "source", "after")
		return os.Symlink(".swarm/active/PLAN.md", filepath.Join(work, "alias"))
	})
	if err == nil || !strings.Contains(err.Error(), "alias") {
		t.Fatalf("accepted symlink: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "source"))
	if string(data) != "before" {
		t.Fatal("partial import")
	}
	data, _ = os.ReadFile(filepath.Join(dir, ".swarm/active/PLAN.md"))
	if string(data) != "approved" {
		t.Fatal("changed protected target")
	}
}
