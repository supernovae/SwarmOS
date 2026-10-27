package cli

import (
	"fmt"
	"strings"

	"github.com/supernovae/swarmos/internal/lease"
)

type planRecoveryError struct {
	err      error
	recovery string
}

func (e *planRecoveryError) Error() string { return e.err.Error() }
func (e *planRecoveryError) Unwrap() error { return e.err }

func planReviewRecovery(opts *RootOptions, st *lease.IssueState, resource string, number int) string {
	if st == nil || st.PendingPlanReview == nil {
		return ""
	}
	command := "swarm"
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	if opts.Backend != "" {
		command += " --backend " + quote(opts.Backend)
	}
	if opts.Repo != "" {
		command += " --repo " + quote(opts.Repo)
	}
	if opts.RepoRoot != "" {
		command += " --root " + quote(opts.RepoRoot)
	}
	if opts.UpstreamRemote != "" {
		command += " --upstream-remote " + quote(opts.UpstreamRemote)
	}
	if opts.Fork {
		command += " --fork"
	}
	if opts.ForkRemote != "" && (opts.Fork || resource == "pr") {
		command += " --fork-remote " + quote(opts.ForkRemote)
	}
	review := fmt.Sprintf("%s issue review-plan %d", command, st.Lease.IssueNumber)
	if resource == "pr" {
		review = fmt.Sprintf("%s pr work %d --phase review", command, number)
	}
	return fmt.Sprintf("%s. Revise the plan: %s %s work %d --phase plan; then run %s. Automatic retries remain subject to the per-revision cap.", st.NextPhaseReason, command, resource, number, review)
}
