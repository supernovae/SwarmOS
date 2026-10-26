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
	command := "swarm"
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	for _, flag := range []struct{ name, value string }{{"backend", opts.Backend}, {"repo", opts.Repo}, {"root", opts.RepoRoot}} {
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
	command += fmt.Sprintf(" %s work %d --phase %s", resource, number, result.HandedOffToPhase)
	result.ClaimBudgetPause.ContinueCommand = command
	return result.ClaimBudgetPause.Reason + ". Continue: " + command
}
