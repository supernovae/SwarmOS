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

var ErrPlanUnchanged = errors.New("re-plan is unchanged after CHANGES_REQUESTED")

var ErrPlanReplanLimit = errors.New("automatic plan retry limit reached")

// PlanRejection survives replacement of the rejected plan's artifacts.
type PlanRejection struct {
	ReviewID     string `json:"review_id"`
	Version      int    `json:"version"`
	Digest       string `json:"digest"`
	TaskDigest   string `json:"task_digest"`
	PolicyDigest string `json:"policy_digest"`
	BaseSHA      string `json:"base_sha"`
	Source       string `json:"source,omitempty"`
	Findings     string `json:"findings,omitempty"`
	PlanDigest   string `json:"plan_digest,omitempty"`
	TestDigest   string `json:"test_digest,omitempty"`
}

func recordPlanReviewTransition(st *IssueState, art contract.Artifact, advisory bool) {
	appendPlanReviewHistory(st, art, advisory)
	if !advisory && art.Type == contract.ArtifactReview && art.Metadata["review_source"] == "authenticated-human" {
		if directIssue(st) {
			if art.Verdict == "CHANGES_REQUESTED" {
				st.HumanPlanReview = rejectionFor(st, art)
				st.HumanDraftCount = 0
			} else if art.Verdict == "APPROVED" {
				st.HumanPlanReview = nil
				st.HumanDraftCount = 0
			}
		}
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
		st.PendingPlanReview = rejectionFor(st, art)
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
	if directIssue(st) && st.HumanPlanReview != nil && !strings.EqualFold(cfg.PlanMode, "interactive") {
		if !validRejection(st.HumanPlanReview) {
			return ErrApprovalRequired
		}
		if st.HumanDraftCount >= cfg.Lease.PlanReplanAttempts() {
			if CompletePlanPair(st) {
				return fmt.Errorf("%w (%d/%d): fresh authenticated exact-report approval required; run swarm issue review-plan %d --approve", ErrPlanReplanLimit, st.HumanDraftCount, cfg.Lease.PlanReplanAttempts(), st.Lease.IssueNumber)
			}
			return fmt.Errorf("%w (%d/%d): %s", ErrPlanReplanLimit, st.HumanDraftCount, cfg.Lease.PlanReplanAttempts(), m.planRevisionGuidance(st))
		}
		return nil
	}
	if strings.EqualFold(cfg.PlanMode, "interactive") || st.PendingPlanReview == nil {
		return nil
	}
	if directIssue(st) && !validRejection(st.PendingPlanReview) {
		return ErrApprovalRequired
	}
	limit := cfg.Lease.PlanReplanAttempts()
	if st.PlanReplanCount < limit {
		return nil
	}
	if directIssue(st) && !CompletePlanPair(st) {
		return fmt.Errorf("%w (%d/%d): %s", ErrPlanReplanLimit, st.PlanReplanCount, limit, m.planRevisionGuidance(st))
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
	if directIssue(st) && !strings.EqualFold(cfg.PlanMode, "interactive") {
		p := st.PendingPlanReview
		if st.HumanPlanReview != nil {
			p = st.HumanPlanReview
		}
		identity := ""
		if p != nil {
			identity = p.Digest
		}
		st.PlanReservation = &PlanDraftReservation{Generation: st.PlanGeneration, Rejection: identity, ClaimID: st.Lease.ClaimID}
		if p != nil {
			st.PlanReservation.ReviewID = p.ReviewID
			st.PlanReservation.ReviewVersion = p.Version
		}
	}
	if directIssue(st) && st.HumanPlanReview != nil && !strings.EqualFold(cfg.PlanMode, "interactive") {
		st.HumanDraftCount++
	} else if st.PendingPlanReview != nil && !strings.EqualFold(cfg.PlanMode, "interactive") {
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
		if directIssue(st) {
			markPlanPair(st)
		}
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

// PlanPair is coordinator-owned evidence of an atomic report generation. It
// deliberately binds content, not approval policy; carry checks policy separately.
type PlanPair struct {
	Generation  int    `json:"generation"`
	PlanVersion int    `json:"plan_version"`
	TestVersion int    `json:"test_version"`
	PlanDigest  string `json:"plan_digest"`
	TestDigest  string `json:"test_digest"`
}

func directIssue(st *IssueState) bool {
	return st != nil && st.ForkRepo == "" && st.PRNumber == 0 && st.PRRevision == nil
}
func planReports(st *IssueState) (plan, test *contract.Artifact) {
	for i := range st.Artifacts {
		a := &st.Artifacts[i]
		switch a.Type {
		case contract.ArtifactDesign:
			plan = a
		case contract.ArtifactTestPlan:
			test = a
		}
	}
	return
}
func rejectionFor(st *IssueState, art contract.Artifact) *PlanRejection {
	plan, test := planReports(st)
	p := &PlanRejection{ReviewID: art.ID, Version: art.Version, Digest: hash(art.Body), TaskDigest: st.TaskDigest, PolicyDigest: st.PolicyDigest, BaseSHA: st.BaseSHA, Source: art.Metadata["review_source"], Findings: art.Body}
	if plan != nil {
		p.PlanDigest = hash(contract.CanonicalArtifactBody(plan.Body))
	}
	if test != nil {
		p.TestDigest = hash(contract.CanonicalArtifactBody(test.Body))
	}
	return p
}
func validRejection(p *PlanRejection) bool {
	return p != nil && p.ReviewID != "" && p.Version > 0 && p.Digest == hash(p.Findings) && p.PlanDigest != "" && p.TestDigest != "" && (p.Source == "authenticated-agent" || p.Source == "authenticated-human") && reviewBodyMatchesVerdict(p.Findings, "CHANGES_REQUESTED")
}

// CompletePlanPair cannot be supplied through artifact metadata or worker imports.
func CompletePlanPair(st *IssueState) bool {
	if st == nil || st.CompletePlan == nil {
		return false
	}
	p, t := planReports(st)
	c := st.CompletePlan
	return p != nil && t != nil && c.Generation == st.PlanGeneration && c.Generation > 0 && p.Version == c.PlanVersion && t.Version == c.TestVersion && hash(p.Body) == c.PlanDigest && hash(t.Body) == c.TestDigest
}
func IncompletePlanPair(st *IssueState) bool {
	if !directIssue(st) {
		return false
	}
	// Missing reports retain tracked rejection safety; regeneration is admitted separately.
	return (st.PendingPlanReview != nil || st.HumanPlanReview != nil || st.PlanGeneration > 0) && !CompletePlanPair(st)
}
func markPlanPair(st *IssueState) {
	p, t := planReports(st)
	if p == nil || t == nil {
		return
	}
	st.PlanGeneration++
	st.CompletePlan = &PlanPair{Generation: st.PlanGeneration, PlanVersion: p.Version, TestVersion: t.Version, PlanDigest: hash(p.Body), TestDigest: hash(t.Body)}
}

// RecordPlanPair installs both reports and their completion evidence in one fenced
// CAS. Failed validation or CAS leaves the previous generation untouched.
func (m *Manager) RecordPlanPair(ctx context.Context, n int, owner string, plan, test contract.Artifact) (*IssueState, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("plan pair requires direct coordinator")
	}
	st, err := m.mutate(ctx, n, owner, func(st *IssueState) error {
		if !directIssue(st) {
			return fmt.Errorf("plan pair requires direct issue")
		}
		if plan.Type != contract.ArtifactDesign || test.Type != contract.ArtifactTestPlan {
			return fmt.Errorf("expected design and test-plan pair")
		}
		oldPlan, oldTest := planReports(st)
		rejection := st.PendingPlanReview
		if st.HumanPlanReview != nil {
			rejection = st.HumanPlanReview
		}
		if rejection != nil {
			if !validRejection(rejection) || rejection.TaskDigest != st.TaskDigest || !planReservationMatches(st, rejection) {
				return ErrApprovalRequired
			}
			if oldPlan != nil && oldTest != nil && contract.CanonicalArtifactBody(oldPlan.Body) == contract.CanonicalArtifactBody(plan.Body) && contract.CanonicalArtifactBody(oldTest.Body) == contract.CanonicalArtifactBody(test.Body) {
				return ErrPlanUnchanged
			}
			if hash(contract.CanonicalArtifactBody(plan.Body)) == rejection.PlanDigest && hash(contract.CanonicalArtifactBody(test.Body)) == rejection.TestDigest {
				return ErrPlanUnchanged
			}
			if st.HumanPlanReview != nil && st.HumanDraftCount == 0 || st.HumanPlanReview == nil && st.PlanReplanCount == 0 {
				return ErrApprovalRequired
			}
		}
		pair := []contract.Artifact{plan, test}
		for i := range pair {
			a := &pair[i]
			if strings.TrimSpace(a.Body) == "" {
				return fmt.Errorf("empty plan report")
			}
			a.IssueNumber = n
			a.ID = fmt.Sprintf("art-%d-%s", n, a.Type)
			a.Version = 1
			a.CreatedAt = m.nowFn()
			if a.Type == contract.ArtifactDesign && oldPlan != nil {
				a.Version = oldPlan.Version + 1
			}
			if a.Type == contract.ArtifactTestPlan && oldTest != nil {
				a.Version = oldTest.Version + 1
			}
			meta := map[string]string{}
			for k, v := range a.Metadata {
				if !strings.HasPrefix(k, "checkpoint_plan_") {
					meta[k] = v
				}
			}
			a.Metadata = meta
			recordRoleRun(st, a)
			a.Metadata["content_sha256"] = hash(a.Body)
			if err := contract.ValidateArtifact(a); err != nil {
				return err
			}
			RecordRoleAttestation(st, contract.RoleAttestation{Role: "planner", Agent: meta["agent"], Harness: meta["harness"], HarnessVersion: meta["harness_version"], Model: meta["model"], ExecutionMode: meta["execution_mode"], Authentication: meta["authentication"], Sandbox: meta["sandbox"], ArtifactType: string(a.Type), ArtifactDigest: hash(a.Body)})
		}
		kept := make([]contract.Artifact, 0, len(st.Artifacts)+2)
		for _, a := range st.Artifacts {
			if a.Type != contract.ArtifactDesign && a.Type != contract.ArtifactTestPlan && a.Type != contract.ArtifactReview {
				kept = append(kept, a)
			}
		}
		st.Artifacts = append(kept, pair...)
		markPlanPair(st)
		st.PlanReservation = nil
		return nil
	})
	if err == nil && m.tracker != nil && (st.EffectiveGate != contract.GateNone || st.HumanPlanReview != nil) {
		_ = m.tracker.AddLabels(ctx, n, []string{"swarm:plan-review"})
	}
	return st, err
}

// HasRevisedPlan distinguishes completion from a replacement of rejected bytes.
func HasRevisedPlan(st *IssueState) bool {
	if !CompletePlanPair(st) {
		return false
	}
	p := st.PendingPlanReview
	if st.HumanPlanReview != nil {
		p = st.HumanPlanReview
	}
	if p == nil {
		return true
	}
	if !validRejection(p) {
		return false
	}
	plan, test := planReports(st)
	return hash(contract.CanonicalArtifactBody(plan.Body)) != p.PlanDigest || hash(contract.CanonicalArtifactBody(test.Body)) != p.TestDigest
}
func ValidPlanRejection(p *PlanRejection) bool { return validRejection(p) }

type PlanDraftReservation struct {
	ClaimID       string `json:"claim_id"`
	ReviewID      string `json:"review_id"`
	ReviewVersion int    `json:"review_version"`
	Generation    int    `json:"generation"`
	Rejection     string `json:"rejection"`
}

func planReservationMatches(st *IssueState, p *PlanRejection) bool {
	return st.PlanReservation != nil && st.PlanReservation.ClaimID == st.Lease.ClaimID && st.PlanReservation.Generation == st.PlanGeneration && p != nil && st.PlanReservation.Rejection == p.Digest && st.PlanReservation.ReviewID == p.ReviewID && st.PlanReservation.ReviewVersion == p.Version
}

func (m *Manager) planRevisionGuidance(st *IssueState) string {
	prefix := ""
	if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(m.nowFn()) {
		prefix = fmt.Sprintf("the active claim owner must first run swarm issue release %d; ", st.Lease.IssueNumber)
	}
	return fmt.Sprintf("identical WorkUnit cannot recover here; %sa maintainer must run swarm issue edit %d --objective '<maintainer-chosen revised objective>', then separately swarm issue approve %d and swarm issue work %d", prefix, st.Lease.IssueNumber, st.Lease.IssueNumber, st.Lease.IssueNumber)
}
