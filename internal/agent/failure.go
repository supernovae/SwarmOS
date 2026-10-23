package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// runFailure keeps the original cause available to errors.Is/As without
// interpolating harness-controlled diagnostics into terminal or JSON errors.
type runFailure struct {
	role  string
	cause error
}

func (e *runFailure) Error() string {
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

func (e *runFailure) Unwrap() error { return e.cause }

func safeRunFailure(ctx context.Context, role string, result *RunResult, err error) error {
	if err == nil {
		return nil
	}
	if result != nil {
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
	return &runFailure{role: role, cause: err}
}
