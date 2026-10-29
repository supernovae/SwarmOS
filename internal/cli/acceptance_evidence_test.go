package cli

import (
	"bytes"
	"encoding/json"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/validation"
	"strconv"
	"strings"
	"testing"
)

func TestIssueSubmitAcceptanceEvidenceAdvisory(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(strconv.FormatBool(asJSON), func(t *testing.T) {
			f := newCLIFixture(t)
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
			for retry := 0; retry < 2; retry++ {
				cmd := f.command(nil)
				var stdout, stderr bytes.Buffer
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				args := []string{"--root", f.root, "--backend", "local", "issue", "submit", n}
				if asJSON {
					args = append(args, "--json")
				}
				cmd.SetArgs(args)
				if err := cmd.Execute(); err != nil {
					t.Fatal(err, stdout.String(), stderr.String())
				}
				if st := f.state(t, issue); st.PRNumber == 0 || st.Lease.Status != "completed" {
					t.Fatal("warning blocked publication")
				}
				notice := stdout.String()
				if asJSON {
					var got struct {
						Warnings []validation.AcceptanceWarning `json:"acceptance_evidence_warnings"`
						Status   string                         `json:"status"`
					}
					if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
						t.Fatal(err, stdout.String())
					}
					if got.Status != "submitted" || len(got.Warnings) != 1 || got.Warnings[0].Reason != "unmentioned" {
						t.Fatal(stdout.String())
					}
					notice = stderr.String()
				}
				if !strings.Contains(notice, "advisory") || !strings.Contains(notice, "Produce a checked checkpoint") {
					t.Fatal(notice)
				}
			}
		})
	}
}

func TestIssueSubmitMappedAcceptanceEvidence(t *testing.T) {
	for _, mode := range []string{"met", "unmet", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
			state := f.state(t, issue)
			f.manager.BindClaim(issue, state.Lease.ClaimID)
			// Deliver a bounded tester snapshot through the coordinator recording API.
			var art contract.Artifact
			for _, a := range state.Artifacts {
				if a.Type == contract.ArtifactValidation {
					art = a
				}
			}
			body := validation.AcceptanceEvidenceStart + "\n" + `{"version":1,"criteria":[{"index":1,"criterion":"Produce a checked checkpoint","status":"` + mode + `","evidence":"VALIDATION.md: observation"}]}` + "\n" + validation.AcceptanceEvidenceEnd
			if mode == "malformed" {
				body = validation.AcceptanceEvidenceStart
			}
			validation.StoreAcceptanceSnapshot(art.Metadata, validation.CaptureAcceptanceEvidence(body))
			art.Body += "\n" + body
			if _, err := f.manager.RecordTrustedValidation(t.Context(), issue, "alice", art); err != nil {
				t.Fatal(err)
			}
			// Fresh validate and staging regenerate checks while retaining eligible mapping.
			f.must(t, "issue", "validate", n, "--json")
			f.must(t, "issue", "stage", n, "--phase", "implement", "--harness", "codex", "--model", "default")
			for retry := 0; retry < 2; retry++ {
				out := f.must(t, "issue", "submit", n, "--json")
				var got map[string]json.RawMessage
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatal(err, out)
				}
				warnings, exists := got["acceptance_evidence_warnings"]
				if exists != (mode != "met") {
					t.Fatal(out)
				}
				if exists && !strings.Contains(string(warnings), `"reason": "`+mode+`"`) && !strings.Contains(string(warnings), `"reason":"`+mode+`"`) {
					t.Fatal(out)
				}
				if f.state(t, issue).PRNumber == 0 {
					t.Fatal("did not publish")
				}
			}
		})
	}
}

func TestPRSubmitAcceptanceEvidenceOutput(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(strconv.FormatBool(asJSON), func(t *testing.T) {
			f, _, number := submittedPRFixture(t, "agent")
			p := strconv.Itoa(number)
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'update\\n' > evidence-update.txt")
			f.must(t, "pr", "work", p, "--agent", "mock", "--json")
			for retry := 0; retry < 2; retry++ {
				cmd := f.command(nil)
				var out, diagnostics bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&diagnostics)
				args := []string{"--root", f.root, "--backend", "local", "pr", "submit", p}
				if asJSON {
					args = append(args, "--json")
				}
				cmd.SetArgs(args)
				if err := cmd.Execute(); err != nil {
					t.Fatal(err, out.String(), diagnostics.String())
				}
				notice := out.String()
				if asJSON {
					r := readPROutcome(t, out.String())
					if len(r.AcceptanceEvidenceWarnings) != 1 {
						t.Fatal(out.String())
					}
					notice = diagnostics.String()
				}
				if !strings.Contains(notice, "advisory") || !strings.Contains(notice, "Produce a checked checkpoint") {
					t.Fatal(notice)
				}
			}
		})
	}
}

func TestIssueSubmitMixedAcceptanceEvidence(t *testing.T) {
	f := newCLIFixture(t)
	f.must(t, "issue", "create", "--title", "Mixed evidence", "--objective", "Report observations", "--constraint", "Advisory only", "--acceptance", "First criterion", "--acceptance", "Second criterion", "--acceptance", "Third criterion", "--gate", "agent")
	const issue = 1
	f.must(t, "issue", "approve", "1")
	f.must(t, "issue", "work", "1", "--agent", "mock", "--headless")
	st := f.state(t, issue)
	f.manager.BindClaim(issue, st.Lease.ClaimID)
	art := latestValidation(t, f, issue)
	body := validation.AcceptanceEvidenceStart + "\n" + `{"version":1,"criteria":[{"index":1,"criterion":"First criterion","status":"met","evidence":"VALIDATION.md: observation"},{"index":2,"criterion":"Second criterion","status":"unmet"}]}` + "\n" + validation.AcceptanceEvidenceEnd
	validation.StoreAcceptanceSnapshot(art.Metadata, validation.CaptureAcceptanceEvidence(body))
	if _, err := f.manager.RecordTrustedValidation(t.Context(), issue, "alice", art); err != nil {
		t.Fatal(err)
	}
	out := f.must(t, "issue", "submit", "1", "--json")
	var got struct {
		Warnings []validation.AcceptanceWarning `json:"acceptance_evidence_warnings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err, out)
	}
	if len(got.Warnings) != 2 || got.Warnings[0].Index != 2 || got.Warnings[0].Criterion != "Second criterion" || got.Warnings[0].Reason != "unmet" || got.Warnings[1].Index != 3 || got.Warnings[1].Reason != "unmentioned" {
		t.Fatal(out)
	}
	if f.state(t, issue).PRNumber == 0 {
		t.Fatal("did not publish")
	}
}
