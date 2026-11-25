package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/workspace"
	"strings"
	"unicode/utf8"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
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
	return m.PlanReplanLimitContext(context.Background(), st)
}

func (m *Manager) PlanReplanLimitContext(ctx context.Context, st *IssueState) (int, error) {
	cfg, err := harness.LoadConfigAtContext(ctx, m.repoRoot, st.BaseSHA)
	if err != nil {
		return 0, err
	}
	return cfg.Lease.PlanReplanAttempts(), nil
}

func (m *Manager) CheckPlanReplan(st *IssueState) error {
	return m.CheckPlanReplanContext(context.Background(), st)
}

func (m *Manager) CheckPlanReplanContext(ctx context.Context, st *IssueState) error {
	cfg, err := harness.LoadConfigAtContext(ctx, m.repoRoot, st.BaseSHA)
	if err != nil {
		return err
	}
	if directIssue(st) && st.HumanPlanReview != nil && !strings.EqualFold(cfg.PlanMode, "interactive") {
		if !validRejection(st.HumanPlanReview) {
			return ErrPlanApprovalRequired
		}
		if st.HumanDraftCount >= cfg.Lease.PlanReplanAttempts() {
			if CompletePlanPair(st) {
				return fmt.Errorf("%w (%d/%d): fresh authenticated exact-report approval required", ErrPlanReplanLimit, st.HumanDraftCount, cfg.Lease.PlanReplanAttempts())
			}
			return fmt.Errorf("%w (%d/%d): %s", ErrPlanReplanLimit, st.HumanDraftCount, cfg.Lease.PlanReplanAttempts(), m.planRevisionGuidance(st))
		}
		return nil
	}
	if strings.EqualFold(cfg.PlanMode, "interactive") || st.PendingPlanReview == nil {
		return nil
	}
	if directIssue(st) && !validRejection(st.PendingPlanReview) {
		return ErrPlanApprovalRequired
	}
	limit := cfg.Lease.PlanReplanAttempts()
	if st.PlanReplanCount < limit {
		return nil
	}
	if directIssue(st) && !CompletePlanPair(st) {
		return fmt.Errorf("%w (%d/%d): %s", ErrPlanReplanLimit, st.PlanReplanCount, limit, m.planRevisionGuidance(st))
	}
	guidance := "request plan review, or edit and reapprove the WorkUnit"
	if st.PRRevision != nil {
		guidance = fmt.Sprintf("run swarm pr work %d --phase review, or edit and reapprove the WorkUnit", st.PRRevision.Number)
	}
	guidance += "; an independent upstream maintainer can request a revised plan or approve the current reports outside --fork"
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
func (m *Manager) reservePlanReplanContext(ctx context.Context, st *IssueState) error {
	if err := m.CheckPlanReplanContext(ctx, st); err != nil {
		return err
	}
	cfg, err := harness.LoadConfigAtContext(ctx, m.repoRoot, st.BaseSHA)
	if err != nil {
		return err
	}
	// A new planner admission retires any older unfinished identity, including
	// when a human decision changed which allowance applies.
	st.PlanRetryReservation = nil
	st.PlanRetryReceipt = nil
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
		if directIssue(st) {
			reservation, err := newRetryReservation(st)
			if err != nil {
				return err
			}
			st.PlanRetryReservation = reservation
		}
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
		cfg, err := harness.LoadConfigAtContext(ctx, m.repoRoot, st.BaseSHA)
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
			return ErrPlanApprovalRequired
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
	if st == nil || st.PRNumber != 0 || st.PRRevision != nil || art.Type != contract.ArtifactReview {
		return
	}
	source := art.Metadata["review_source"]
	if advisory {
		if _, err := contract.ParsePlanFindings(art.Body); err != nil || source != "authenticated-agent" {
			return
		}
	}
	if source != "authenticated-agent" && source != "authenticated-human" {
		return
	}
	findings := contract.CanonicalArtifactBody(art.Body)
	const marker = "\n[Earlier review findings truncated]"
	_, structuredErr := contract.ParsePlanFindings(findings)
	if structuredErr == nil {
		// Keep only the bounded complete ledger; narrative must not evict stable IDs.
		start := strings.Index(findings, contract.PlanFindingsStart)
		end := strings.Index(findings, contract.PlanFindingsEnd) + len(contract.PlanFindingsEnd)
		findings = "VERDICT: " + art.Verdict + "\n" + findings[start:end]
	}
	if structuredErr != nil && len(findings) > 16<<10 {
		findings = findings[:(16<<10)-len(marker)]
		for !utf8.ValidString(findings) {
			findings = findings[:len(findings)-1]
		}
		findings += marker
	}
	st.PlanReviewSequence++
	entry := workspace.PlanReviewEvidence{Source: source, Round: st.PlanReviewSequence, ReviewID: art.ID, Version: art.Version, Verdict: art.Verdict, Findings: findings, Digest: hash(art.Body), TaskDigest: st.TaskDigest, PolicyDigest: st.PolicyDigest, BaseSHA: st.BaseSHA, PlanDigest: art.Metadata["plan_digest"], TestPlanDigest: art.Metadata["test_plan_digest"]}
	// Allocate independently of previously loaded state and bounded slice storage.
	history := append(append([]workspace.PlanReviewEvidence(nil), st.PlanReviewHistory...), entry)
	if len(history) > 8 {
		history = history[len(history)-8:]
	}
	st.PlanReviewHistory = history
}

// ReviewHistory projects only evidence from this approved revision.
func ReviewHistory(st *IssueState) []workspace.PlanReviewEvidence {
	if st == nil || st.PRNumber != 0 || st.PRRevision != nil {
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
		return m.applyPlanPair(st, n, plan, test)
	})
	if err == nil {
		m.projectPlanPair(ctx, n, st)
	}
	return st, err
}

func (m *Manager) preparePlanPair(st *IssueState, n int, plan, test contract.Artifact) ([]contract.Artifact, error) {
	if !directIssue(st) {
		return nil, fmt.Errorf("plan pair requires direct issue")
	}
	if plan.Type != contract.ArtifactDesign || test.Type != contract.ArtifactTestPlan {
		return nil, fmt.Errorf("expected design and test-plan pair")
	}
	oldPlan, oldTest := planReports(st)
	rejection := st.PendingPlanReview
	if st.HumanPlanReview != nil {
		rejection = st.HumanPlanReview
	}
	if rejection != nil {
		if !validRejection(rejection) || rejection.TaskDigest != st.TaskDigest || !planReservationMatches(st, rejection) {
			return nil, ErrPlanApprovalRequired
		}
		if oldPlan != nil && oldTest != nil && contract.CanonicalArtifactBody(oldPlan.Body) == contract.CanonicalArtifactBody(plan.Body) && contract.CanonicalArtifactBody(oldTest.Body) == contract.CanonicalArtifactBody(test.Body) {
			return nil, ErrPlanUnchanged
		}
		if hash(contract.CanonicalArtifactBody(plan.Body)) == rejection.PlanDigest && hash(contract.CanonicalArtifactBody(test.Body)) == rejection.TestDigest {
			return nil, ErrPlanUnchanged
		}
		if st.HumanPlanReview != nil && st.HumanDraftCount == 0 || st.HumanPlanReview == nil && st.PlanReplanCount == 0 {
			return nil, ErrPlanApprovalRequired
		}
	}
	pair := []contract.Artifact{plan, test}
	for i := range pair {
		a := &pair[i]
		if strings.TrimSpace(a.Body) == "" {
			return nil, fmt.Errorf("empty plan report")
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
		a.Metadata["content_sha256"] = hash(a.Body)
		if err := contract.ValidateArtifact(a); err != nil {
			return nil, err
		}
	}
	return pair, nil
}

func (m *Manager) ValidatePlanRetryPair(st *IssueState, n int, plan, test contract.Artifact) error {
	_, err := m.preparePlanPair(st, n, plan, test)
	return err
}

func (m *Manager) applyPlanPair(st *IssueState, n int, plan, test contract.Artifact) error {
	pair, err := m.preparePlanPair(st, n, plan, test)
	if err != nil {
		return err
	}
	for i := range pair {
		a := &pair[i]
		recordRoleRun(st, a)
		meta := a.Metadata
		RecordRoleAttestation(st, contract.RoleAttestation{Role: "planner", Agent: meta["agent"], Harness: meta["harness"], HarnessVersion: meta["harness_version"], Model: meta["model"], ExecutionMode: meta["execution_mode"], Authentication: meta["authentication"], Sandbox: meta["sandbox"], ArtifactType: string(a.Type), ArtifactDigest: hash(a.Body)})
	}
	kept := make([]contract.Artifact, 0, len(st.Artifacts)+2)
	for _, a := range st.Artifacts {
		if a.Type != contract.ArtifactDesign && a.Type != contract.ArtifactTestPlan && a.Type != contract.ArtifactReview {
			kept = append(kept, a)
		}
	}
	delete(st.AttemptDiagnostics, "planner")
	st.Artifacts = append(kept, pair...)
	markPlanPair(st)
	st.PlanRetryReservation = nil
	st.PlanReservation = nil
	return nil
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
		prefix = "the active claim owner must first release the claim; "
	}
	return fmt.Sprintf("identical WorkUnit cannot recover here; %sa maintainer must revise the objective, then separately run swarm issue approve %d and swarm issue work %d", prefix, st.Lease.IssueNumber, st.Lease.IssueNumber)
}

// RetryReservation is coordinator-owned authority for one unfinished automatic
// attempt. Older states without an identity cannot authorize a refund.
type RetryReservation struct {
	ID      string `json:"id"`
	ClaimID string `json:"claim_id"`
	Binding string `json:"binding"`
}

func retryBinding(st *IssueState) string {
	body, _ := json.Marshal(struct {
		Task, Policy, Base, Head, Branch string
		Generation                       int
		Rejection, Human                 *PlanRejection
		Artifacts                        []contract.Artifact
	}{st.TaskDigest, st.PolicyDigest, st.BaseSHA, st.HeadSHA, st.Branch, st.PlanGeneration, st.PendingPlanReview, st.HumanPlanReview, st.Artifacts})
	return hash(string(body))
}

func newRetryReservation(st *IssueState) (*RetryReservation, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nil, err
	}
	return &RetryReservation{ID: hex.EncodeToString(entropy[:]), ClaimID: st.Lease.ClaimID, Binding: retryBinding(st)}, nil
}

// RefundRetryReservation returns exactly one still-uncompleted attempt. The
// caller supplies only its opaque identity, never a counter or decrement.
func (m *Manager) RefundRetryReservation(ctx context.Context, n int, owner, id string) (*IssueState, error) {
	return m.finishRetryReservation(ctx, n, owner, id, true)
}

// CompleteRetryReservation seals consumption before output is processed, even
// when that output will subsequently be rejected. Plan draft admission survives.
func (m *Manager) CompleteRetryReservation(ctx context.Context, n int, owner, id string) (*IssueState, error) {
	return m.finishRetryReservation(ctx, n, owner, id, false)
}

func (m *Manager) finishRetryReservation(ctx context.Context, n int, owner, id string, refund bool) (*IssueState, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("retry completion requires direct coordinator")
	}
	return m.mutate(ctx, n, owner, func(st *IssueState) error {
		if !directIssue(st) || id == "" {
			return ErrClaimFence
		}
		reservation := st.PlanRetryReservation
		plan := reservation != nil && reservation.ID == id
		if !plan {
			reservation = st.RepairRetryReservation
		}
		if reservation == nil || reservation.ID != id || reservation.ClaimID != st.Lease.ClaimID || reservation.Binding != retryBinding(st) {
			return ErrClaimFence
		}
		if plan {
			if st.PlanReplanCount <= 0 || st.HumanPlanReview != nil || !planReservationMatches(st, st.PendingPlanReview) {
				return ErrClaimFence
			}
			if refund {
				st.PlanReplanCount--
				st.PlanReservation = nil
			}
			st.PlanRetryReservation = nil
		} else {
			if st.ValidationRepairCount <= 0 || st.ValidationRepairKey != validationRepairKey(st) {
				return ErrClaimFence
			}
			if refund {
				st.ValidationRepairCount--
			}
			st.RepairRetryReservation = nil
		}
		return nil
	})
}

// PlanRetryOutcome is the coordinator-prepared result of one planner retry.
// Failure is a fixed diagnostic code. FileDigests bind the retained reports.
type PlanRetryOutcome struct {
	Reservation RetryReservation
	Owner       string
	Plan, Test  contract.Artifact
	FileDigests [2]string
	Failure     string
	Telemetry   string
	Refund      bool
}

// PlanRetryReceipt authenticates an exact outcome after admission is consumed.
type PlanRetryReceipt struct {
	ID      string `json:"id"`
	ClaimID string `json:"claim_id"`
	Owner   string `json:"owner"`
	Outcome string `json:"outcome"`
	Binding string `json:"binding"`
}

func (o PlanRetryOutcome) Digest() (string, error) {
	payload, err := json.Marshal(o)
	return hash(string(payload)), err
}

func planRetryReceiptBinding(st *IssueState) string {
	copy := *st
	copy.Artifacts = nil
	for _, a := range st.Artifacts {
		switch a.Type {
		case contract.ArtifactDesign, contract.ArtifactTestPlan, contract.ArtifactReview:
			copy.Artifacts = append(copy.Artifacts, a)
		}
	}
	return retryBinding(&copy)
}

// MatchPlanRetryOutcome classifies a state already checked under the claim
// fence. The pending and settled forms bind different plan artifact versions.
func MatchPlanRetryOutcome(st *IssueState, owner string, o PlanRetryOutcome) (settled bool, err error) {
	r := o.Reservation
	if !directIssue(st) || r.ID == "" || r.ClaimID != st.Lease.ClaimID || owner != st.Lease.Owner || o.Owner != owner {
		return false, ErrClaimFence
	}
	if receipt := st.PlanRetryReceipt; receipt != nil && receipt.ID == r.ID {
		digest, err := o.Digest()
		if err != nil {
			return false, err
		}
		if *receipt != (PlanRetryReceipt{r.ID, r.ClaimID, owner, digest, planRetryReceiptBinding(st)}) {
			return false, ErrClaimFence
		}
		return true, nil
	}
	if st.PlanRetryReservation == nil || *st.PlanRetryReservation != r || r.Binding != retryBinding(st) ||
		st.PlanReplanCount <= 0 || st.HumanPlanReview != nil || !planReservationMatches(st, st.PendingPlanReview) {
		return false, ErrClaimFence
	}
	return false, nil
}

// InspectPlanRetry returns a fenced authoritative snapshot and its CAS revision.
func (m *Manager) InspectPlanRetry(ctx context.Context, n int, owner string) (*IssueState, string, error) {
	if m.gateway != nil {
		return nil, "", ErrClaimFence
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, "", err
	}
	if err := m.checkStateClaim(ctx, n, owner, st); err != nil {
		var approval *TaskApprovalError
		if errors.As(err, &approval) {
			err = errors.Join(ErrClaimFence, err)
		}
		return nil, "", err
	}
	return st, rev, nil
}

// SettlePlanRetry commits the result and receipt in one fenced CAS. An explicit
// predecessor pins replay to the revision classified by the caller.
func (m *Manager) SettlePlanRetry(ctx context.Context, n int, owner, predecessor string, o PlanRetryOutcome) (*IssueState, error) {
	st, rev, err := m.InspectPlanRetry(ctx, n, owner)
	if err != nil {
		return nil, err
	}
	if predecessor != "" && predecessor != rev {
		return nil, coordination.ErrConflict
	}
	settled, err := MatchPlanRetryOutcome(st, owner, o)
	if err != nil || settled {
		return st, err
	}
	digest, err := o.Digest()
	if err != nil {
		return nil, err
	}
	if o.Failure == "" {
		if o.Refund {
			return nil, fmt.Errorf("cannot refund a completed planner retry")
		}
		if err := m.applyPlanPair(st, n, o.Plan, o.Test); err != nil {
			return nil, err
		}
	} else {
		switch o.Failure {
		case "failed", "invalid-output", "unchanged", "timeout":
		default:
			return nil, fmt.Errorf("invalid planner retry diagnostic")
		}
		if o.Refund && o.Failure != "failed" && o.Failure != "timeout" {
			return nil, fmt.Errorf("invalid planner retry refund")
		}
		if o.Telemetry != "" {
			run, err := contract.ParseRoleRunTelemetry(o.Telemetry)
			if err != nil || run.Role != "planner" || run.FailureClass == "" {
				return nil, fmt.Errorf("invalid failed planner telemetry")
			}
			appendRoleRun(st, run)
		}
		if st.AttemptDiagnostics == nil {
			st.AttemptDiagnostics = map[string]AttemptDiagnostic{}
		}
		kind := "planner-retry-" + o.Failure
		if o.Failure == "timeout" {
			kind = "timeout"
		}
		st.AttemptDiagnostics["planner"] = AttemptDiagnostic{TaskDigest: st.TaskDigest, ClaimID: st.Lease.ClaimID, Kind: kind}
		if o.Refund {
			st.PlanReplanCount--
		}
		st.PlanReservation = nil
		st.PlanRetryReservation = nil
	}
	st.PlanRetryReceipt = &PlanRetryReceipt{o.Reservation.ID, o.Reservation.ClaimID, owner, digest, planRetryReceiptBinding(st)}
	if err := m.checkClaimFence(n, owner, st); err != nil {
		return nil, err
	}
	st, err = m.save(ctx, n, rev, st)
	if err == nil && o.Failure == "" {
		m.projectPlanPair(ctx, n, st)
	}
	return st, err
}

// ProjectSettledPlanRetry refreshes advisory tracker output after the
// authoritative receipt resolves an ambiguous write.
func (m *Manager) ProjectSettledPlanRetry(ctx context.Context, n int, st *IssueState, planRecorded bool) {
	m.projectWithIssue(ctx, n, st, nil)
	if planRecorded {
		m.projectPlanPair(ctx, n, st)
	}
}

func (m *Manager) projectPlanPair(ctx context.Context, n int, st *IssueState) {
	if m.tracker != nil && (st.EffectiveGate != contract.GateNone || st.HumanPlanReview != nil) {
		_ = m.tracker.AddLabels(ctx, n, []string{"swarm:plan-review"})
	}
}
