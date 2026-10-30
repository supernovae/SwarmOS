package lease

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/workspace"
	"strings"
	"unicode/utf8"

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
	appendPlanReviewHistory(st, art, advisory)
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
	if st.PRRevision != nil {
		guidance = fmt.Sprintf("run swarm pr work %d --phase review, or edit and reapprove the WorkUnit", st.PRRevision.Number)
	}
	guidance += fmt.Sprintf("; an authorized independent upstream maintainer, outside --fork, can run swarm issue review-plan %d --reject to request a revised plan (this does not approve implementation), or swarm issue review-plan %d --approve after reviewing the current reports", st.Lease.IssueNumber, st.Lease.IssueNumber)
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

func dropPlanReviewHistory(st *IssueState) {
	st.PlanReviewHistory = nil
	st.PlanReviewSequence = 0
}

// DirectPlanReview excludes the independent PR and fork review protocols.
func DirectPlanReview(st *IssueState) bool {
	return st != nil && st.PRNumber == 0 && st.PRRevision == nil && st.ForkRepo == ""
}

func appendPlanReviewHistory(st *IssueState, art contract.Artifact, advisory bool) {
	if !DirectPlanReview(st) || advisory || art.Type != contract.ArtifactReview {
		return
	}
	source := art.Metadata["review_source"]
	if source != "authenticated-agent" && source != "authenticated-human" {
		return
	}
	findings := contract.CanonicalArtifactBody(art.Body)
	const marker = "\n[Earlier review findings truncated]"
	if len(findings) > 16<<10 {
		findings = findings[:(16<<10)-len(marker)]
		for !utf8.ValidString(findings) {
			findings = findings[:len(findings)-1]
		}
		findings += marker
	}
	st.PlanReviewSequence++
	entry := workspace.PlanReviewEvidence{Round: st.PlanReviewSequence, ReviewID: art.ID, Version: art.Version, Verdict: art.Verdict, Findings: findings, Digest: hash(art.Body), TaskDigest: st.TaskDigest, PolicyDigest: st.PolicyDigest, BaseSHA: st.BaseSHA, PlanDigest: art.Metadata["plan_digest"], TestPlanDigest: art.Metadata["test_plan_digest"]}
	// Allocate independently of previously loaded state and bounded slice storage.
	history := append(append([]workspace.PlanReviewEvidence(nil), st.PlanReviewHistory...), entry)
	if len(history) > 8 {
		history = history[len(history)-8:]
	}
	st.PlanReviewHistory = history
}

// ReviewHistory projects only evidence from this approved revision.
func ReviewHistory(st *IssueState) []workspace.PlanReviewEvidence {
	if !DirectPlanReview(st) {
		return nil
	}
	var result []workspace.PlanReviewEvidence
	for _, h := range st.PlanReviewHistory {
		if h.TaskDigest == st.TaskDigest && h.PolicyDigest == st.PolicyDigest && h.BaseSHA == st.BaseSHA {
			result = append(result, h)
		}
	}
	return result
}

// PlanReviewRound is independent of retry reservations and human budget resets.
func PlanReviewRound(st *IssueState) int {
	history := ReviewHistory(st)
	if len(history) == 0 {
		return 1
	}
	last := history[len(history)-1]
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactReview && a.ID == last.ReviewID && a.Version == last.Version && hash(a.Body) == last.Digest && a.Metadata["plan_digest"] == last.PlanDigest && a.Metadata["test_plan_digest"] == last.TestPlanDigest {
			return last.Round
		}
	}
	return st.PlanReviewSequence + 1
}

// CurrentPlanRejection requires the current authenticated rejection, not merely
// a spent retry reservation surviving report replacement or a reviewer crash.
func CurrentPlanRejection(st *IssueState) bool {
	p := st.PendingPlanReview
	if p == nil || p.TaskDigest != st.TaskDigest || p.PolicyDigest != st.PolicyDigest || p.BaseSHA != st.BaseSHA {
		return false
	}
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactReview && a.Verdict == "CHANGES_REQUESTED" && a.Metadata["review_source"] == "authenticated-agent" && a.ID == p.ReviewID && a.Version == p.Version && hash(a.Body) == p.Digest {
			for k, v := range ReviewBindings(st) {
				if a.Metadata[k] != v {
					return false
				}
			}
			return true
		}
	}
	return false
}
