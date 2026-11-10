package lease

import (
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
)

// Check output is whatever a required check printed, so it can name local
// paths and users. It belongs in the state ref, never in the issue comment.
func TestIssueCommentOmitsRawCheckOutput(t *testing.T) {
	body := "# Validation\n\n| Check | Result |\n| --- | --- |\n| unit | FAIL |\n\n### Output: `unit`\n```text\nrootdir: /home/operator-name/src/project\n```\n"
	st := &IssueState{Artifacts: []contract.Artifact{{Type: contract.ArtifactValidation, Verdict: "FAIL", Body: body}}}
	st.Lease.IssueNumber = 7
	comment := RenderStateProjection(&contract.WorkUnit{IssueNumber: 7}, st, coordination.RefLayout{})
	if strings.Contains(comment, "operator-name") || strings.Contains(comment, "rootdir") {
		t.Fatalf("raw check output reached the issue comment:\n%s", comment)
	}
	for _, want := range []string{"| unit | FAIL |", "Check output is stored in"} {
		if !strings.Contains(comment, want) {
			t.Fatalf("missing %q in:\n%s", want, comment)
		}
	}
	// Other artifacts are projected unchanged.
	plan := &IssueState{Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: "# Plan\n\n### Output: kept\n"}}}
	if got := RenderStateProjection(&contract.WorkUnit{IssueNumber: 7}, plan, coordination.RefLayout{}); !strings.Contains(got, "### Output: kept") {
		t.Fatal(got)
	}
}

// A node ID derived from the machine's hostname is not recorded in state; an
// ID the operator chose is.
func TestRecordableNodeIDDropsHostname(t *testing.T) {
	old := nodeHostname
	nodeHostname = func() (string, error) { return "Workstation.example.internal", nil }
	t.Cleanup(func() { nodeHostname = old })
	for id, want := range map[string]string{
		"workstation-linux-amd64":      "",
		"Workstation":                  "",
		"workstation.example.internal": "",
		"pool-worker-3":                "pool-worker-3",
		"":                             "",
	} {
		if got := recordableNodeID(id); got != want {
			t.Errorf("recordableNodeID(%q) = %q, want %q", id, got, want)
		}
	}
}

// A fork request cannot ask a phase update to record an arbitrary status.
func TestValidateRequestedPhaseStatus(t *testing.T) {
	for _, ok := range []string{"", "active"} {
		if err := ValidateRequestedPhaseStatus(ok); err != nil {
			t.Fatalf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"completed", "released", "handoff", "Active", "<b>anything</b>"} {
		if err := ValidateRequestedPhaseStatus(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
