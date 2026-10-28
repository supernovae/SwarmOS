package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/claimpause"
)

func TestNativeClaimBudgetPreservesOnlyCoordinatorPause(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"developer", "tester", "planner", "reviewer"} {
		for _, cause := range []error{claimpause.ErrBudget, context.Canceled, fmt.Errorf("paused for claim budget")} {
			t.Run(role+cause.Error(), func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				writeWorkerFixture(t, dir, "source", "before")
				writeWorkerFixture(t, dir, "deleted", "before")
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				ctx = claimpause.WithParent(ctx, context.Background())
				opts := WorkerOptions{Role: role, ReadOnly: role == "planner" || role == "reviewer"}
				checked := false
				opts.CheckImport = func(ctx context.Context, _, _ []string) error { checked = true; return ctx.Err() }
				err := withNativeWorkspace(ctx, dir, opts, func(_ context.Context, work, _ string) error {
					writeWorkerFixture(t, work, "source", "after")
					writeWorkerFixture(t, work, "added", "saved")
					if err := os.Remove(filepath.Join(work, "deleted")); err != nil {
						return err
					}
					cancel(cause)
					return context.Canceled
				})
				wantImport := cause == claimpause.ErrBudget && (role == "developer" || role == "tester")
				if wantImport {
					if !errors.Is(err, claimpause.ErrBudget) || !checked {
						t.Fatalf("pause: %v checked=%t", err, checked)
					}
					data, _ := os.ReadFile(filepath.Join(dir, "source"))
					if string(data) != "after" {
						t.Fatalf("lost edit: %q", data)
					}
					if _, err := os.Stat(filepath.Join(dir, "deleted")); !os.IsNotExist(err) {
						t.Fatal("lost deletion")
					}
					data, _ = os.ReadFile(filepath.Join(dir, "added"))
					if string(data) != "saved" {
						t.Fatal("lost addition")
					}
				} else {
					data, _ := os.ReadFile(filepath.Join(dir, "source"))
					if err == nil || string(data) != "before" || checked {
						t.Fatalf("unexpected import: %q %v", data, err)
					}
				}
			})
		}
	}
}

func TestNativeClaimBudgetRejectsUnsafeOutput(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"symlink", "control", "oversized", "protected", "fence", "harness failure"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeWorkerFixture(t, dir, "source", "before")
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			ctx = claimpause.WithParent(ctx, context.Background())
			opts := WorkerOptions{Role: "developer", CheckImport: func(context.Context, []string, []string) error {
				if kind == "protected" || kind == "fence" {
					return fmt.Errorf("rejected %s", kind)
				}
				return nil
			}}
			err := withNativeWorkspace(ctx, dir, opts, func(_ context.Context, work, _ string) error {
				writeWorkerFixture(t, work, "source", "after")
				switch kind {
				case "symlink":
					if err := os.Symlink("source", filepath.Join(work, "alias")); err != nil {
						return err
					}
				case "control":
					writeWorkerFixture(t, work, ".git/config", "spoofed")
				case "oversized":
					f, err := os.Create(filepath.Join(work, "huge"))
					if err != nil {
						return err
					}
					err = f.Truncate(workerFileBytes + 1)
					f.Close()
					if err != nil {
						return err
					}
				}
				cancel(claimpause.ErrBudget)
				if kind == "harness failure" {
					return errors.New("execution failed")
				}
				return context.Canceled
			})
			if err == nil || errors.Is(err, claimpause.ErrBudget) {
				t.Fatalf("accepted unsafe pause: %v", err)
			}
			data, _ := os.ReadFile(filepath.Join(dir, "source"))
			if string(data) != "before" {
				t.Fatalf("partial import: %s", data)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("unexpected imported files: %v", entries)
			}
			if kind == "control" && !strings.Contains(err.Error(), "control path") {
				t.Fatal(err)
			}
		})
	}
}
