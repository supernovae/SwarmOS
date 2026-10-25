package lease

import (
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
)

// PreserveTesterGuidance carries the last tester's delivered guidance when
// required checks are repeated without invoking another tester agent.
func PreserveTesterGuidance(st *IssueState, artifact *contract.Artifact) {
	if st == nil || artifact == nil {
		return
	}
	for _, previous := range st.Artifacts {
		if previous.Type != contract.ArtifactValidation || previous.Metadata["operator_steered"] != "true" {
			continue
		}
		if artifact.Metadata == nil {
			artifact.Metadata = map[string]string{}
		}
		for _, key := range []string{"operator_steered", "guidance_count", "guidance_sha256"} {
			artifact.Metadata[key] = previous.Metadata[key]
		}
		canonical := contract.CanonicalArtifactBody(previous.Body)
		if len(canonical) < len(previous.Body) && !strings.Contains(artifact.Body, "<!-- swarm:guidance:start -->") {
			artifact.Body += previous.Body[len(canonical):]
		}
	}
}
