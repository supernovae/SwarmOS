package lease

import (
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestRebaseMicroPlanNeverSatisfiesReviewGate(t *testing.T) {
	t.Parallel()
	wu := &contract.WorkUnit{IssueNumber: 1, Objective: "preserve behavior"}
	for _, gate := range []contract.GateMode{contract.GateAgent, contract.GateHuman} {
		st := &IssueState{TaskDigest: contract.DigestWorkUnit(wu), EffectiveGate: gate, LastRebase: &RebaseEvidence{Plans: []RebasePlanReport{{Body: "VERDICT: APPROVED"}}}}
		if VerifyApproval(st, wu) == nil {
			t.Fatalf("micro-plan satisfied %s gate", gate)
		}
	}
}
