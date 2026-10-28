package lease

import (
	"github.com/supernovae/swarmos/internal/contract"
	"strings"
	"testing"
)

func TestRepeatedChecksRetainTesterGuidance(t *testing.T) {
	t.Parallel()
	appendix := "\n<!-- swarm:guidance:start -->\n## Operator guidance\n\n> Use local fixtures\n\n<!-- swarm:guidance:end -->\n"
	st := &IssueState{Artifacts: []contract.Artifact{{Type: contract.ArtifactValidation, Body: "Previous check evidence" + appendix, Metadata: map[string]string{"operator_steered": "true", "guidance_count": "1", "guidance_sha256": "digest"}}}}
	next := contract.Artifact{Type: contract.ArtifactValidation, Body: "New check evidence"}
	PreserveTesterGuidance(st, &next)
	PreserveTesterGuidance(st, &next)
	if next.Metadata["operator_steered"] != "true" || strings.Count(next.Body, "Use local fixtures") != 1 || strings.Contains(next.Body, "Previous check evidence") {
		t.Fatalf("%+v", next)
	}
}
