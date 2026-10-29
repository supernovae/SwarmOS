package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
)

// writeLeaseReminder is advisory: its only authority operation is a fresh read.
// Callers defer it before stopping the heartbeat, using the command context.
func writeLeaseReminder(ctx context.Context, read func(context.Context, int) (*lease.IssueState, error), duration time.Duration, renewable bool, issue int, opts *RootOptions, backend string, out io.Writer) {
	if duration <= 0 {
		return
	}
	state, err := read(ctx, issue)
	if err != nil || state == nil || state.Lease.ExpiresAt.IsZero() {
		return
	}
	remaining := state.Lease.ExpiresAt.Sub(opts.now())
	status := ""
	if remaining <= 0 {
		status = " (expired)"
	}
	fmt.Fprintf(out, "Claim time remaining for issue #%d: %s%s\n", issue, max(time.Duration(0), remaining).Round(time.Second), status)
	// Compare against the rational third without overflowing a multiplication.
	below := remaining < duration/3 || remaining == duration/3 && duration%3 != 0
	if renewable && below {
		fmt.Fprintf(out, "Renew: %s\n", leaseReminderRenewCommand(opts, backend, issue))
	}
}

func leaseReminderRenewCommand(opts *RootOptions, backend string, issue int) string {
	if strings.HasPrefix(backend, "github") {
		backend = "github"
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	command := "swarm --backend " + quote(backend)
	for _, flag := range []struct{ name, value string }{{"repo", opts.Repo}, {"root", opts.RepoRoot}} {
		if flag.value != "" {
			command += " --" + flag.name + " " + quote(flag.value)
		}
	}
	if backend == "github" {
		command += " --upstream-remote " + quote(opts.effectiveUpstreamRemote())
	}
	if opts.Fork {
		command += " --fork --fork-remote " + quote(opts.effectiveForkRemote())
	}
	return command + fmt.Sprintf(" issue renew %d", issue)
}
