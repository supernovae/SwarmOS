package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

// bulkApplyIssueCleanup is replaced by tests to inject per-issue apply faults.
var bulkApplyIssueCleanup = applyIssueCleanup

type bulkCleanupCounts struct {
	Eligible int `json:"eligible"`
	Retired  int `json:"retired"`
	Retained int `json:"retained"`
	Failed   int `json:"failed"`
}

type bulkCleanupResource struct {
	lease.CleanupResource
	// Disposition is eligible, retired, retained or failed for counted
	// resources, and absent, not-applicable or ledger for the rest.
	Disposition string `json:"disposition"`
}

type bulkCleanupIssue struct {
	IssueNumber int                   `json:"issue_number"`
	PRNumber    int                   `json:"pr_number,omitempty"`
	PRStatus    string                `json:"pr_status,omitempty"`
	Blocked     string                `json:"blocked,omitempty"`
	Error       string                `json:"error,omitempty"`
	Counts      bulkCleanupCounts     `json:"counts"`
	Resources   []bulkCleanupResource `json:"resources"`
}

type bulkCleanupSummary struct {
	Kind         string                            `json:"kind"`
	Mode         string                            `json:"mode"`
	Status       string                            `json:"status"`
	Error        string                            `json:"error,omitempty"`
	Issues       []bulkCleanupIssue                `json:"issues"`
	Unresolved   []lease.UnresolvedCleanupWorktree `json:"unresolved"`
	Totals       bulkCleanupCounts                 `json:"totals"`
	FailedIssues int                               `json:"failed_issues"`
}

// runBulkCleanup previews every managed issue and PR worktree from one PR
// listing. With apply, each issue that previews an eligible resource is
// reinspected with fresh tracker reads and retired through the single-issue
// path; issues run sequentially and a failure never stops or undoes others.
func runBulkCleanup(cmd *cobra.Command, opts *RootOptions, args []string, apply, asJSON, recoverReservation bool) (runErr error) {
	summary := &bulkCleanupSummary{Kind: "cleanup-bulk", Mode: "preview", Issues: []bulkCleanupIssue{}, Unresolved: []lease.UnresolvedCleanupWorktree{}}
	if apply {
		summary.Mode = "apply"
	}
	rendered := false
	defer func() {
		if !asJSON {
			return
		}
		if !rendered {
			summary.Status = "failed"
			if runErr != nil {
				summary.Error = runErr.Error()
			}
		}
		if e := writeJSON(cmd, summary); runErr == nil {
			runErr = e
		}
	}()
	switch {
	case len(args) > 0:
		return fmt.Errorf("--all-merged does not take an issue number")
	case recoverReservation:
		return fmt.Errorf("--all-merged and --recover-reservation are mutually exclusive; recover each issue explicitly")
	case opts.Fork:
		return fmt.Errorf("cleanup requires an upstream repository operator")
	}
	tr, m, _, err := opts.resolveLeaseManager(cmd)
	if err != nil {
		return err
	}
	if apply && m.HasWorkerGateway() {
		return fmt.Errorf("cleanup is unavailable to agent workers")
	}
	if apply {
		if err = tr.Authorize(cmd.Context(), "approve-task"); err != nil {
			return err
		}
	}
	ctx := cmd.Context()
	root := opts.resolveRepoRoot()
	worktrees, err := workspace.DiscoverCleanupWorktrees(ctx, root)
	if err != nil {
		return err
	}
	snap := m.SnapshotCleanup(ctx)
	candidates, unresolved := m.ResolveCleanupWorktrees(ctx, snap, worktrees)
	summary.Unresolved = append(summary.Unresolved, unresolved...)
	summary.Totals.Retained += len(unresolved)
	var failures []error
	for _, c := range candidates {
		n := c.IssueNumber
		preview, err := lease.InspectCleanupSnapshot(ctx, root, m, n, snap)
		if err == nil && apply && cleanupHasEligible(preview) {
			preview, err = applyBulkIssue(ctx, root, tr, m, n, preview)
		}
		result := bulkIssueResult(c, preview, err)
		if err != nil {
			failures = append(failures, fmt.Errorf("issue #%d: %w", n, err))
			summary.FailedIssues++
		}
		summary.Totals.Eligible += result.Counts.Eligible
		summary.Totals.Retired += result.Counts.Retired
		summary.Totals.Retained += result.Counts.Retained
		summary.Totals.Failed += result.Counts.Failed
		summary.Issues = append(summary.Issues, result)
	}
	runErr = errors.Join(failures...)
	switch {
	case runErr != nil:
		summary.Status = "failed"
		summary.Error = runErr.Error()
	case apply:
		summary.Status = "completed"
	default:
		summary.Status = "preview"
	}
	rendered = true
	if !asJSON {
		renderBulkCleanup(cmd.OutOrStdout(), summary)
	}
	return runErr
}

// applyBulkIssue discards the snapshot preview and applies a fresh one. On
// failure the most recent preview is returned so completed retirements count.
func applyBulkIssue(ctx context.Context, root string, tr tracker.Tracker, m *lease.Manager, n int, stale *lease.CleanupPreview) (*lease.CleanupPreview, error) {
	fresh, err := lease.InspectCleanup(ctx, root, tr, m, n)
	if err != nil {
		return stale, err
	}
	if fresh.Blocked == "" && !cleanupHasEligible(fresh) {
		return fresh, nil
	}
	_, err = bulkApplyIssueCleanup(ctx, tr, m, n, fresh)
	return fresh, err
}

func cleanupHasEligible(p *lease.CleanupPreview) bool {
	for _, r := range p.Resources {
		if r.Eligible {
			return true
		}
	}
	return false
}

func bulkIssueResult(c lease.CleanupCandidate, p *lease.CleanupPreview, err error) bulkCleanupIssue {
	result := bulkCleanupIssue{IssueNumber: c.IssueNumber, Resources: []bulkCleanupResource{}}
	if err != nil {
		result.Error = err.Error()
	}
	if p == nil {
		// Inspection failed, so the discovered worktrees were left untouched.
		for _, w := range c.Worktrees {
			r := lease.CleanupResource{Kind: "worktree", Name: w.Path, Absent: !w.Present, Reason: "inspection failed"}
			result.Resources = append(result.Resources, bulkCleanupResource{r, "failed"})
			result.Counts.Failed++
		}
		return result
	}
	result.Blocked = p.Blocked
	if p.Outcome != nil {
		result.PRNumber = p.Outcome.Number
		result.PRStatus = p.Outcome.Status
	}
	for _, r := range p.Resources {
		d := "retained"
		switch {
		case r.Retired:
			d = "retired"
			result.Counts.Retired++
		case r.Kind == "ledger":
			d = "ledger"
		case r.Kind == "remote-code-branch" && !p.RemoteApplicable:
			d = "not-applicable"
		case r.Absent:
			d = "absent"
		case r.Eligible && err != nil:
			d = "failed"
			result.Counts.Failed++
		case r.Eligible:
			d = "eligible"
			result.Counts.Eligible++
		default:
			result.Counts.Retained++
		}
		result.Resources = append(result.Resources, bulkCleanupResource{r, d})
	}
	return result
}

func renderBulkCleanup(w io.Writer, s *bulkCleanupSummary) {
	if s.Mode == "preview" {
		fmt.Fprintln(w, "cleanup preview: eligibility is advisory; --apply reinspects each issue before retiring")
	}
	for _, issue := range s.Issues {
		pr := "no recorded PR"
		if issue.PRNumber > 0 {
			pr = fmt.Sprintf("PR #%d %s", issue.PRNumber, issue.PRStatus)
		}
		c := issue.Counts
		fmt.Fprintf(w, "issue #%d (%s): eligible=%d retired=%d retained=%d failed=%d\n", issue.IssueNumber, pr, c.Eligible, c.Retired, c.Retained, c.Failed)
		for _, r := range issue.Resources {
			fmt.Fprintf(w, "  %s %s: %s: %s\n", r.Kind, r.Name, r.Disposition, r.Reason)
		}
		if issue.Error != "" {
			fmt.Fprintf(w, "  error: %s\n", issue.Error)
		}
	}
	for _, u := range s.Unresolved {
		fmt.Fprintf(w, "unresolved worktree %s: retained: %s\n", u.Path, u.Reason)
	}
	t := s.Totals
	fmt.Fprintf(w, "total: issues=%d failed_issues=%d unresolved=%d eligible=%d retired=%d retained=%d failed=%d\n", len(s.Issues), s.FailedIssues, len(s.Unresolved), t.Eligible, t.Retired, t.Retained, t.Failed)
}
