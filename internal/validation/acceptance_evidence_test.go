package validation

import (
	"encoding/json"
	"github.com/supernovae/swarmos/internal/contract"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func evidenceBlock(raw string) string {
	return AcceptanceEvidenceStart + "\n" + raw + "\n" + AcceptanceEvidenceEnd
}
func TestAcceptanceEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       string
	}{
		{"missing", "prose", "unmentioned"},
		{"met", evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"met","evidence":"fictional/nonexistent: 12s"}]}`), ""},
		{"unmet", evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"unmet"}]}`), "unmet"},
		{"blank", evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"met","evidence":" "}]}`), "malformed"},
		{"unknown status", evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"maybe"}]}`), "malformed"},
		{"duplicate", evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"unmet"},{"index":1,"criterion":"works","status":"met","evidence":"x"}]}`), "malformed"},
		{"mismatch", evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"- [ ] works","status":"unmet"}]}`), "malformed"},
		{"trim", evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":" works ","status":"met","evidence":"x"}],"extra":true}`), ""},
		{"outside", evidenceBlock(`{"version":1,"criteria":[{"index":2,"criterion":"works","status":"met","evidence":"x"}]}`), "unmentioned"},
		{"empty", evidenceBlock(`{"version":1,"criteria":[]}`), "unmentioned"},
		{"null", evidenceBlock(`{"version":1,"criteria":null}`), "malformed"},
		{"version", evidenceBlock(`{"version":2,"criteria":[]}`), "malformed"},
		{"json", evidenceBlock(`{`), "malformed"},
		{"trailing", evidenceBlock(`{"version":1,"criteria":[]} {}`), "malformed"},
		{"incomplete", AcceptanceEvidenceStart, "malformed"},
		{"end only", AcceptanceEvidenceEnd, "malformed"},
		{"inline", "prefix" + evidenceBlock(`{"version":1,"criteria":[]}`), "malformed"},
		{"multiple", evidenceBlock(`{}`) + "\n" + evidenceBlock(`{}`), "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := CaptureAcceptanceEvidence(tc.body)
			metadata := map[string]string{}
			StoreAcceptanceSnapshot(metadata, snapshot)
			got := AcceptanceEvidenceWarnings([]string{"works"}, AcceptanceSnapshotFromMetadata(metadata))
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatal(got)
				}
				return
			}
			if len(got) != 1 || got[0].Reason != tc.want || got[0].Index != 1 || got[0].Criterion != "works" {
				t.Fatal(got)
			}
		})
	}
}
func TestAcceptanceEvidenceBoundsOrderAndNotice(t *testing.T) {
	raw := `{"version":1,"criteria":[]}`
	raw += strings.Repeat(" ", MaxAcceptanceEvidenceBytes-len(raw)-2)
	if got := CaptureAcceptanceEvidence(AcceptanceEvidenceStart + "\n" + raw + "\n" + AcceptanceEvidenceEnd); got.State != "valid" {
		t.Fatal(got.State)
	}
	if got := CaptureAcceptanceEvidence(AcceptanceEvidenceStart + "\n" + raw + " \n" + AcceptanceEvidenceEnd); got.State != "malformed" || got.Block != "" {
		t.Fatal(got.State)
	}
	snapshot := CaptureAcceptanceEvidence(evidenceBlock(`{"version":1,"criteria":[{"index":2,"criterion":"same","status":"met","evidence":"x"},{"index":3,"criterion":"last","status":"unmet"}]}`))
	got := AcceptanceEvidenceWarnings([]string{"same", "same", "last", "\u001b[31m\n"}, snapshot)
	if len(got) != 3 || got[0].Index != 1 || got[1].Index != 3 || got[1].Reason != "unmet" {
		t.Fatal(got)
	}
	notice := AcceptanceEvidenceNotice(got)
	if strings.ContainsAny(notice, "\u001b") || !strings.Contains(notice, "advisory") {
		t.Fatal(notice)
	}
	if len(AcceptanceEvidenceWarnings(nil, snapshot)) != 0 || AcceptanceEvidenceNotice(nil) != "" {
		t.Fatal("empty criteria")
	}
	metadata := map[string]string{}
	StoreAcceptanceSnapshot(metadata, snapshot)
	body := RestoreAcceptanceEvidence("checks", metadata)
	if RestoreAcceptanceEvidence(body, metadata) != body {
		t.Fatal("accumulation")
	}
	guided := RestoreAcceptanceEvidence("checks\n<!-- swarm:guidance:start -->\noperator\n<!-- swarm:guidance:end -->\n", metadata)
	if !strings.Contains(contract.CanonicalArtifactBody(guided), snapshot.Block) {
		t.Fatal("mapping lost into guidance appendix")
	}
	var v any
	if json.Unmarshal([]byte(metadata[AcceptanceEvidenceKey]), &v) != nil {
		t.Fatal(metadata)
	}
}

func TestAcceptanceEvidenceRepeatedReuse(t *testing.T) {
	root := setupGitRepoForValidation(t)
	if err := os.WriteFile(filepath.Join(root, ".git/info/exclude"), []byte(".swarm/active/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := ValidateOptions{BaseSHA: gitRevision(t, root, "HEAD"), Version: "v1.2.3", ClaimID: "claim"}
	report, err := Validate(t.Context(), root, &contract.WorkUnit{IssueNumber: 1}, opts)
	if err != nil {
		t.Fatal(err)
	}
	art := report.Artifact
	// Model the fenced coordinator stamp, as in TestFingerprintReuseBindings.
	art.Metadata["validation_trusted_origin"] = "coordinator"
	art.Metadata["validation_trusted_owner"] = "owner"
	art.Metadata["validation_trusted_claim"] = "claim"
	snapshot := CaptureAcceptanceEvidence(evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"met","evidence":"VALIDATION.md: result"}]}`))
	StoreAcceptanceSnapshot(art.Metadata, snapshot)
	for i := 0; i < 3; i++ {
		reused, err := Reuse(t.Context(), root, opts, "owner", []contract.Artifact{art})
		if err != nil || reused == nil {
			t.Fatal(err)
		}
		art = reused.Artifact
		if strings.Count(art.Body, AcceptanceEvidenceStart) != 1 || len(AcceptanceEvidenceWarnings([]string{"works"}, AcceptanceSnapshotFromMetadata(art.Metadata))) != 0 {
			t.Fatal(art)
		}
	}
	// A same-tree new commit may reuse checks, but not the older tester mapping.
	if _, err := gitOutput(root, "commit", "--allow-empty", "-m", "new head"); err != nil {
		t.Fatal(err)
	}
	reused, err := Reuse(t.Context(), root, opts, "owner", []contract.Artifact{art})
	if err != nil || reused == nil {
		t.Fatal(err)
	}
	if AcceptanceSnapshotFromMetadata(reused.Artifact.Metadata).State != "missing" {
		t.Fatal("stale head carried evidence")
	}
	// Check-output delimiters must not suppress the real separately captured block.
	metadata := map[string]string{}
	StoreAcceptanceSnapshot(metadata, snapshot)
	spoof := evidenceBlock(`{"version":1,"criteria":[]}`)
	restored := RestoreAcceptanceEvidence(spoof, metadata)
	if !strings.Contains(restored, snapshot.Block) || RestoreAcceptanceEvidence(restored, metadata) != restored {
		t.Fatal(restored)
	}
}
