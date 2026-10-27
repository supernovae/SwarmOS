package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

func revisionState() *IssueState {
	return &IssueState{TaskDigest: "task", PolicyDigest: "policy", BaseSHA: "base", EffectiveGate: contract.GateNone, Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: "original"}, {Type: contract.ArtifactTestPlan, Body: "original tests"}}}
}
func TestRevisionContextLifecycle(t *testing.T) {
	st := revisionState()
	if err := captureRevisionPlan(st); err != nil {
		t.Fatal(err)
	}
	invalidatePRRevisionEvidence(st)
	if st.RevisionPlan == nil || st.RevisionPlan.Original.Plan != "original" || len(st.Artifacts) != 0 {
		t.Fatal(st)
	}
	for i := 0; i < 3; i++ {
		st.Artifacts = []contract.Artifact{{Type: contract.ArtifactDesign, Body: fmt.Sprintf("delta %d", i)}, {Type: contract.ArtifactTestPlan, Body: "changed tests"}}
		if err := captureRevisionPlan(st); err != nil {
			t.Fatal(err)
		}
		if err := captureRevisionPlan(st); err != nil {
			t.Fatal(err)
		}
		if len(st.RevisionPlan.Accepted) != i+1 {
			t.Fatal("duplicate or missing delta")
		}
	}
	if st.RevisionPlan.Original.Plan != "original" {
		t.Fatal("original changed")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var restored IssueState
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.RevisionPlan.Original.Plan != "original" {
		t.Fatal("lost context")
	}
	if !strings.Contains(PlanSummary(&restored, true), "Current revision plan") {
		t.Fatal("missing output")
	}
	var legacy IssueState
	if err = json.Unmarshal([]byte(`{"task_digest":"task"}`), &legacy); err != nil || legacy.RevisionPlan != nil {
		t.Fatal(err)
	}
}
func TestRevisionContextEligibility(t *testing.T) {
	for _, name := range []string{"missing plan", "missing tests", "missing identity", "agent unapproved", "human unapproved", "stale review"} {
		t.Run(name, func(t *testing.T) {
			st := revisionState()
			switch name {
			case "missing plan":
				st.Artifacts = st.Artifacts[1:]
			case "missing tests":
				st.Artifacts = st.Artifacts[:1]
			case "missing identity":
				st.TaskDigest = ""
			case "agent unapproved":
				st.EffectiveGate = contract.GateAgent
			case "human unapproved":
				st.EffectiveGate = contract.GateHuman
			case "stale review":
				st.EffectiveGate = contract.GateAgent
				st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactReview, Verdict: "APPROVED", Body: "VERDICT: APPROVED"})
			}
			if err := captureRevisionPlan(st); err != nil {
				t.Fatal(err)
			}
			if st.RevisionPlan != nil {
				t.Fatal("ineligible pair promoted")
			}
		})
	}
	st := revisionState()
	_ = captureRevisionPlan(st)
	st.TaskDigest = "new"
	st.Artifacts = nil
	_ = captureRevisionPlan(st)
	if st.RevisionPlan != nil {
		t.Fatal("stale identity retained")
	}
}
func TestRevisionBindingsUseActivePair(t *testing.T) {
	st := revisionState()
	_ = captureRevisionPlan(st)
	st.Artifacts[0].Body = "delta"
	st.Artifacts[1].Body = "delta tests"
	bindings := ReviewBindings(st)
	if bindings["plan_digest"] != hash("delta") || bindings["test_plan_digest"] != hash("delta tests") {
		t.Fatal(bindings)
	}
	if bindings["plan_digest"] == st.RevisionPlan.Original.PlanDigest {
		t.Fatal("original approval reused")
	}
}
func TestRevisionHistoryBound(t *testing.T) {
	st := revisionState()
	_ = captureRevisionPlan(st)
	for i := 0; i < 16; i++ {
		st.Artifacts[0].Body = fmt.Sprint(i)
		if err := captureRevisionPlan(st); err != nil {
			t.Fatal(err)
		}
	}
	st.Artifacts[0].Body = "too many"
	if err := captureRevisionPlan(st); err == nil || len(st.RevisionPlan.Accepted) != 16 {
		t.Fatal("history not bounded")
	}
}

func TestRebaseToWorkCapturesOriginal(t *testing.T) {
	m, n, st := freshPlanRebaseFixture(t)
	current, rev, err := m.load(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	current.EffectiveGate = contract.GateNone
	if _, err = m.save(t.Context(), n, rev, current); err != nil {
		t.Fatal(err)
	}
	result, err := m.BeginPRRevision(t.Context(), n, "local-dev", 37, st.PRRevision.RemoteHeadSHA, st.PRRevision.LocalHeadSHA, st.PRRevision.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if result.RevisionPlan == nil || result.RevisionPlan.Original.Plan != "old evidence" || len(result.Artifacts) != 0 {
		t.Fatal("conversion lost original")
	}
	resumed, err := m.BeginPRRevision(t.Context(), n, "local-dev", 37, st.PRRevision.RemoteHeadSHA, st.PRRevision.LocalHeadSHA, st.PRRevision.Branch)
	if err != nil || resumed.RevisionPlan == nil || len(resumed.RevisionPlan.Accepted) != 0 {
		t.Fatal("resume promoted pending delta", err)
	}
}

func TestOriginalReviewCannotApproveRevision(t *testing.T) {
	m, _, issue := testManager(t)
	ctx := t.Context()
	if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	for _, a := range []contract.Artifact{{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "original"}, {Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "original tests"}} {
		if _, err := m.UpdateArtifact(ctx, issue.Number, "local-dev", a); err != nil {
			t.Fatal(err)
		}
	}
	st, err := m.RecordHumanReview(ctx, issue.Number, "APPROVED", "checked")
	if err != nil {
		t.Fatal(err)
	}
	if err := captureRevisionPlan(st); err != nil || st.RevisionPlan == nil {
		t.Fatal("authenticated original ineligible", err)
	}
	for _, which := range []int{0, 1} {
		copy := *st
		copy.Artifacts = append([]contract.Artifact(nil), st.Artifacts...)
		copy.Artifacts[which].Body = "revision changed"
		if err := verifyReviewBinding(&copy, contract.GateHuman); err == nil {
			t.Fatal("original review approved changed revision")
		}
	}
	st.EffectiveGate = contract.GateNone
	for i := range st.Artifacts {
		if st.Artifacts[i].Type == contract.ArtifactReview {
			st.Artifacts[i].Verdict = "CHANGES_REQUESTED"
		}
	}
	st.Artifacts[0].Body = "rejected delta"
	if err := captureRevisionPlan(st); err != nil || len(st.RevisionPlan.Accepted) != 0 {
		t.Fatal("rejected delta promoted", err)
	}
}

func TestHumanReviewAfterSubmissionWithLinkedPR(t *testing.T) {
	m, _, issue := testManager(t)
	ctx := t.Context()
	if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	for _, a := range []contract.Artifact{{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "original"}, {Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "original tests"}} {
		if _, err := m.UpdateArtifact(ctx, issue.Number, "local-dev", a); err != nil {
			t.Fatal(err)
		}
	}
	complete := func(pr int) {
		t.Helper()
		st, rev, err := m.load(ctx, issue.Number)
		if err != nil {
			t.Fatal(err)
		}
		st.Lease.Status, st.PRNumber = "completed", pr
		if _, err := m.save(ctx, issue.Number, rev, st); err != nil {
			t.Fatal(err)
		}
	}
	complete(0)
	if _, err := m.RecordHumanReview(ctx, issue.Number, "APPROVED", "checked"); !errors.Is(err, ErrApprovalRequired) || !strings.Contains(err.Error(), "no linked PR") {
		t.Fatalf("completed issue without PR accepted review: %v", err)
	}
	complete(42)
	st, err := m.RecordHumanReview(ctx, issue.Number, "APPROVED", "re-reviewed after policy move")
	if err != nil {
		t.Fatalf("submitted issue with linked PR refused human review: %v", err)
	}
	if err := verifyReviewBinding(st, contract.GateHuman); err != nil {
		t.Fatalf("recorded review does not satisfy the human gate: %v", err)
	}
}
