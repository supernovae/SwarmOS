package lease

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestVerifyApprovalTypesHumanDecisions(t *testing.T) {
	t.Parallel()
	wu := &contract.WorkUnit{IssueNumber: 3, Title: "Task", Objective: "Do it", Acceptance: []string{"done"}}
	digest := contract.DigestWorkUnit(wu)
	for _, tc := range []struct {
		name  string
		st    *IssueState
		task  bool
		human bool
	}{
		{"missing state stays ambiguous", nil, false, false},
		{"changed WorkUnit needs issue approve", &IssueState{TaskDigest: "stale", EffectiveGate: contract.GateNone}, true, false},
		{"human gate without decision", &IssueState{TaskDigest: digest, EffectiveGate: contract.GateHuman}, false, true},
		{"agent gate without review", &IssueState{TaskDigest: digest, EffectiveGate: contract.GateAgent}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyApproval(tc.st, wu)
			var task *TaskApprovalError
			var human *HumanPlanApprovalError
			if !errors.Is(err, ErrApprovalRequired) || !strings.Contains(err.Error(), "approval is missing or stale") {
				t.Fatalf("err = %v", err)
			}
			if errors.As(err, &task) != tc.task || errors.As(err, &human) != tc.human {
				t.Fatalf("err = %#v", err)
			}
			if tc.task && task.IssueNumber != 3 || tc.human && human.IssueNumber != 3 {
				t.Fatalf("issue number lost: %#v", err)
			}
		})
	}
	for _, mode := range []string{"missing both", "missing design", "missing tests", "missing review", "advisory", "stale", "task first"} {
		t.Run(mode, func(t *testing.T) {
			st := &IssueState{TaskDigest: digest, EffectiveGate: contract.GateAgent}
			if mode != "missing both" && mode != "missing design" {
				st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactDesign, Body: "plan"})
			}
			if mode != "missing both" && mode != "missing tests" {
				st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactTestPlan, Body: "tests"})
			}
			if mode == "advisory" || mode == "stale" {
				st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactReview, Verdict: "APPROVED", Metadata: map[string]string{"review_source": "contributor", "plan_digest": "stale"}})
			}
			if mode == "task first" {
				st.TaskDigest = "changed"
			}
			err := VerifyApproval(st, wu)
			var plan *PlanApprovalError
			typed := mode == "missing review" || mode == "advisory" || mode == "stale"
			if !errors.Is(err, ErrApprovalRequired) || !strings.Contains(err.Error(), "approval is missing or stale") || errors.As(fmt.Errorf("outer: %w", err), &plan) != typed {
				t.Fatalf("approval = %#v", err)
			}
			if typed && plan.IssueNumber != 3 {
				t.Fatalf("issue = %d", plan.IssueNumber)
			}
		})
	}
	// Use the existing manager fixture to obtain authenticated, bound evidence.
	m, _, iss := testManager(t)
	ctx := context.Background()
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "claude", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	for _, art := range []contract.Artifact{{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan evidence", Body: "plan"}, {Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "plan evidence", Body: "tests"}} {
		if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", art); err != nil {
			t.Fatal(err)
		}
	}
	boundWU, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"human", "agent"} {
		t.Run("authenticated "+kind, func(t *testing.T) {
			var st *IssueState
			var err error
			if kind == "human" {
				st, err = m.RecordHumanReview(ctx, iss.Number, "APPROVED", "Reviewed the current plan")
			} else {
				before, loadErr := m.GetCurrentState(ctx, iss.Number)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				metadata := ReviewBindings(before)
				metadata["kind"] = "agent"
				st, err = m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactReview, Producer: "mock:reviewer", Summary: "review", Verdict: "APPROVED", Body: "VERDICT: APPROVED\n", Metadata: metadata})
			}
			if err != nil {
				t.Fatal(err)
			}
			st.EffectiveGate = contract.GateAgent
			if err := VerifyApproval(st, boundWU); err != nil {
				t.Fatalf("current review: %v", err)
			}
			if kind == "agent" {
				st.EffectiveGate = contract.GateHuman
				var human *HumanPlanApprovalError
				if err := VerifyApproval(st, boundWU); !errors.As(err, &human) {
					t.Fatalf("agent satisfied human gate: %v", err)
				}
				st.EffectiveGate = contract.GateAgent
			}
			st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactDesign, Body: "new current plan"})
			var plan *PlanApprovalError
			if err := VerifyApproval(st, boundWU); !errors.Is(err, ErrApprovalRequired) || !errors.As(err, &plan) {
				t.Fatalf("stale review: %v", err)
			}
		})
	}

	if err := VerifyApproval(nil, nil); !errors.Is(err, ErrApprovalRequired) {
		t.Fatal(err)
	}

	if err := VerifyApproval(&IssueState{TaskDigest: digest, EffectiveGate: contract.GateNone}, wu); err != nil {
		t.Fatalf("no-gate approval = %v", err)
	}
}

func TestPlanApprovalDiagnosticCompatibility(t *testing.T) {
	for _, cause := range []error{nil, fmt.Errorf("%w: diagnostic suffix", ErrApprovalRequired)} {
		for _, err := range []error{&HumanPlanApprovalError{Err: cause}, &PlanApprovalError{Err: cause}} {
			if !errors.Is(err, ErrApprovalRequired) || !strings.Contains(err.Error(), "plan approval") || strings.Contains(err.Error(), "task approval") {
				t.Fatalf("plan diagnostic: %v", err)
			}
			if cause != nil && (!errors.Is(err, cause) || !strings.Contains(err.Error(), "diagnostic suffix")) {
				t.Fatal(err)
			}
		}
	}
}

func TestVerifyHumanPlanApprovalReadOnly(t *testing.T) {
	if VerifyHumanPlanApproval(nil) != nil || VerifyHumanPlanApproval(&IssueState{EffectiveGate: contract.GateAgent}) != nil {
		t.Fatal("non-human check")
	}
	st := &IssueState{EffectiveGate: contract.GateHuman}
	st.Lease.IssueNumber = 17
	err := VerifyHumanPlanApproval(st)
	var human *HumanPlanApprovalError
	if !errors.As(err, &human) || human.IssueNumber != 17 || !errors.Is(err, ErrApprovalRequired) {
		t.Fatal(err)
	}
}
