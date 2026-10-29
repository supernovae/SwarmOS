package lease

import (
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/validation"
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
