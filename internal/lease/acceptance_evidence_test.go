package lease

import (
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/validation"
	"strings"
	"testing"
)

func TestAcceptanceEvidenceCarryBindings(t *testing.T) {
	for _, change := range []string{"", "head_sha", "base_sha", "validation_claim", "validation_base", "validation_tree", "validation_policy", "validation_trusted_origin", "validation_trusted_owner", "validation_trusted_claim", "newer-missing", "newer-malformed", "newer-unmet"} {
		t.Run(change, func(t *testing.T) {
			metadata := map[string]string{"head_sha": "head", "base_sha": "base", "validation_claim": "claim", "validation_base": "base", "validation_tree": "tree", "validation_policy": "policy", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "owner", "validation_trusted_claim": "claim"}
			validation.StoreAcceptanceSnapshot(metadata, validation.CaptureAcceptanceEvidence("## Independent acceptance review 1\nThe original finding.\n"+validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"met","evidence":"Independent acceptance review 1"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
			dst := contract.Artifact{Body: "checks", Metadata: map[string]string{}}
			for k, v := range metadata {
				if k != validation.AcceptanceEvidenceKey {
					dst.Metadata[k] = v
				}
			}
			src := contract.Artifact{Type: contract.ArtifactValidation, Metadata: metadata}
			st := &IssueState{BaseSHA: "base", HeadSHA: "head", Lease: contract.Lease{Owner: "owner", ClaimID: "claim"}, Artifacts: []contract.Artifact{src}}
			if strings.HasPrefix(change, "newer-") {
				next := contract.Artifact{Type: contract.ArtifactValidation, Metadata: map[string]string{}}
				for k, v := range metadata {
					next.Metadata[k] = v
				}
				delete(next.Metadata, validation.AcceptanceEvidenceKey)
				if change == "newer-malformed" {
					validation.StoreAcceptanceSnapshot(next.Metadata, validation.AcceptanceSnapshot{State: "malformed"})
				}
				if change == "newer-unmet" {
					validation.StoreAcceptanceSnapshot(next.Metadata, validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"unmet"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
				}
				st.Artifacts = append(st.Artifacts, next)
			} else if change != "" {
				metadata[change] = "different"
			}
			PreserveAcceptanceEvidence(st, &dst)
			if strings.Contains(dst.Body, "The original finding.") != (change == "") {
				t.Fatal("stale or lost findings", change, dst.Body)
			}
			body := dst.Body
			PreserveAcceptanceEvidence(st, &dst)
			if body != dst.Body {
				t.Fatal("accumulation")
			}
			got := validation.AcceptanceEvidenceWarnings([]string{"works"}, validation.AcceptanceSnapshotFromMetadata(dst.Metadata))
			if (len(got) == 0) != (change == "") {
				t.Fatal(got)
			}
			retry := RecordedAcceptanceWarnings(st, []string{"works"})
			// Retry has no independent current tree/policy; it still rejects state identity mismatches.
			if change == "" && len(retry) != 0 {
				t.Fatal(retry)
			}
		})
	}
}
func TestNewTesterSnapshotReplacesPrevious(t *testing.T) {
	for _, state := range []string{"missing", "malformed", "valid"} {
		t.Run(state, func(t *testing.T) {
			bindings := map[string]string{"head_sha": "head", "base_sha": "base", "validation_claim": "claim", "validation_base": "base", "validation_tree": "tree", "validation_policy": "policy", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "owner", "validation_trusted_claim": "claim"}
			src := contract.Artifact{Type: contract.ArtifactValidation, Metadata: map[string]string{}}
			dst := contract.Artifact{Metadata: map[string]string{}}
			for k, v := range bindings {
				src.Metadata[k] = v
				dst.Metadata[k] = v
			}
			validation.StoreAcceptanceSnapshot(src.Metadata, validation.CaptureAcceptanceEvidence("Old finding must disappear.\n"+validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"met","evidence":"old"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
			snapshot := validation.AcceptanceSnapshot{State: state}
			if state == "valid" {
				snapshot = validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart + "\n" + `{"version":1,"criteria":[{"index":1,"criterion":"works","status":"unmet"}]}` + "\n" + validation.AcceptanceEvidenceEnd)
			}
			validation.StoreAcceptanceSnapshot(dst.Metadata, snapshot)
			original := dst.Metadata[validation.AcceptanceEvidenceKey]
			st := &IssueState{BaseSHA: "base", HeadSHA: "head", Lease: contract.Lease{Owner: "owner", ClaimID: "claim"}, Artifacts: []contract.Artifact{src}}
			PreserveAcceptanceEvidence(st, &dst)
			if strings.Contains(dst.Body, "Old finding must disappear.") {
				t.Fatal("resurrected old finding")
			}
			if dst.Metadata[validation.AcceptanceEvidenceKey] != original {
				t.Fatal("old evidence replaced new snapshot")
			}
		})
	}
}

func TestRecordedAcceptanceReadiness(t *testing.T) {
	for _, change := range []string{"", "head_sha", "base_sha", "validation_base", "validation_claim", "validation_trusted_origin", "validation_trusted_owner", "validation_trusted_claim", "validation_tree", "validation_policy", "newer-missing", "newer-malformed", "newer-unmet"} {
		t.Run(change, func(t *testing.T) {
			md := map[string]string{"head_sha": "head", "base_sha": "base", "validation_base": "base", "validation_claim": "claim", "validation_tree": "tree", "validation_policy": "policy", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "owner", "validation_trusted_claim": "claim"}
			validation.StoreAcceptanceSnapshot(md, validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"external","reason":"Windows host"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
			art := contract.Artifact{Type: contract.ArtifactValidation, Verdict: "PASS", Metadata: md}
			st := &IssueState{HeadSHA: "head", BaseSHA: "base", Lease: contract.Lease{Owner: "owner", ClaimID: "claim"}, Artifacts: []contract.Artifact{art}}
			if strings.HasPrefix(change, "newer-") {
				next := art
				next.Metadata = map[string]string{}
				for k, v := range md {
					next.Metadata[k] = v
				}
				delete(next.Metadata, validation.AcceptanceEvidenceKey)
				if change == "newer-malformed" {
					validation.StoreAcceptanceSnapshot(next.Metadata, validation.AcceptanceSnapshot{State: "malformed"})
				}
				if change == "newer-unmet" {
					validation.StoreAcceptanceSnapshot(next.Metadata, validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"unmet"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
				}
				st.Artifacts = append(st.Artifacts, next)
			} else if change != "" {
				md[change] = ""
			}
			got := RecordedReadiness(st, []string{"works"})
			if (got.AcceptanceEvidence.State == "complete") != (change == "") {
				t.Fatal(got)
			}
			if change == "" && (got.RequiredChecks != "passed" || got.ExternalVerification != "pending") {
				t.Fatal(got)
			}
			if change != "" && !strings.HasPrefix(change, "newer-") && got.RequiredChecks != "unavailable" {
				t.Fatal(got)
			}
		})
	}
}
