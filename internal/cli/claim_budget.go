package cli

import (
	"fmt"
	"strings"

	"github.com/supernovae/swarmos/internal/workflow"
)

func claimBudgetGuidance(opts *RootOptions, result *workflow.WorkResult, resource string, number int) string {
	if result == nil || result.ClaimBudgetPause == nil {
		return ""
	}
	command := swarmCommand(opts) + fmt.Sprintf(" %s work %d --phase %s", resource, number, result.HandedOffToPhase)
	result.ClaimBudgetPause.ContinueCommand = command
	return result.ClaimBudgetPause.Reason + ". Continue: " + command
}

// swarmCommand is the swarm invocation, with the run's repository selection,
// that guidance tells the operator to run next.
func swarmCommand(opts *RootOptions) string {
	command := "swarm"
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	for _, flag := range []struct{ name, value string }{{"backend", opts.Backend}, {"repo", opts.Repo}, {"root", opts.RepoRoot}, {"upstream-remote", opts.UpstreamRemote}} {
		if flag.value != "" {
			command += " --" + flag.name + " " + quote(flag.value)
		}
	}
	if opts.Fork {
		command += " --fork"
	}
	if opts.ForkRemote != "" {
		command += " --fork-remote " + quote(opts.ForkRemote)
	}
	return command
}
