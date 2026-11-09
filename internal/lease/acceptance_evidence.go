package lease

import (
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/validation"
	"slices"
	"strings"
	"time"
)

// PreserveAcceptanceEvidence considers only the latest validation. Missing or
// malformed newer tester evidence must never resurrect an older met mapping.
func PreserveAcceptanceEvidence(st *IssueState, dst *contract.Artifact) {
	if st == nil || dst == nil {
		return
	}
	if _, exists := dst.Metadata[validation.AcceptanceEvidenceKey]; exists {
		return
	}
	for i := len(st.Artifacts) - 1; i >= 0; i-- {
		src := st.Artifacts[i]
		if src.Type != contract.ArtifactValidation {
			continue
		}
		if src.Metadata["validation_trusted_origin"] != "coordinator" || src.Metadata["validation_trusted_owner"] != st.Lease.Owner || src.Metadata["validation_trusted_claim"] != st.Lease.ClaimID || st.Lease.ClaimID == "" || st.Lease.Owner == "" {
			return
		}
		for _, key := range []string{"head_sha", "base_sha", "validation_claim", "validation_base", "validation_tree", "validation_policy"} {
			if src.Metadata[key] == "" || src.Metadata[key] != dst.Metadata[key] {
				return
			}
		}
		if src.Metadata["validation_claim"] != st.Lease.ClaimID || src.Metadata["base_sha"] != st.BaseSHA || src.Metadata["validation_base"] != st.BaseSHA {
			return
		}
		if dst.Metadata == nil {
			dst.Metadata = map[string]string{}
		}
		validation.StoreAcceptanceSnapshot(dst.Metadata, validation.AcceptanceSnapshotFromMetadata(src.Metadata))
		dst.Body = validation.RestoreAcceptanceEvidence(dst.Body, dst.Metadata)
		return
	}
}

// RecordedAcceptanceWarnings uses the latest eligible report for completed retries.
func RecordedAcceptanceWarnings(st *IssueState, criteria []string) []validation.AcceptanceWarning {
	dst := contract.Artifact{Metadata: map[string]string{}}
	if st != nil {
		for i := len(st.Artifacts) - 1; i >= 0; i-- {
			src := st.Artifacts[i]
			if src.Type != contract.ArtifactValidation {
				continue
			}
			for _, k := range []string{"head_sha", "base_sha", "validation_claim", "validation_base", "validation_tree", "validation_policy"} {
				dst.Metadata[k] = src.Metadata[k]
			}
			dst.Metadata["head_sha"] = st.HeadSHA
			PreserveAcceptanceEvidence(st, &dst)
			break
		}
	}
	return validation.AcceptanceEvidenceWarnings(criteria, validation.AcceptanceSnapshotFromMetadata(dst.Metadata))
}

// RecordedReadiness is a read-only projection, not fresh validation authority.
func RecordedReadiness(st *IssueState, criteria []string) validation.Readiness {
	dst := contract.Artifact{Metadata: map[string]string{}}
	checks := "unavailable"
	if st != nil {
		for i := len(st.Artifacts) - 1; i >= 0; i-- {
			src := st.Artifacts[i]
			if src.Type != contract.ArtifactValidation {
				continue
			}
			for _, k := range []string{"head_sha", "base_sha", "validation_claim", "validation_base", "validation_tree", "validation_policy"} {
				dst.Metadata[k] = src.Metadata[k]
			}
			dst.Metadata["head_sha"] = st.HeadSHA
			PreserveAcceptanceEvidence(st, &dst)
			// Preservation eligibility also applies when a report contains no mapping.
			if _, ok := dst.Metadata[validation.AcceptanceEvidenceKey]; ok {
				if src.Verdict == "PASS" {
					checks = "passed"
				} else {
					checks = "failed"
				}
			}
			break
		}
	}
	return validation.NewReadiness(checks, criteria, validation.AcceptanceSnapshotFromMetadata(dst.Metadata))
}

// GapAcknowledgment is a coordinator-only decision, never imported from artifacts
// or public IssueState JSON.
type GapAcknowledgment struct {
	Actor          string    `json:"actor"`
	At             time.Time `json:"at"`
	Indices        []int     `json:"indices"`
	PreviousHead   string    `json:"previous_head"`
	Head           string    `json:"head"`
	SnapshotDigest string    `json:"snapshot_digest"`
	TaskDigest     string    `json:"task_digest"`
	ClaimID        string    `json:"claim_id"`
}

func (m *Manager) AuthorizeGapAcknowledgment(ctx context.Context, owner string) error {
	if m.gateway != nil {
		return fmt.Errorf("gap acknowledgment is unavailable under a worker gateway")
	}
	actor, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(actor) == "" || actor != owner {
		return fmt.Errorf("gap acknowledgment requires the authenticated claim owner")
	}
	return m.tracker.Authorize(ctx, "approve-task")
}

func recordedGapEvidence(st *IssueState, unit *contract.WorkUnit, head string) ([]validation.AcceptanceGap, string, error) {
	if st == nil || head == "" || st.ForkRepo != "" || st.PRRevision != nil || st.TaskDigest != contract.DigestWorkUnit(unit) {
		return nil, "", fmt.Errorf("gap acknowledgment identity differs")
	}
	candidate := *st
	candidate.HeadSHA = head
	r := RecordedReadiness(&candidate, unit.Acceptance)
	if r.RequiredChecks != "passed" {
		return nil, "", fmt.Errorf("trusted required checks are %s", r.RequiredChecks)
	}
	dst := contract.Artifact{Metadata: map[string]string{}}
	for i := len(st.Artifacts) - 1; i >= 0; i-- {
		a := st.Artifacts[i]
		if a.Type != contract.ArtifactValidation {
			continue
		}
		for k, v := range a.Metadata {
			dst.Metadata[k] = v
		}
		delete(dst.Metadata, validation.AcceptanceEvidenceKey)
		dst.Metadata["head_sha"] = head
		PreserveAcceptanceEvidence(st, &dst)
		break
	}
	return validation.AcceptanceGaps(unit.Acceptance, validation.AcceptanceSnapshotFromMetadata(dst.Metadata))
}

func applicableGaps(st *IssueState, unit *contract.WorkUnit, head string) []validation.AcceptanceGap {
	gaps, digest, err := recordedGapEvidence(st, unit, head)
	if err != nil || st.gapAcknowledgment == nil {
		return nil
	}
	a := st.gapAcknowledgment
	if a.Actor != st.Lease.Owner || a.Actor == "" || a.ClaimID == "" || a.ClaimID != st.Lease.ClaimID || a.Head != head || a.TaskDigest != st.TaskDigest || a.SnapshotDigest != digest || len(a.Indices) != len(gaps) {
		return nil
	}
	for i, g := range gaps {
		if a.Indices[i] != g.Index {
			return nil
		}
	}
	return gaps
}

// KnownGaps projects only a decision applicable to the current authoritative head.
func KnownGaps(st *IssueState, unit *contract.WorkUnit) []validation.AcceptanceGap {
	if st == nil {
		return nil
	}
	return applicableGaps(st, unit, st.HeadSHA)
}

// RecordGapAcknowledgment derives all authority and evidence inside the fenced write.
func (m *Manager) RecordGapAcknowledgment(ctx context.Context, n int, owner, head string, indices []int) (*IssueState, error) {
	if err := m.AuthorizeGapAcknowledgment(ctx, owner); err != nil {
		return nil, err
	}
	requested := slices.Clone(indices)
	slices.Sort(requested)
	before, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if before.Lease.Status != "completed" {
		if err := m.checkStateClaim(ctx, n, owner, before); err != nil {
			return nil, err
		}
	}
	issue, err := m.tracker.GetIssue(ctx, n)
	if err != nil {
		return nil, err
	}
	unit, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
	if err != nil {
		return nil, err
	}
	_, expectedDigest, err := recordedGapEvidence(before, unit, head)
	if err != nil {
		return nil, err
	}
	record := func(st *IssueState) error {
		if st.TaskDigest != before.TaskDigest || st.HeadSHA != before.HeadSHA || st.Lease.ClaimID != before.Lease.ClaimID {
			return fmt.Errorf("acknowledgment candidate identity changed: %w", ErrClaimFence)
		}

		if err := m.AuthorizeGapAcknowledgment(ctx, owner); err != nil {
			return err
		}
		issue, err := m.tracker.GetIssue(ctx, n)
		if err != nil {
			return err
		}
		unit, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
		if err != nil {
			return err
		}
		gaps, digest, err := recordedGapEvidence(st, unit, head)
		if err != nil {
			return err
		}
		if digest != expectedDigest {
			return fmt.Errorf("acceptance evidence changed during acknowledgment")
		}
		if len(requested) != len(gaps) {
			return fmt.Errorf("--acknowledge-gap must name every unmet index exactly once")
		}
		for i, g := range gaps {
			if requested[i] != g.Index {
				return fmt.Errorf("--acknowledge-gap must name every unmet index exactly once")
			}
		}
		st.gapAcknowledgment = &GapAcknowledgment{PreviousHead: st.HeadSHA, Actor: owner, At: m.nowFn(), Indices: requested, Head: head, SnapshotDigest: digest, TaskDigest: st.TaskDigest, ClaimID: st.Lease.ClaimID}
		return nil
	}
	if before.Lease.Status == "completed" {
		if before.Lease.Owner != owner || before.Lease.ClaimID != m.token(n) || before.PRNumber == 0 || before.HeadSHA != head {
			return nil, ErrClaimFence
		}
		if err := m.checkApprovedState(ctx, n, before); err != nil {
			return nil, err
		}
		if err := record(before); err != nil {
			return nil, err
		}
		return m.save(ctx, n, rev, before)
	}
	return m.mutate(ctx, n, owner, record)
}

type gapSubmissionKey struct{}

// WithGapSubmission enables authoritative acceptance rechecks at publication.
func WithGapSubmission(ctx context.Context, head string) context.Context {
	return context.WithValue(ctx, gapSubmissionKey{}, head)
}
func (m *Manager) checkGapPublication(ctx context.Context, st *IssueState, checkpoint string) error {
	head, enabled := ctx.Value(gapSubmissionKey{}).(string)
	if !enabled {
		return nil
	}
	if checkpoint != "" && checkpoint != head {
		return fmt.Errorf("gap submission candidate changed")
	}
	issue, err := m.tracker.GetIssue(ctx, st.Lease.IssueNumber)
	if err != nil {
		return err
	}
	unit, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		return err
	}
	candidate := *st
	candidate.HeadSHA = head
	r := RecordedReadiness(&candidate, unit.Acceptance)
	if st.TaskDigest != contract.DigestWorkUnit(unit) || r.RequiredChecks != "passed" {
		return fmt.Errorf("submission validation identity changed")
	}
	if a := st.gapAcknowledgment; a != nil && st.HeadSHA != head && st.HeadSHA != a.PreviousHead {
		return fmt.Errorf("gap submission checkpoint identity changed")
	}
	if r.AcceptanceEvidence.State == "complete" || len(applicableGaps(st, unit, head)) > 0 {
		return nil
	}
	return r.SubmissionError(issue.Number, false)
}

// KnownGapsText retains evidence without changing the tester's acceptance status.
func KnownGapsText(gaps []validation.AcceptanceGap) string {
	if len(gaps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Known gaps\n")
	for _, g := range gaps {
		evidence := g.Evidence
		if strings.TrimSpace(evidence) == "" {
			evidence = "No evidence supplied"
		}
		fmt.Fprintf(&b, "%d. %s\nEvidence: %s\n", g.Index, validation.EscapeDiagnostic(g.Criterion), validation.EscapeDiagnostic(evidence))
	}
	if gaps[0].Findings != "" {
		fmt.Fprintf(&b, "Tester findings: %s\n", validation.EscapeDiagnostic(gaps[0].Findings))
	}
	return b.String()
}
