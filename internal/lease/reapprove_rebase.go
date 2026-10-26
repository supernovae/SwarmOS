package lease

import (
	"context"
	"fmt"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/workspace"
)

// ReapproveRebase only moves an existing approval across an identical policy
// tree. It cannot approve a new task or a changed policy.
func (m *Manager) ReapproveRebase(ctx context.Context, n, number int, expectedBase, target, expectedHead string) (*IssueState, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("routine reapproval requires the upstream coordinator; run swarm issue approve %d", n)
	}
	actor, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	if actor == "" {
		return nil, fmt.Errorf("authenticated actor required")
	}
	if err = m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return nil, err
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if st.BaseSHA != expectedBase || st.PRNumber != number {
		return nil, fmt.Errorf("approval state changed")
	}
	if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(m.nowFn()) {
		return nil, fmt.Errorf("active claim: run swarm issue release %d before reapproval", n)
	}
	pr, err := m.tracker.GetPullRequest(ctx, number)
	if err != nil {
		return nil, err
	}
	if pr.BaseSHA != target || pr.HeadSHA != expectedHead || pr.Merged || pr.State != "open" {
		return nil, fmt.Errorf("PR target or head changed")
	}
	issue, err := m.tracker.GetIssue(ctx, n)
	if err != nil {
		return nil, err
	}
	wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
	if err != nil {
		return nil, err
	}
	if st.TaskDigest != contract.DigestWorkUnit(wu) {
		return nil, fmt.Errorf("WorkUnit changed")
	}
	if m.tracker.BackendName() == "github" {
		if err = workspace.FetchCommit(m.repoRoot, m.UpstreamRemote(), target); err != nil {
			return nil, fmt.Errorf("fetch verified PR target: %w", err)
		}
	}
	ancestor, err := workspace.BaseIsAncestor(ctx, m.repoRoot, expectedBase, target)
	if err != nil {
		return nil, err
	}
	if !ancestor {
		return nil, fmt.Errorf("routine reapproval requires a descendant target base")
	}
	oldTree, err := workspace.PolicyTreeDigest(ctx, m.repoRoot, st.BaseSHA)
	if err != nil {
		return nil, err
	}
	tree, err := workspace.PolicyTreeDigest(ctx, m.repoRoot, target)
	if err != nil {
		return nil, err
	}
	if oldTree == "" || oldTree != tree || st.PolicyTreeDigest != oldTree {
		return nil, fmt.Errorf("policy tree changed; run swarm issue approve %d", n)
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, target)
	if err != nil {
		return nil, err
	}
	digest := contract.DigestApprovedPolicy(target, cfg)
	m.carryApproval(ctx, st, wu, target, digest, tree)
	st.PreviousBaseSHA = st.BaseSHA
	st.BaseSHA = target
	st.PolicyDigest = digest
	st.ApprovedBy = actor
	st.ApprovedAt = m.nowFn()

	latest, err := m.tracker.GetPullRequest(ctx, number)
	if err != nil {
		return nil, err
	}
	latestIssue, err := m.tracker.GetIssue(ctx, n)
	if err != nil {
		return nil, err
	}
	latestUnit, err := contract.ParseWorkUnitFromIssue(n, latestIssue.Title, latestIssue.Body)
	if err != nil {
		return nil, err
	}
	if latest.BaseSHA != target || latest.HeadSHA != expectedHead || latest.Merged || latest.State != "open" || contract.DigestWorkUnit(latestUnit) != st.TaskDigest {
		return nil, fmt.Errorf("task or PR changed during routine reapproval")
	}
	st.RebaseApprovalAudit = fmt.Sprintf("%s re-pinned the unchanged WorkUnit approval for PR #%d from %s to %s with an identical committed policy tree.", actor, number, expectedBase, target)
	if _, err = m.save(ctx, n, rev, st); err != nil {
		return nil, err
	}
	return m.ProjectRebaseApprovalAudit(ctx, n)
}

// ProjectRebaseApprovalAudit retries a pending audit projection without granting
// approval again. The text comes only from the saved coordinator transition.
func (m *Manager) ProjectRebaseApprovalAudit(ctx context.Context, n int) (*IssueState, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("approval audit requires the coordinator")
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if st.RebaseApprovalAudit == "" {
		return st, nil
	}
	if _, err = m.tracker.CreateComment(ctx, n, st.RebaseApprovalAudit); err != nil {
		return st, fmt.Errorf("approval re-pinned; retry rebase to project audit: %w", err)
	}
	st.RebaseApprovalAudit = ""
	if _, err = m.save(ctx, n, rev, st); err != nil {
		return st, err
	}
	return st, nil
}
