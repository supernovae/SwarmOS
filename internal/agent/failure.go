package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/sandbox"
	"os/exec"
	"strings"
)

// runFailure keeps the original cause available to errors.Is/As without
// interpolating harness-controlled diagnostics into terminal or JSON errors.
type runFailure struct {
	class string
	role  string
	cause error
	// diagnostics is coordinator-rendered text naming the saved transcript.
	diagnostics string
}

func (e *runFailure) Error() string {
	if e.diagnostics == "" {
		return e.status()
	}
	return e.status() + "; " + e.diagnostics
}

func (e *runFailure) status() string {
	var timeout *sandbox.AgentTimeoutError
	if errors.As(e.cause, &timeout) {
		return timeout.Error()
	}
	status := "failed"
	if errors.Is(e.cause, context.Canceled) {
		status = "canceled"
	} else if errors.Is(e.cause, context.DeadlineExceeded) {
		status = "timed out"
	}
	var exitErr *exec.ExitError
	if errors.As(e.cause, &exitErr) {
		return fmt.Sprintf("%s agent %s (exit %d; harness diagnostics withheld)", e.role, status, exitErr.ExitCode())
	}
	return fmt.Sprintf("%s agent %s (harness diagnostics withheld)", e.role, status)
}

func (e *runFailure) FailureClass() string { return e.class }

func (e *runFailure) Unwrap() error { return e.cause }

func safeRunFailure(ctx context.Context, role, worktree string, result *RunResult, err error, eligible ...bool) error {
	if err == nil {
		return nil
	}
	class := ""
	if len(eligible) > 0 && eligible[0] && ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		var timeout *sandbox.AgentTimeoutError
		if !errors.As(err, &timeout) {
			class = "unknown"
			if result != nil && result.transcript != nil {
				raw, _ := result.transcript.Bytes()
				class = classifyFailure(string(raw))
			}
			if result != nil && result.Telemetry != nil {
				result.Telemetry.FailureClass = class
			}
		}
	}
	diagnostics := ""
	if result != nil {
		if result.transcript != nil {
			diagnostics = failureDiagnosticsNote(worktree, result.transcript)
			result.transcript = nil
		}
		// A failed RunResult may itself be serialized by a caller.
		result.Output = ""
		result.Command = nil
	}
	switch role {
	case "planner", "reviewer", "developer", "tester":
	default:
		role = "configured"
	}
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	return &runFailure{class: class, role: role, cause: err, diagnostics: diagnostics}
}

// classifyFailure returns only fixed enums; diagnostic text never escapes.
func classifyFailure(raw string) string {
	raw = strings.ToLower(raw)
	for _, row := range []struct {
		class    string
		patterns []string
	}{
		{"usage_limit", []string{"usage limit reached", "you've hit your limit", "insufficient_quota"}},
		{"rate_limit", []string{"rate limit exceeded", "rate_limit_exceeded", "too many requests"}},
		{"capacity", []string{"model is at capacity", "model is overloaded", "overloaded_error"}},
		{"network", []string{"getaddrinfo", "enotfound", "eai_again", "temporary failure in name resolution", "could not resolve host", "connection refused", "connection reset"}},
	} {
		for _, pattern := range row.patterns {
			if strings.Contains(raw, pattern) {
				return row.class
			}
		}
	}
	return "unknown"
}

// FailureCause maps a validated classification to a fixed operator message.
func FailureCause(class string) string {
	switch class {
	case "capacity":
		return "provider model at capacity"
	case "usage_limit":
		return "provider usage limit reached"
	case "rate_limit":
		return "provider rate limit reached"
	case "network":
		return "provider network connection failed"
	}
	return ""
}
