package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type testRejection struct{ recoverable bool }

func (e *testRejection) Error() string           { return "rejected agent changes" }
func (e *testRejection) RecoverableImport() bool { return e.recoverable }
func (e *testRejection) RoleOutputImport() bool  { return e.recoverable }

func TestPreserveRejectedOnlyWhenRecoverable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		binary bool
		called bool
		patch  bool
	}{
		{"recoverable", &testRejection{recoverable: true}, false, true, true},
		{"wrapped recoverable", errors.Join(errors.New("x"), &testRejection{recoverable: true}), false, true, true},
		{"not recoverable", &testRejection{}, false, false, false},
		{"plain error", errors.New("protected"), false, false, false},
		{"binary change", &testRejection{recoverable: true}, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeWorkerFixture(t, dir, "src/main.go", "package main\n")
			writeWorkerFixture(t, dir, "unchanged.bin", "\x00asset")
			called := false
			var got []byte
			opts := WorkerOptions{Role: "developer",
				CheckImport: func(context.Context, []string, []string) error { return tc.err },
				PreserveRejected: func(_ context.Context, importErr error, patch []byte) {
					called = true
					got = patch
					if importErr != tc.err {
						t.Errorf("callback error %v", importErr)
					}
				}}
			err := withNativeWorkspace(t.Context(), dir, opts, func(_ context.Context, work, _ string) error {
				writeWorkerFixture(t, work, "src/main.go", "package main\n\nfunc main() {}\n")
				writeWorkerFixture(t, work, ".swarm/active/VALIDATION.md", "results\n")
				if tc.binary {
					writeWorkerFixture(t, work, "blob", "\x00\x01")
				}
				return nil
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("lost policy cause: %v", err)
			}
			var completed *CompletedImportError
			if errors.As(err, &completed) != tc.called {
				t.Fatalf("completed classification: %v", err)
			}
			if called != tc.called || (got != nil) != tc.patch {
				t.Fatalf("called=%v patch=%q", called, got)
			}
			if tc.patch {
				for _, text := range []string{"diff --git a/src/main.go b/src/main.go", "+func main() {}", "diff --git a/.swarm/active/VALIDATION.md b/.swarm/active/VALIDATION.md", "+results"} {
					if !strings.Contains(string(got), text) {
						t.Fatalf("patch missing %q:\n%s", text, got)
					}
				}
			}
			data, _ := os.ReadFile(filepath.Join(dir, "src/main.go"))
			if string(data) != "package main\n" {
				t.Fatal("rejected run was imported")
			}
		})
	}
}

func TestCompletedTesterImportRejected(t *testing.T) {
	t.Parallel()
	for _, exit := range []error{nil, errors.New("nonzero exit"), context.Canceled, context.DeadlineExceeded} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeWorkerFixture(t, dir, "source", "before")
			policy := &testRejection{recoverable: true}
			err := withNativeWorkspace(t.Context(), dir, WorkerOptions{Role: "tester", CheckImport: func(_ context.Context, paths, _ []string) error {
				if !slices.Contains(paths, ".swarm/active/IMPLEMENTATION.md") {
					t.Fatalf("paths: %v", paths)
				}
				return policy
			}}, func(_ context.Context, work, _ string) error {
				writeWorkerFixture(t, work, ".swarm/active/IMPLEMENTATION.md", "wrong output")
				writeWorkerFixture(t, work, "source", "after")
				return exit
			})
			var completed *CompletedImportError
			if errors.As(err, &completed) != (exit == nil) {
				t.Fatalf("classification: %v", err)
			}
			if exit == nil && !errors.Is(err, policy) {
				t.Fatalf("lost cause: %v", err)
			}
			if exit != nil && !errors.Is(err, exit) {
				t.Fatalf("lost exit: %v", err)
			}
			body, _ := os.ReadFile(filepath.Join(dir, "source"))
			if string(body) != "before" {
				t.Fatal("imported rejected change")
			}
		})
	}
}

type scratchRejection struct{}

func (*scratchRejection) Error() string               { return "forbidden active file" }
func (*scratchRejection) ActiveDirectoryImport() bool { return true }
func TestCompletedTesterScratchImportRejected(t *testing.T) {
	t.Parallel()
	for _, exit := range []error{nil, errors.New("nonzero exit"), context.Canceled, context.DeadlineExceeded} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeWorkerFixture(t, dir, "source", "before")
			policy := &scratchRejection{}
			err := withNativeWorkspace(t.Context(), dir, WorkerOptions{Role: "tester", PreserveRejected: func(context.Context, error, []byte) { t.Fatal("scratch rejection preserved") }, CheckImport: func(_ context.Context, paths, _ []string) error {
				if !slices.Contains(paths, ".swarm/active/VALIDATION_CHECK.log") {
					t.Fatalf("paths: %v", paths)
				}
				return policy
			}}, func(_ context.Context, work, _ string) error {
				writeWorkerFixture(t, work, ".swarm/active/VALIDATION_CHECK.log", "wrong output")
				writeWorkerFixture(t, work, "source", "after")
				return exit
			})
			var completed *CompletedImportError
			if errors.As(err, &completed) != (exit == nil) {
				t.Fatalf("classification: %v", err)
			}
			if exit == nil && !errors.Is(err, policy) {
				t.Fatalf("lost cause: %v", err)
			}
			if exit != nil && !errors.Is(err, exit) {
				t.Fatalf("lost exit: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, ".swarm/active/VALIDATION_CHECK.log")); !os.IsNotExist(statErr) {
				t.Fatalf("log imported: %v", statErr)
			}
			body, _ := os.ReadFile(filepath.Join(dir, "source"))
			if string(body) != "before" {
				t.Fatal("imported rejected change")
			}
		})
	}
}

func TestCompletedImportDiagnosticOnly(t *testing.T) {
	t.Parallel()
	for _, err := range []error{&scratchRejection{}, fmt.Errorf("wrapped: %w", &scratchRejection{}), errors.New("outside policy"), &testRejection{}} {
		for _, success := range []bool{false, true} {
			got := completedImport(err, success)
			var active *scratchRejection
			var completed *CompletedImportError
			want := success && errors.As(err, &active)
			if errors.As(got, &completed) != want || !errors.Is(got, err) {
				t.Fatalf("classification: %v", got)
			}
		}
	}
}
