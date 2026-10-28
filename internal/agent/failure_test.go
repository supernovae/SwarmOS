package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestFailureOutputHelper(t *testing.T) {
	if os.Getenv("SWARM_FAILURE_HELPER") != "1" {
		return
	}
	// Split writes, terminal escapes, and large output must never reach errors.
	fmt.Fprint(os.Stdout, "sk-")
	fmt.Fprint(os.Stderr, "synthetic-private-token\x1b]52;c;private\a")
	fmt.Fprint(os.Stdout, strings.Repeat("private-transcript", 100000))
	os.Exit(42)
}

func TestAdapterFailureWithholdsTranscriptAndPreservesExit(t *testing.T) {
	t.Setenv("SWARM_FAILURE_HELPER", "1")
	runner := &CLIAdapter{agentName: "private-config-name", binary: os.Args[0], prefixArgs: []string{"-test.run=^TestFailureOutputHelper$", "--"}, envAllow: []string{"SWARM_FAILURE_HELPER"}}
	result, err := runner.Run(t.Context(), RunRequest{Role: "planner", Mode: "headless", WorktreePath: t.TempDir(), TaskPrompt: "private-prompt"})
	if err == nil || result == nil || result.ExitCode != 42 {
		t.Fatalf("missing failure result: %+v %v", result, err)
	}
	if !strings.Contains(err.Error(), "planner agent failed (exit 42;") {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatal("exit cause was lost")
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, text := range []string{err.Error(), string(encoded)} {
		for _, secret := range []string{"synthetic-private-token", "private-transcript", "private-prompt", "\x1b", "52;c"} {
			if strings.Contains(text, secret) {
				t.Fatalf("failure leaked %q", secret)
			}
		}
	}
	if result.Output != "" || result.Command != nil {
		t.Fatal("failure retained a printable transcript or command")
	}
}

func TestSafeRunFailureHidesNestedHarnessErrors(t *testing.T) {
	for _, cause := range []error{
		errors.New("OMP RPC error: Bearer private-token\x1b[31m"),
		fmt.Errorf("private-diagnostics: %w", context.DeadlineExceeded),
	} {
		result := &RunResult{Output: "private-transcript", Command: []string{"private-prompt"}}
		err := safeRunFailure(t.Context(), "private-role", "", result, cause)
		if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "\x1b") {
			t.Fatal("unsafe error rendering")
		}
		if !errors.Is(err, cause) {
			t.Fatal("underlying cause was lost")
		}
		if errors.Is(cause, context.DeadlineExceeded) && !strings.Contains(err.Error(), "timed out") {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := safeRunFailure(ctx, "tester", "", nil, errors.New("signal: killed; private details"))
	if !errors.Is(err, context.Canceled) || err.Error() != "tester agent canceled (harness diagnostics withheld)" {
		t.Fatal(err)
	}
}

func TestProviderFailurePatterns(t *testing.T) {
	for _, row := range []struct {
		class    string
		patterns []string
	}{
		{"usage_limit", []string{"usage limit reached", "you've hit your limit", "insufficient_quota"}},
		{"rate_limit", []string{"rate limit exceeded", "rate_limit_exceeded", "too many requests"}},
		{"capacity", []string{"model is at capacity", "model is overloaded", "overloaded_error"}},
		{"network", []string{"getaddrinfo", "ENOTFOUND", "EAI_AGAIN", "temporary failure in name resolution", "could not resolve host", "connection refused", "connection reset"}},
		{"unknown", []string{"429 503 limit", "", "unmatched fixture"}},
	} {
		for _, pattern := range row.patterns {
			t.Run(pattern, func(t *testing.T) {
				raw := strings.ToUpper(pattern) + " synthetic-private-token\x1b[31m"
				if got := classifyFailure(raw); got != row.class {
					t.Fatalf("class = %q", got)
				}
				tail := newDiagnosticsTail()
				_, _ = tail.Write([]byte(raw))
				result := &RunResult{transcript: tail, Output: raw, Command: []string{raw}}
				err := safeRunFailure(context.Background(), "developer", t.TempDir(), result, errors.New("private underlying cause"), true)
				var classified interface{ FailureClass() string }
				if !errors.As(err, &classified) || classified.FailureClass() != row.class {
					t.Fatal("missing classification")
				}
				if strings.Contains(err.Error(), "synthetic-private-token") || strings.Contains(err.Error(), "private underlying cause") || strings.Contains(err.Error(), "\x1b") {
					t.Fatalf("unsafe error: %s", err)
				}
				if result.Output != "" || result.Command != nil || result.transcript != nil {
					t.Fatal("diagnostics retained")
				}
			})
		}
	}
	if classifyFailure("model is overloaded; usage limit reached; connection refused") != "usage_limit" {
		t.Fatal("precedence")
	}
}

func TestProviderFailureExclusions(t *testing.T) {
	for _, row := range []struct {
		name     string
		cause    error
		eligible bool
		want     string
	}{
		{"unknown", errors.New("exit"), true, "unknown"},
		{"unsupported", errors.New("exit"), false, ""},
		{"success", nil, true, ""},
		{"canceled", context.Canceled, true, ""},
		{"deadline", context.DeadlineExceeded, true, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			tail := newDiagnosticsTail()
			_, _ = tail.Write([]byte("model is overloaded"))
			result := &RunResult{transcript: tail}
			err := safeRunFailure(context.Background(), "developer", t.TempDir(), result, row.cause, row.eligible)
			if row.cause == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var classified interface{ FailureClass() string }
			if !errors.As(err, &classified) {
				t.Fatal("missing typed error")
			}
			want := row.want
			if row.name == "unknown" {
				want = "capacity"
			}
			if classified.FailureClass() != want {
				t.Fatalf("got %q want %q", classified.FailureClass(), want)
			}
			if !errors.Is(err, row.cause) {
				t.Fatal("cause lost")
			}
		})
	}
}
