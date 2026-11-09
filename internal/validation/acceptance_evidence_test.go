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
	snapshot := CaptureAcceptanceEvidence("## Independent acceptance review 1\nThe behavior loses data.\n" + evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"unmet","evidence":"Independent acceptance review 1"}]}`))
	StoreAcceptanceSnapshot(art.Metadata, snapshot)
	for i := 0; i < 3; i++ {
		reused, err := Reuse(t.Context(), root, opts, "owner", []contract.Artifact{art})
		if err != nil || reused == nil {
			t.Fatal(err)
		}
		art = reused.Artifact
		if strings.Count(art.Body, AcceptanceEvidenceStart) != 1 || strings.Count(art.Body, "The behavior loses data.") != 1 || !strings.Contains(art.Body, "Independent acceptance review 1") {
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

func TestAcceptanceAssessment(t *testing.T) {
	for _, tc := range []struct{ raw, state, external string }{
		{`{"version":1,"criteria":[]}`, "incomplete", "none"},
		{`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"external","reason":"Windows host"}]}`, "complete", "pending"},
		{`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"external","reason":" "}]}`, "malformed", "none"},
		{`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"met","evidence":"x"},{"index":2,"criterion":"extra","status":"met","evidence":"x"}]}`, "malformed", "none"},
	} {
		r := NewReadiness("failed", []string{"works"}, CaptureAcceptanceEvidence(evidenceBlock(tc.raw)))
		if r.AcceptanceEvidence.State != tc.state || r.ExternalVerification != tc.external || r.RequiredChecks != "failed" {
			t.Fatalf("%+v", r)
		}
	}
	if r := NewReadiness("unavailable", nil, AcceptanceSnapshot{State: "malformed"}); r.AcceptanceEvidence.State != "complete" {
		t.Fatal(r)
	}
}

func TestAcceptanceAssessmentMixed(t *testing.T) {
	criteria := []string{"first", "second", "third"}
	snapshot := CaptureAcceptanceEvidence(evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"first","status":"met","evidence":"observed"},{"index":2,"criterion":"second","status":"unmet"}]}`))
	a := AssessAcceptance(criteria, snapshot)
	if a.State != "incomplete" || len(a.Blockers) != 2 || a.Blockers[0].Index != 2 || a.Blockers[1].Index != 3 {
		t.Fatal(a)
	}
	r := NewReadiness("passed", criteria, snapshot)
	err := r.SubmissionError(440, true)
	if err == nil || !strings.Contains(err.Error(), "2. second (unmet)") || !strings.Contains(err.Error(), "3. third (unmentioned)") || !strings.Contains(err.Error(), "upstream") {
		t.Fatal(err)
	}
	snapshot = CaptureAcceptanceEvidence(evidenceBlock(`{"version":1,"criteria":[{"index":3,"criterion":"third","status":"external","reason":"macOS host"},{"index":1,"criterion":"first","status":"met","evidence":"observed"},{"index":2,"criterion":"second","status":"external","reason":"Windows host"}]}`))
	a = AssessAcceptance(criteria, snapshot)
	if a.State != "complete" || len(a.External) != 2 || a.External[0].Index != 2 || a.External[1].Index != 3 {
		t.Fatal(a)
	}
}

func TestIssue464FindingsAndAction(t *testing.T) {
	body := "## Independent acceptance review 1\nThe first behavior is absent.\n\n## Independent acceptance review 2\nThe second behavior loses data.\n" + evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"first","status":"unmet","evidence":"Independent acceptance review 1"},{"index":2,"criterion":"second","status":"unmet","evidence":"Independent acceptance review 2"}]}`)
	metadata := map[string]string{}
	StoreAcceptanceSnapshot(metadata, CaptureAcceptanceEvidence(body))
	restored := RestoreAcceptanceEvidence("Required checks passed.\n", metadata)
	for _, sentence := range []string{"The first behavior is absent.", "The second behavior loses data."} {
		if !strings.Contains(restored, sentence) {
			t.Errorf("lost finding: %s", sentence)
		}
	}
	if again := RestoreAcceptanceEvidence(restored, metadata); again != restored {
		t.Error("restoration accumulates findings")
	}
	r := NewReadiness("passed", []string{"first", "second"}, AcceptanceSnapshotFromMetadata(metadata))
	if err := r.SubmissionError(464, false); err == nil || !strings.Contains(err.Error(), "--phase implement") {
		t.Errorf("wrong action: %v", err)
	}
}

func TestIssue464RepairAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, entries string
		repair        bool
	}{
		{"unmet", `{"index":1,"criterion":"first","status":"unmet"}`, true},
		{"missing", "", false},
		{"malformed", `{"index":1,"criterion":"first","status":"unmet"},{"index":2,"criterion":"second","status":"oops"}`, false},
		{"duplicate", `{"index":1,"criterion":"first","status":"unmet"},{"index":1,"criterion":"first","status":"unmet"}`, false},
		{"mismatch", `{"index":1,"criterion":"other","status":"unmet"}`, false},
		{"outside", `{"index":3,"criterion":"other","status":"unmet"}`, false},
		{"mixed", `{"index":1,"criterion":"first","status":"unmet"},{"index":2,"criterion":"second","status":"external","reason":"Windows host"}`, true},
		{"external", `{"index":1,"criterion":"first","status":"external","reason":"Windows host"},{"index":2,"criterion":"second","status":"met","evidence":"observed"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReadiness("passed", []string{"first", "second"}, CaptureAcceptanceEvidence(evidenceBlock(`{"version":1,"criteria":[`+tc.entries+`]}`)))
			if r.AcceptanceEvidence.NeedsRepair() != tc.repair {
				t.Fatalf("%+v", r)
			}
			err := r.SubmissionError(464, false)
			if err != nil {
				phase := "test"
				if tc.repair {
					phase = "implement"
				}
				if !strings.Contains(err.Error(), "--phase "+phase) {
					t.Fatal(err)
				}
			}
			if forkErr := r.SubmissionError(464, true); forkErr != nil && !strings.Contains(forkErr.Error(), "--phase test upstream") {
				t.Fatal(forkErr)
			}
		})
	}
}

func TestIssue464BoundedAndLegacyFindings(t *testing.T) {
	block := evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"unmet"}]}`)
	snapshot := CaptureAcceptanceEvidence(strings.Repeat("x", MaxAcceptanceEvidenceBytes+1) + "\n" + block)
	if len(snapshot.Findings) != MaxAcceptanceEvidenceBytes {
		t.Fatal("unbounded findings")
	}
	md := map[string]string{}
	StoreAcceptanceSnapshot(md, snapshot)
	if AcceptanceSnapshotFromMetadata(md).State != "valid" {
		t.Fatal("bounded snapshot unreadable")
	}
	// Old metadata contains the mapping without a prose field.
	md[AcceptanceEvidenceKey] = `{"state":"valid","block":"{\"version\":1,\"criteria\":[]}"}`
	if s := AcceptanceSnapshotFromMetadata(md); s.State != "valid" || s.Findings != "" {
		t.Fatal(s)
	}
	for _, state := range []string{"missing", "malformed"} {
		StoreAcceptanceSnapshot(md, AcceptanceSnapshot{State: state, Findings: "stale finding"})
		if s := AcceptanceSnapshotFromMetadata(md); s.Findings != "" || strings.Contains(RestoreAcceptanceEvidence("", md), "stale finding") {
			t.Fatal(s)
		}
	}
}
