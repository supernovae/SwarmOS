package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testRejection struct{ recoverable bool }

func (e *testRejection) Error() string           { return "rejected agent changes" }
func (e *testRejection) RecoverableImport() bool { return e.recoverable }

func TestPreserveRejectedOnlyWhenRecoverable(t *testing.T) {
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
			dir := t.TempDir()
			writeWorkerFixture(t, dir, "src/main.go", "package main\n")
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
			if err != tc.err {
				t.Fatalf("returned %v, want the import error unchanged", err)
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
