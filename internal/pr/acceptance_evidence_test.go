package pr

import (
	"encoding/json"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workflow"
	"testing"
)

func TestPRSubmitAcceptanceEvidenceAdvisory(t *testing.T) {
	for _, mode := range []string{"met", "unmet", "missing", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			s, issue, number, _ := fixture(t)
			s.Version = "v1.2.3"
			r, err := s.Submit(t.Context(), number)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != "submitted" || len(r.AcceptanceEvidenceWarnings) == 0 {
				t.Fatalf("%+v", r)
			}
			_, err = s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Validate(t.Context(), number)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.approvedAcceptance) != 1 {
				t.Fatal(r.approvedAcceptance)
			}
			criterion := r.approvedAcceptance[0]
			mapping, _ := json.Marshal(map[string]any{"version": 1, "criteria": []map[string]any{{"index": 1, "criterion": criterion, "status": mode, "evidence": "VALIDATION.md: observation"}}})
			body := validation.AcceptanceEvidenceStart + "\n" + string(mapping) + "\n" + validation.AcceptanceEvidenceEnd
			if mode == "missing" {
				body = ""
			}
			if mode == "malformed" {
				body = validation.AcceptanceEvidenceStart
			}
			art := r.Validation.Artifact
			validation.StoreAcceptanceSnapshot(art.Metadata, validation.CaptureAcceptanceEvidence(body))
			art.Body += "\n" + body
			if _, err = s.Manager.RecordTrustedValidation(t.Context(), issue, "alice", art); err != nil {
				t.Fatal(err)
			}
			for retry := 0; retry < 2; retry++ {
				r, err = s.Submit(t.Context(), number)
				if err != nil {
					t.Fatal(err)
				}
				if r.Status != "submitted" {
					t.Fatal(r.Status)
				}
				expected := mode != "met"
				if (len(r.AcceptanceEvidenceWarnings) > 0) != expected {
					t.Fatalf("mode=%s retry=%d warnings=%+v", mode, retry, r.AcceptanceEvidenceWarnings)
				}
				data, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err = json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				_, exists := fields["acceptance_evidence_warnings"]
				if exists != expected {
					t.Fatal(string(data))
				}
				if expected {
					want := mode
					if mode == "missing" {
						want = "unmentioned"
					}
					if r.AcceptanceEvidenceWarnings[0].Reason != want {
						t.Fatal(r.AcceptanceEvidenceWarnings)
					}
				}
			}
		})
	}
}
