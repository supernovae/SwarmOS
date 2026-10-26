package lease

import (
	"context"
	"fmt"
	"strings"

	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

func (m *Manager) cleanupLocal() bool {
	_, upstream := m.tracker.(interface{ UpstreamRepo() string })
	return !upstream
}

func cleanupBranchName(branch string) string {
	if i := strings.IndexByte(branch, ':'); i >= 0 {
		branch = branch[i+1:]
	}
	return strings.TrimPrefix(branch, "refs/heads/")
}

// cleanupPolicy is shared by preview and every apply entry point. Only the
// absence of an upstream provider selects local scope; missing identity cannot.
func (m *Manager) cleanupPolicy(ctx context.Context, st *IssueState, o *PROutcome, reserved bool) string {
	return m.cleanupPolicyFrom(ctx, st, o, reserved, nil)
}

// cleanupPolicyFrom reads open PRs from snap when given; apply never passes one.
func (m *Manager) cleanupPolicyFrom(ctx context.Context, st *IssueState, o *PROutcome, reserved bool, snap *CleanupSnapshot) string {
	switch {
	case st.Lease.Status == "active" || st.PRRevision != nil:
		return "active attempt or pending PR revision"
	case !reserved && st.Cleanup != nil:
		return "cleanup reservation is active; explicit recovery is required"
	case o.Status != "merged":
		return "recorded PR is not merged"
	case o.MergeCommitSHA == "" || o.MergedAt == nil || o.MergedAt.IsZero():
		return "merge evidence is incomplete"
	case o.SubmittedHeadSHA == "" || o.FinalHeadSHA != o.SubmittedHeadSHA:
		return "final PR head differs from recorded submission"
	case st.ForkRepo != "":
		return "fork branch retirement is unsupported"
	case !strings.HasPrefix(st.Branch, fmt.Sprintf("swarm/issue-%d-", st.Lease.IssueNumber)):
		return "branch is outside the issue task namespace"
	case o.TargetBranch == "" || st.Branch == o.TargetBranch || st.Branch == "main" || st.Branch == "master":
		return "base/default branches are retained"
	}
	if !m.cleanupLocal() {
		if err := m.verifyCleanupRemote(ctx, o); err != nil {
			return err.Error()
		}
	}
	var prs []*tracker.PullRequest
	var err error
	if snap != nil {
		prs, err = snap.openPullRequests()
	} else {
		prs, err = m.tracker.ListPullRequests(ctx, tracker.PullRequestFilter{State: "open"})
	}
	if err != nil {
		return err.Error()
	}
	for _, pr := range prs {
		if pr == nil {
			return "open PR identity unavailable"
		}
		if pr.Number == st.PRNumber {
			return "linked PR is open"
		}
		branch := cleanupBranchName(pr.Head)
		candidate := branch == st.Branch || branch == workspace.PRBranchName(st.PRNumber)
		if pr.HeadRepo == "" {
			if !m.cleanupLocal() {
				return "open PR repository identity is incomplete"
			}
			if candidate {
				return "branch is used by an open PR"
			}
		} else if strings.EqualFold(pr.HeadRepo, o.HeadRepo) && candidate {
			return "branch is used by an open PR"
		}
	}
	return ""
}
