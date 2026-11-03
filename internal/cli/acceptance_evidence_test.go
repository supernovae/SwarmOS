package cli

import (
	"bytes"
	"encoding/json"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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

// seedCompleteIssueAcceptance explicitly opts successful lifecycle fixtures into
// eligible evidence; generic mock reports remain missing by default.
func seedCompleteIssueAcceptance(t *testing.T, f *cliFixture, issue int) {
	t.Helper()
	st := f.state(t, issue)
	found := false
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactValidation {
			found = true
		}
	}
	if !found {
		return
	}
	f.manager.BindClaim(issue, st.Lease.ClaimID)
	art := latestValidation(t, f, issue)
	identity, identityErr := validation.AcceptanceCandidateIdentity(t.Context(), workspace.WorktreePath(f.root, issue), validation.ValidateOptions{BaseSHA: st.BaseSHA, ClaimID: st.Lease.ClaimID, Version: Version})
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	for k, v := range identity {
		if art.Metadata[k] == "" {
			art.Metadata[k] = v
		}
	}
	iss, err := f.tracker.GetIssue(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]map[string]any, 0, len(wu.Acceptance))
	for i, c := range wu.Acceptance {
		entries = append(entries, map[string]any{"index": i + 1, "criterion": c, "status": "met", "evidence": "fixture observation"})
	}
	raw, _ := json.Marshal(map[string]any{"version": 1, "criteria": entries})
	validation.StoreAcceptanceSnapshot(art.Metadata, validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart+"\n"+string(raw)+"\n"+validation.AcceptanceEvidenceEnd))
	if _, err := f.manager.RecordTrustedValidation(t.Context(), issue, "alice", art); err != nil {
		t.Fatal(err)
	}
}

func TestIssueSubmitAcceptanceRefusal(t *testing.T) {
	for _, mode := range []string{"missing", "malformed", "omitted", "unmet", "duplicate", "mismatch", "outside", "blank-external", "met", "external"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			remote := filepath.Join(t.TempDir(), "origin.git")
			prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
			prGit(t, f.root, "remote", "add", "origin", remote)
			prGit(t, f.root, "push", "origin", "main")
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
			st := f.state(t, issue)
			f.manager.BindClaim(issue, st.Lease.ClaimID)
			art := latestValidation(t, f, issue)
			entry := `{"index":1,"criterion":"Produce a checked checkpoint","status":"` + mode + `","evidence":"observation","reason":"Windows host unavailable"}`
			switch mode {
			case "duplicate":
				entry = `{"index":1,"criterion":"Produce a checked checkpoint","status":"met","evidence":"x"},` + entry
			case "mismatch":
				entry = `{"index":1,"criterion":"wrong","status":"met","evidence":"x"}`
			case "outside":
				entry = `{"index":2,"criterion":"Produce a checked checkpoint","status":"met","evidence":"x"}`
			case "blank-external":
				entry = `{"index":1,"criterion":"Produce a checked checkpoint","status":"external","reason":" "}`
			case "omitted":
				entry = ""
			}
			body := validation.AcceptanceEvidenceStart + "\n" + `{"version":1,"criteria":[` + entry + `]}` + "\n" + validation.AcceptanceEvidenceEnd
			if mode == "missing" {
				body = ""
			}
			if mode == "malformed" {
				body = validation.AcceptanceEvidenceStart
			}
			validation.StoreAcceptanceSnapshot(art.Metadata, validation.CaptureAcceptanceEvidence(body))
			if _, err := f.manager.RecordTrustedValidation(t.Context(), issue, "alice", art); err != nil {
				t.Fatal(err)
			}
			var receiptBefore []byte
			if err := f.tracker.WithResumeReceipt(issue, func(r *tracker.ResumeReceipt, _ *tracker.Issue, _ func() error) error {
				receiptBefore, _ = json.Marshal(r)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			publishedBefore := prGit(t, f.root, "ls-remote", "origin", "refs/heads/"+st.Branch)
			calls := 0
			client, clientErr := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "fixture", Transport: descriptionRoundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				rec := httptest.NewRecorder()
				rec.WriteHeader(201)
				_, _ = rec.Write([]byte(`{"number":1,"html_url":"https://github.com/o/r/pull/1"}`))
				return rec.Result(), nil
			})})
			if clientErr != nil {
				t.Fatal(clientErr)
			}
			strict := &strictDescriptionTracker{Tracker: f.tracker, github: tracker.NewGitHubTrackerWithClient("o", "r", client)}
			for _, asJSON := range []bool{false, true} {
				// Exercise concrete local resume receipts separately: nonlocal tracker
				// wrappers intentionally revoke local resume authority on any save.
				var opts *RootOptions
				if asJSON {
					opts = &RootOptions{trackerOverride: strict}
				}
				cmd := f.command(opts)
				var out, diag bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&diag)
				args := []string{"--root", f.root, "--backend", "local", "issue", "submit", n}
				if asJSON {
					args = append(args, "--json")
				}
				cmd.SetArgs(args)
				err := cmd.Execute()
				success := mode == "met" || mode == "external"
				if success != (err == nil) {
					t.Fatal(err, out.String(), diag.String())
				}
				current := f.state(t, issue)
				if !success {
					var receiptAfter []byte
					if err := f.tracker.WithResumeReceipt(issue, func(r *tracker.ResumeReceipt, _ *tracker.Issue, _ func() error) error {
						receiptAfter, _ = json.Marshal(r)
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if !asJSON && !bytes.Equal(receiptBefore, receiptAfter) {
						t.Fatal("publication attempt revoked resume receipt")
					}
					if calls != 0 || prGit(t, f.root, "ls-remote", "origin", "refs/heads/"+st.Branch) != publishedBefore {
						t.Fatal("publication boundary crossed")
					}
					if current.PRNumber != 0 || current.Lease.Status == "completed" || current.HeadSHA != st.HeadSHA {
						t.Fatal("published rejected evidence")
					}
					if !strings.Contains(err.Error(), "swarm issue work "+n+" --phase test") || !strings.Contains(err.Error(), "Produce a checked checkpoint") {
						t.Fatal(err)
					}
				}
				combined := out.String() + diag.String()
				if !strings.Contains(combined, "required_checks") && !strings.Contains(combined, "Required checks:") {
					t.Fatal(combined)
				}
			}
		})
	}
}

func mustIssueNumber(s string) int { n, _ := strconv.Atoi(s); return n }

func TestSubmitAcceptanceFailedChecks(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(strconv.FormatBool(external), func(t *testing.T) {
			f, counter := countedValidationFixture(t)
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
			seedCompleteIssueAcceptance(t, f, issue)
			if external {
				st := f.state(t, issue)
				f.manager.BindClaim(issue, st.Lease.ClaimID)
				art := latestValidation(t, f, issue)
				validation.StoreAcceptanceSnapshot(art.Metadata, validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"Produce a checked checkpoint","status":"external","reason":"Windows host"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
				if _, err := f.manager.RecordTrustedValidation(t.Context(), issue, "alice", art); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(counter+".fail", []byte("fail"), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := f.run("issue", "submit", n, "--json", "--progress=false")
			if err == nil || !strings.Contains(out, `"required_checks":"failed"`) || !strings.Contains(out, `"state":"complete"`) || f.state(t, issue).PRNumber != 0 {
				t.Fatal(err, out)
			}
		})
	}
}

func TestStatusAcceptanceReadiness(t *testing.T) {
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	out := f.must(t, "issue", "status", n, "--json")
	if !strings.Contains(out, `"required_checks":"unavailable"`) || !strings.Contains(out, `"state":"missing"`) {
		t.Fatal(out)
	}
	f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
	seedCompleteIssueAcceptance(t, f, issue)
	out = f.must(t, "issue", "status", n)
	if !strings.Contains(out, "Required checks: passed\nAcceptance evidence: complete\nExternal verification: none") {
		t.Fatal(out)
	}
}

func TestSubmitAcceptanceCompletedRetryRefusesGaps(t *testing.T) {
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
	seedCompleteIssueAcceptance(t, f, issue)
	f.must(t, "issue", "submit", n)
	// Model a completed historical submission whose newest recorded mapping is
	// missing. Preserve the private state fields while replacing only that block.
	raw, rev, err := f.manager.Store().Load(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	artifacts := state["artifacts"].([]any)
	for _, v := range artifacts {
		a := v.(map[string]any)
		if a["type"] == "validation" {
			a["metadata"].(map[string]any)[validation.AcceptanceEvidenceKey] = `{"state":"missing"}`
		}
	}
	raw, _ = json.Marshal(state)
	if _, err := f.manager.Store().CompareAndSwap(t.Context(), issue, rev, raw); err != nil {
		t.Fatal(err)
	}
	if err := f.tracker.RemoveLabel(t.Context(), issue, "swarm:pr-open"); err != nil {
		t.Fatal(err)
	}
	before, err := f.tracker.GetIssue(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.run("issue", "submit", n, "--json", "--progress=false")
	if err == nil || !strings.Contains(err.Error(), "acceptance evidence blocks") || !strings.Contains(out, `"required_checks":"passed"`) {
		t.Fatal(err, out)
	}
	after, err := f.tracker.GetIssue(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	beforeLabels, _ := json.Marshal(before.Labels)
	afterLabels, _ := json.Marshal(after.Labels)
	if !bytes.Equal(beforeLabels, afterLabels) {
		t.Fatal("rejected retry projected submitted labels")
	}
}
