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
		err := safeRunFailure(t.Context(), "private-role", result, cause)
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
	err := safeRunFailure(ctx, "tester", nil, errors.New("signal: killed; private details"))
	if !errors.Is(err, context.Canceled) || err.Error() != "tester agent canceled (harness diagnostics withheld)" {
		t.Fatal(err)
	}
}
