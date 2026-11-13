package lease

import (
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/validation"
)

func TestRepairAcceptanceSurfacesUnmetFromCurrentFailedReport(t *testing.T) {
	evidence := func(status string) map[string]string {
		body := validation.AcceptanceEvidenceStart + "\n" + `{"version":1,"criteria":[{"index":1,"criterion":"one","status":"` + status + `","evidence":"VALIDATION.md: review"}]}` + "\n" + validation.AcceptanceEvidenceEnd
		md := map[string]string{"head_sha": "h", "base_sha": "b", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "o", "validation_trusted_claim": "c"}
		validation.StoreAcceptanceSnapshot(md, validation.CaptureAcceptanceEvidence(body))
		return md
	}
	state := func(verdict string, md map[string]string) *IssueState {
		return &IssueState{HeadSHA: "h", BaseSHA: "b", Lease: contract.Lease{Owner: "o", ClaimID: "c"}, Artifacts: []contract.Artifact{{Type: contract.ArtifactValidation, Verdict: verdict, Metadata: md}}}
	}
	if !RepairAcceptance(state("FAIL", evidence("unmet")), []string{"one"}).NeedsRepair() {
		t.Fatal("unmet criterion on a failed report was hidden from repair")
	}
	// Submission and status readiness are unchanged.
	if RecordedReadiness(state("FAIL", evidence("unmet")), []string{"one"}).AcceptanceEvidence.State != "missing" {
		t.Fatal("recorded readiness changed")
	}
	// A failed report never makes acceptance complete.
	if got := RepairAcceptance(state("FAIL", evidence("met")), []string{"one"}); got.State == "complete" {
		t.Fatal("failed report reported acceptance complete")
	}
	for name, mutate := range map[string]func(*IssueState){
		"stale claim": func(st *IssueState) { st.Lease.ClaimID = "successor" },
		"other owner": func(st *IssueState) { st.Lease.Owner = "other" },
		"new head":    func(st *IssueState) { st.HeadSHA = "h2" },
		"new base":    func(st *IssueState) { st.BaseSHA = "b2" },
		"untrusted":   func(st *IssueState) { delete(st.Artifacts[0].Metadata, "validation_trusted_origin") },
	} {
		st := state("FAIL", evidence("unmet"))
		mutate(st)
		if RepairAcceptance(st, []string{"one"}).NeedsRepair() {
			t.Fatalf("%s: repair used evidence it is not bound to", name)
		}
	}
}
