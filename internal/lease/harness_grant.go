package lease

import (
	"context"
	"errors"
	"fmt"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workspace"
)

// HarnessGrant names the CI run that asks an approved WorkUnit to authorize
// protected-path changes. Issue and PR are lookup keys only; Base is the CI
// target commit and CandidateHead the pull request head being validated.
type HarnessGrant struct {
	Issue, PR           int
	Base, CandidateHead string
}

var ErrHarnessGrant = errors.New("harness change not authorized by approved WorkUnit")

// VerifyHarnessGrant reports whether authoritative state st still approves wu
// with allow_harness_changes for exactly this pull request head. cfg is the
// configuration loaded at g.Base, so a policy change since approval refuses.
// The approved base may trail g.Base only when the committed .swarm policy
// tree is unchanged between them.
func VerifyHarnessGrant(ctx context.Context, root string, st *IssueState, wu *contract.WorkUnit, cfg *contract.SwarmConfig, g HarnessGrant) error {
	refuse := func(reason string) error { return fmt.Errorf("%w: %s", ErrHarnessGrant, reason) }
	if st == nil || wu == nil {
		return refuse("no authoritative state or WorkUnit")
	}
	if wu.IssueNumber != g.Issue || (st.Lease.IssueNumber != 0 && st.Lease.IssueNumber != g.Issue) {
		return refuse("WorkUnit does not belong to the linked issue")
	}
	if !TaskApprovalCurrent(st, wu, cfg) {
		return refuse("task or policy changed since approval")
	}
	if err := VerifyApproval(st, wu); err != nil {
		return refuse("plan gate not satisfied")
	}
	if !wu.AllowHarnessChanges {
		return refuse("approved WorkUnit does not set allow_harness_changes")
	}
	if st.ForkRepo != "" {
		return refuse("fork contributions cannot change protected paths")
	}
	for _, approved := range []string{st.BaseSHA, st.SubmittedBaseSHA} {
		if approved == "" {
			return refuse("approved or submitted base missing")
		}
		contained, err := workspace.BaseIsAncestor(ctx, root, approved, g.Base)
		if err != nil || !contained {
			return refuse("approved base is not an ancestor of the CI base")
		}
	}
	approvedTree, err := workspace.PolicyTreeDigest(ctx, root, st.BaseSHA)
	if err != nil {
		return refuse("approved policy tree unavailable")
	}
	tree, err := workspace.PolicyTreeDigest(ctx, root, g.Base)
	if err != nil || tree != approvedTree || (st.PolicyTreeDigest != "" && st.PolicyTreeDigest != tree) {
		return refuse("committed policy changed since approval")
	}
	if st.PRNumber == 0 || st.PRNumber != g.PR {
		return refuse("pull request does not match the submitted PR")
	}
	if st.PRRevision != nil {
		return refuse("a PR revision is pending")
	}
	if st.SubmittedHeadSHA == "" || st.SubmittedHeadSHA != g.CandidateHead {
		return refuse("candidate head does not match the submitted PR revision")
	}
	return nil
}
