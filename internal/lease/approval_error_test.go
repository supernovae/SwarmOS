package lease

import (
	"errors"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestVerifyApprovalTypesHumanDecisions(t *testing.T) {
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
			if !errors.Is(err, ErrApprovalRequired) || err.Error() != ErrApprovalRequired.Error() {
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
	if err := VerifyApproval(&IssueState{TaskDigest: digest, EffectiveGate: contract.GateNone}, wu); err != nil {
		t.Fatalf("no-gate approval = %v", err)
	}
}
