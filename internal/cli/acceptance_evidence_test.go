package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
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
	"time"
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
				args := []string{"--root", f.root, "pr", "submit", p}
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
	// Preserve legacy opt-in calls made after plan-only or no-op attempts.
	for _, art := range f.state(t, issue).Artifacts {
		if art.Type == contract.ArtifactValidation {
			seedIssueAcceptance(t, f.root, f.tracker, f.manager, issue, "alice", "")
			return
		}
	}
}

// seedIssueAcceptance also supports fixtures whose worker checkout and
// authoritative tracker live in separate repositories.
func seedIssueAcceptance(t *testing.T, root string, tr tracker.Tracker, manager *lease.Manager, issue int, owner, externalReason string) {
	t.Helper()
	st, err := manager.GetCurrentState(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	var art contract.Artifact
	found := false
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactValidation {
			found = true
			art = a
		}
	}
	if !found {
		t.Fatal("acceptance fixture requires an existing validation artifact")
	}
	manager.BindClaim(issue, st.Lease.ClaimID)
	identity, identityErr := validation.AcceptanceCandidateIdentity(t.Context(), workspace.WorktreePath(root, issue), validation.ValidateOptions{BaseSHA: st.BaseSHA, ClaimID: st.Lease.ClaimID, Version: Version})
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	for k, v := range identity {
		if art.Metadata[k] == "" {
			art.Metadata[k] = v
		}
	}
	iss, err := tr.GetIssue(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	block, err := fixtureAcceptanceBlock(wu.Acceptance, externalReason)
	if err != nil {
		t.Fatal(err)
	}
	validation.StoreAcceptanceSnapshot(art.Metadata, validation.CaptureAcceptanceEvidence(block))
	if _, err := manager.RecordTrustedValidation(t.Context(), issue, owner, art); err != nil {
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
				args := []string{"--root", f.root, "issue", "submit", n}
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
					phase := "test"
					if mode == "unmet" {
						phase = "implement"
					}
					if !strings.Contains(err.Error(), "swarm issue work "+n+" --phase "+phase) || !strings.Contains(err.Error(), "Produce a checked checkpoint") {
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

// Completed receipts are authoritative; imported validation remains advisory.
func TestSubmitForkCompletedRetry(t *testing.T) {
	for _, mode := range []string{"valid", "owner", "claim", "pr", "completion", "submitted-head", "missing-head", "dirty", "changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
			seedCompleteIssueAcceptance(t, f, issue)
			f.must(t, "issue", "submit", n)
			editForkRetryState(t, f, issue, func(st map[string]any) {
				switch mode {
				case "owner":
					st["lease"].(map[string]any)["owner"] = "mallory"
				case "claim":
					st["lease"].(map[string]any)["claim_id"] = "other"
				case "pr":
					st["pr_number"] = 0
				case "completion":
					st["lease"].(map[string]any)["status"] = "active"
				case "submitted-head":
					st["submitted_head_sha"] = "other"
				case "missing-head":
					delete(st, "submitted_head_sha")
				}
			})
			wt := workspace.WorktreePath(f.root, issue)
			if mode == "dirty" || mode == "changed" {
				if err := os.WriteFile(filepath.Join(wt, "retry-change.txt"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
				if mode == "changed" {
					prGit(t, wt, "add", "retry-change.txt")
					prGit(t, wt, "commit", "-m", "changed")
				}
			}
			before, rev, err := f.manager.Store().Load(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			out, err := f.run("issue", "submit", n, "--json", "--progress=false")
			if mode == "valid" {
				st := f.state(t, issue)
				if err != nil || !strings.Contains(out, `"pr_number":`+strconv.Itoa(st.PRNumber)) || !strings.Contains(out, st.HeadSHA) || !strings.Contains(out, `"required_checks":"unavailable"`) {
					t.Fatal(err, out)
				}
			} else if err == nil {
				t.Fatal("accepted invalid retry", out)
			}
			after, afterRev, loadErr := f.manager.Store().Load(t.Context(), issue)
			if mode == "completion" {
				st := f.state(t, issue)
				if st.Lease.Status == "completed" {
					t.Fatal("incomplete receipt authorized submission")
				}
			} else if loadErr != nil || !bytes.Equal(before, after) || rev != afterRev {
				t.Fatal("retry mutated receipt", loadErr)
			}
		})
	}
}

func editForkRetryState(t *testing.T, f *cliFixture, issue int, edit func(map[string]any)) {
	t.Helper()
	raw, rev, err := f.manager.Store().Load(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	st["fork_repo"] = "alice/swarm"
	for _, v := range st["artifacts"].([]any) {
		a := v.(map[string]any)
		if a["type"] == "validation" {
			m := a["metadata"].(map[string]any)
			for k := range m {
				if strings.HasPrefix(k, "validation_trusted_") {
					delete(m, k)
				}
			}
		}
	}
	edit(st)
	raw, err = json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Store().CompareAndSwap(t.Context(), issue, rev, raw); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitForkUntrustedEvidenceRefused(t *testing.T) {
	for _, mode := range []string{"missing", "imported"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
			if mode == "imported" {
				seedCompleteIssueAcceptance(t, f, issue)
			}
			editForkRetryState(t, f, issue, func(map[string]any) {})
			out, err := f.run("issue", "submit", n, "--json", "--progress=false")
			st := f.state(t, issue)
			if err == nil || !strings.Contains(err.Error(), "acceptance evidence blocks") || st.PRNumber != 0 || st.SubmittedHeadSHA != "" || st.Lease.Status == "completed" {
				t.Fatal(err, out, st)
			}
		})
	}
}

func fixtureAcceptanceBlock(criteria []string, externalReason string) (string, error) {
	entries := make([]map[string]any, 0, len(criteria))
	for i, c := range criteria {
		entry := map[string]any{"index": i + 1, "criterion": c, "status": "met", "evidence": "fixture observation"}
		if externalReason != "" {
			entry = map[string]any{"index": i + 1, "criterion": c, "status": "external", "reason": externalReason}
		}
		entries = append(entries, entry)
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "criteria": entries})
	return validation.AcceptanceEvidenceStart + "\n" + string(raw) + "\n" + validation.AcceptanceEvidenceEnd, err
}

// acceptanceFixtureRunner supplies evidence during a single auto-submit run,
// where there is no command boundary between testing and publication.
type acceptanceFixtureRunner struct{ agent.Runner }

func (r acceptanceFixtureRunner) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	result, err := r.Runner.Run(ctx, req)
	if err != nil || req.Role != "tester" {
		return result, err
	}
	wu, err := workspace.LoadActiveWorkUnit(req.WorktreePath)
	if err != nil {
		return nil, err
	}
	block, err := fixtureAcceptanceBlock(wu.Acceptance, "")
	if err != nil {
		return nil, err
	}
	err = os.WriteFile(filepath.Join(req.WorktreePath, ".swarm/active/VALIDATION.md"), []byte("# Fixture validation\n\n"+block+"\n"), 0644)
	return result, err
}

func seedGitIssueAcceptance(t *testing.T, workerRoot, authorityRoot string, tr tracker.Tracker, issue int, now time.Time) {
	t.Helper()
	cfg, err := harness.LoadConfig(authorityRoot)
	if err != nil {
		t.Fatal(err)
	}
	manager := lease.NewManager(tr, cfg.Security)
	manager.SetStore(authorityRoot, trustedLedgerStore(t, authorityRoot, ""))
	manager.SetNowFunc(func() time.Time { return now })
	st, err := manager.GetCurrentState(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	seedIssueAcceptance(t, workerRoot, tr, manager, issue, st.Lease.Owner, "")
}
