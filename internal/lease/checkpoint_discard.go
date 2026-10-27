package lease

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/workspace"
)

// DiscardedCheckpoint is the append-only audit of a checkpoint a maintainer
// abandoned. RetentionRef keeps the commit reachable for recovery.
type DiscardedCheckpoint struct {
	Branch       string    `json:"branch"`
	HeadSHA      string    `json:"head_sha"`
	RetentionRef string    `json:"retention_ref"`
	BaseSHA      string    `json:"base_sha"`
	DiscardedBy  string    `json:"discarded_by"`
	DiscardedAt  time.Time `json:"discarded_at"`
}

// DiscardCheckpoint abandons an unsubmitted same-repository checkpoint so the
// next claim starts fresh from the approved base. It needs the approve-task
// authority, refuses a PR, an active claim, or fork state, and is one fenced
// state write that also retains the commit. A conflict is returned, never
// retried, so a discard cannot land on state the maintainer did not inspect.
func (m *Manager) DiscardCheckpoint(ctx context.Context, n int, owner, expectedHead string) (*IssueState, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("discarding a checkpoint is not permitted in fork mode")
	}
	if m.store == nil {
		return nil, ErrStoreUnconfigured
	}
	actor, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	if owner == "" || owner != actor {
		return nil, fmt.Errorf("discard owner must be authenticated actor")
	}
	if err = m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return nil, err
	}
	expectedHead = strings.ToLower(strings.TrimSpace(expectedHead))
	if _, decodeErr := hex.DecodeString(expectedHead); len(expectedHead) != 40 || decodeErr != nil {
		return nil, fmt.Errorf("--expected-head must be the full 40-character checkpoint SHA")
	}
	retaining, ok := m.store.(coordination.RetainingStore)
	if !ok {
		return nil, fmt.Errorf("coordination store cannot retain a discarded checkpoint")
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	switch {
	case st.ForkRepo != "":
		return nil, fmt.Errorf("issue #%d holds a fork contributor's checkpoint from %s; discard applies to same-repository checkpoints only", n, st.ForkRepo)
	case st.PRNumber != 0 || st.PRURL != "" || st.PRRevision != nil || st.PROutcome != nil || st.SubmittedHeadSHA != "":
		return nil, fmt.Errorf("issue #%d has a submitted pull request; its checkpoint cannot be discarded", n)
	case st.Lease.Status == "active" && st.Lease.ExpiresAt.After(m.nowFn()):
		return nil, &AlreadyClaimedError{IssueNumber: n, Owner: st.Lease.Owner, ExpiresAt: st.Lease.ExpiresAt, CommentID: st.ProjectionCommentID}
	case st.HeadSHA == "" || st.Branch == "":
		return nil, fmt.Errorf("issue #%d has no recorded checkpoint to discard", n)
	case st.HeadSHA != expectedHead:
		return nil, fmt.Errorf("issue #%d records checkpoint %s, not the expected %s; check swarm issue status %d", n, st.HeadSHA, expectedHead, n)
	case !strings.HasPrefix(st.Branch, fmt.Sprintf("swarm/issue-%d-", n)):
		return nil, fmt.Errorf("issue #%d records an invalid checkpoint branch %q", n, st.Branch)
	}
	head := st.HeadSHA
	st.DiscardedCheckpoints = append(st.DiscardedCheckpoints, DiscardedCheckpoint{
		Branch:       st.Branch,
		HeadSHA:      head,
		RetentionRef: coordination.CheckpointRef(n, head),
		BaseSHA:      st.BaseSHA,
		DiscardedBy:  actor,
		DiscardedAt:  m.nowFn(),
	})
	fresh := &workspace.FreshStart{Branch: workspace.FreshStartBranch(n, head), AbandonedBranch: st.Branch, AbandonedHeadSHA: head}
	preserveCheckpointHeads(st, head)
	st.FreshStart = fresh
	st.Branch, st.HeadSHA = fresh.Branch, ""
	st.ResumeCheckpointBranch = ""
	st.DroppedCheckpoint = nil
	// Plan, review, and validation evidence described the abandoned attempt;
	// the next attempt plans afresh. Task, base, and policy approval stand.
	st.Artifacts, st.Attestations = nil, nil
	st.ApprovalTransition, st.CarryReason, st.ChangedPolicyPaths = nil, "", nil
	st.PendingPlanReview, st.PlanReplanCount, st.NextPhaseReason = nil, 0, ""
	st.RevisionPlan, st.LastRebase = nil, nil
	st.Lease.NextPhase = ""
	m.stampForSave(ctx, n, st)
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	if _, err = retaining.CompareAndSwapRetaining(ctx, n, rev, raw, head); err != nil {
		if errors.Is(err, coordination.ErrConflict) {
			return nil, fmt.Errorf("issue #%d state changed while discarding its checkpoint; inspect swarm issue status %d and retry: %w", n, n, err)
		}
		return nil, err
	}
	m.project(ctx, n, st)
	return st, nil
}
