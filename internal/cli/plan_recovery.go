package cli

import (
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/workflow"
	"os"

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

func runCheckpointPlan(cmd *cobra.Command, root *RootOptions, arg string, opts workflow.WorkOptions, dryRun, asJSON bool) error {
	n, err := parseNumberArg(arg, "issue")
	if err != nil {
		return err
	}
	tr, m, err := root.resolveCheckpointManager(cmd)
	if err != nil {
		return err
	}
	repo := root.resolveRepoRoot()
	owner, err := tr.CurrentUser(cmd.Context())
	if err != nil {
		return err
	}
	token, err := lease.LoadClaimToken(repo, n)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.BindClaim(n, token)
	// The preview precedes locks, probes, dependency preparation and all writes.
	p, err := m.PrepareCheckpoint(cmd.Context(), n, owner, opts.ExpectedHead, opts.CandidateHead, false)
	if err != nil {
		return err
	}
	if !p.ClaimRequired {
		hint, err := lease.LoadClaimBaseHint(repo, n)
		if err != nil {
			return err
		}
		if hint != p.State.BaseSHA {
			return lease.ErrClaimFence
		}
	}
	cfg, err := harness.LoadConfigAt(repo, p.State.BaseSHA)
	if err != nil {
		return err
	}
	execution, err := contract.ResolveExecution(cfg, root.getenv("SWARM_EXECUTION_MODE"))
	if err != nil {
		return err
	}
	if execution.Mode != contract.ExecutionIsolated || execution.Backend != contract.BackendNative || strings.EqualFold(cfg.PlanMode, "interactive") {
		return fmt.Errorf("checkpoint planning requires native isolated Codex or harness-tools Bubblewrap and agent plan mode")
	}
	if dryRun {
		return writeJSON(cmd, map[string]any{"kind": "checkpoint-plan-dry-run", "checkpoint": p})
	}
	lock, err := lease.LockClaim(repo, n)
	if err != nil {
		return err
	}
	defer lease.UnlockClaim(lock)
	opts.IssueNumber, opts.Owner, opts.ClaimID, opts.RepoRoot, opts.Headless = n, owner, token, repo, true
	opts.OnClaim = func(st *lease.IssueState) error { return lease.SaveClaimToken(repo, n, st.Lease.ClaimID, st.BaseSHA) }
	engine := workflow.NewEngineWithManagerOptions(repo, tr, m, nil, false, "", root.engineOptions())
	result, err := engine.ExecuteWork(cmd.Context(), opts)
	if err != nil {
		return fmt.Errorf("%w; preserved checkpoint retained; retry explicit checkpoint planning or use swarm issue release %d if the claim remains held", err, n)
	}
	if asJSON {
		return writeJSON(cmd, map[string]any{"kind": "checkpoint-plan", "result": result})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d preserved-checkpoint plan staged; run swarm issue review-plan %d, then explicitly reconcile the checkpoint.\n", n, n)
	return nil
}
