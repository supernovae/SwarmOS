package lease

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

var ErrPlanReplanLimit = errors.New("automatic plan retry limit reached")

// PlanRejection survives replacement of the rejected plan's artifacts.
type PlanRejection struct {
	ReviewID     string `json:"review_id"`
	Version      int    `json:"version"`
	Digest       string `json:"digest"`
	TaskDigest   string `json:"task_digest"`
	PolicyDigest string `json:"policy_digest"`
	BaseSHA      string `json:"base_sha"`
}

func recordPlanReviewTransition(st *IssueState, art contract.Artifact, advisory bool) {
	if !advisory && art.Type == contract.ArtifactReview && art.Metadata["review_source"] == "authenticated-human" {
		st.PlanReplanCount = 0
		st.PendingPlanReview = nil
		st.NextPhaseReason = ""
		st.Lease.NextPhase = ""
		return
	}
	if advisory || art.Type != contract.ArtifactReview || art.Metadata["review_source"] != "authenticated-agent" {
		return
	}
	if art.Verdict == "CHANGES_REQUESTED" {
		st.PendingPlanReview = &PlanRejection{ReviewID: art.ID, Version: art.Version, Digest: hash(art.Body), TaskDigest: st.TaskDigest, PolicyDigest: st.PolicyDigest, BaseSHA: st.BaseSHA}
		st.Lease.NextPhase = "plan"
		st.NextPhaseReason = fmt.Sprintf("review %s v%d sha256:%s returned CHANGES_REQUESTED", art.ID, art.Version, hash(art.Body))
	} else if art.Verdict == "APPROVED" {
		st.PendingPlanReview = nil
		st.Lease.NextPhase = ""
		st.NextPhaseReason = ""
	}
}

// PlanReplanLimit always resolves policy from the approved base.
func (m *Manager) PlanReplanLimit(st *IssueState) (int, error) {
	cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
	if err != nil {
		return 0, err
	}
	return cfg.Lease.PlanReplanAttempts(), nil
}

func (m *Manager) CheckPlanReplan(st *IssueState) error {
	cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
	if err != nil {
		return err
	}
	if strings.EqualFold(cfg.PlanMode, "interactive") || st.PendingPlanReview == nil {
		return nil
	}
	limit := cfg.Lease.PlanReplanAttempts()
	if st.PlanReplanCount < limit {
		return nil
	}
	guidance := fmt.Sprintf("run swarm issue review-plan %d, or edit and reapprove the WorkUnit", st.Lease.IssueNumber)
	if !strings.EqualFold(cfg.PlanMode, "agent") {
		resource, number := "issue", st.Lease.IssueNumber
		if st.PRRevision != nil {
			resource, number = "pr", st.PRRevision.Number
		}
		guidance += fmt.Sprintf("; manually revise both reports and run swarm %s stage %d --phase plan in its managed worktree", resource, number)
	}
	if st.ForkRepo != "" {
		guidance += "; fork staging preserves the count; only maintainer human review or a changed approved revision resets it"
	}
	return fmt.Errorf("%w (%d/%d): %s", ErrPlanReplanLimit, st.PlanReplanCount, limit, guidance)
}

// reservePlanReplan runs inside the existing update-phase fenced CAS. Fork
// requests use that same operation; callers cannot choose a count or a limit.
func (m *Manager) reservePlanReplan(st *IssueState) error {
	if err := m.CheckPlanReplan(st); err != nil {
		return err
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
	if err != nil {
		return err
	}
	if st.PendingPlanReview != nil && !strings.EqualFold(cfg.PlanMode, "interactive") {
		st.PlanReplanCount++
	}
	return nil
}

// ResetPlanReplansAfterStage is coordinator-only and called while holding the
// staging claim lock, after both reports have been persisted successfully.
func (m *Manager) ResetPlanReplansAfterStage(ctx context.Context, issue int, owner string, designVersion, testVersion int) (*IssueState, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("fork staging cannot reset plan retries")
	}
	return m.mutate(ctx, issue, owner, func(st *IssueState) error {
		if st.ForkRepo != "" {
			return fmt.Errorf("fork staging cannot reset plan retries")
		}
		cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
		if err != nil {
			return err
		}
		if strings.EqualFold(cfg.PlanMode, "agent") {
			return fmt.Errorf("manual plan staging is disabled")
		}
		designOK, testOK := false, false
		for _, a := range st.Artifacts {
			if a.Type == contract.ArtifactDesign {
				designOK = a.Version == designVersion && designVersion > 0
			}
			if a.Type == contract.ArtifactTestPlan {
				testOK = a.Version == testVersion && testVersion > 0
			}
		}
		if !designOK || !testOK {
			return fmt.Errorf("staged plan versions changed")
		}
		if p := st.PendingPlanReview; p != nil && (p.TaskDigest != st.TaskDigest || p.PolicyDigest != st.PolicyDigest || p.BaseSHA != st.BaseSHA) {
			return ErrApprovalRequired
		}
		st.PlanReplanCount = 0
		st.Lease.Phase = "plan"
		return nil
	})
}
