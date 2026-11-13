package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
					action := "swarm issue work " + n
					phase := "test"
					if mode == "unmet" {
						phase = "implement"
					}
					if mode != "missing" {
						action += " --phase " + phase
					} else if strings.Contains(err.Error(), "--phase") {
						t.Fatal(err)
					}
					if !strings.Contains(err.Error(), action+";") || !strings.Contains(err.Error(), "Produce a checked checkpoint") {
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

func TestSubmitGapAcknowledgment(t *testing.T) {
	for _, flags := range [][]string{{"--acknowledge-gap", "1"}, {}, {"--acknowledge-gap", "0"}, {"--acknowledge-gap", "2"}, {"--acknowledge-gap", "1", "--acknowledge-gap", "1"}, {"--acknowledge-gap", "-1"}, {"--acknowledge-gap", "x"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			f := newCLIFixture(t)
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
			st := f.state(t, issue)
			f.manager.BindClaim(issue, st.Lease.ClaimID)
			art := latestValidation(t, f, issue)
			validation.StoreAcceptanceSnapshot(art.Metadata, validation.CaptureAcceptanceEvidence("Independent finding\n"+validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"Produce a checked checkpoint","status":"unmet","evidence":"fixture observation"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
			if _, err := f.manager.RecordTrustedValidation(t.Context(), issue, "alice", art); err != nil {
				t.Fatal(err)
			}
			if len(flags) == 2 && flags[1] == "1" {
				cmd := f.command(&RootOptions{trackerOverride: gapFailedPublication{Tracker: f.tracker}})
				cmd.SetArgs([]string{"--root", f.root, "issue", "submit", n, "--acknowledge-gap", "1"})
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(&bytes.Buffer{})
				if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "injected publication failure") {
					t.Fatal(err)
				}
				if f.state(t, issue).PRNumber != 0 {
					t.Fatal("failed publication completed")
				}
			}
			args := append([]string{"issue", "submit", n}, flags...)
			_, err := f.run(args...)
			success := len(flags) == 2 && flags[1] == "1"
			if success != (err == nil) {
				t.Fatalf("success=%v err=%v", success, err)
			}
			current := f.state(t, issue)
			if !success {
				if current.PRNumber != 0 || current.HeadSHA != st.HeadSHA {
					t.Fatal("rejected gap request published")
				}
				if len(flags) == 0 && !strings.Contains(err.Error(), "--acknowledge-gap") {
					t.Fatal(err)
				}
				return
			}
			iss, _ := f.tracker.GetIssue(t.Context(), issue)
			unit, _ := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
			if gaps := lease.KnownGaps(current, unit); len(gaps) != 1 || gaps[0].Evidence != "fixture observation" {
				t.Fatal(gaps)
			}
			body := draftPRSection(unit, current, "state")
			if !strings.Contains(body, "## Known gaps") || !strings.Contains(body, "fixture observation") {
				t.Fatal(body)
			}
			status := f.must(t, "issue", "status", n, "--json")
			if !strings.Contains(status, `"known_gaps"`) || !strings.Contains(status, "fixture observation") {
				t.Fatal(status)
			}
			f.must(t, "issue", "submit", n)
			f.must(t, "issue", "submit", n, "--acknowledge-gap", "1")
			// The actual authorized record must expire independently for each identity.
			for _, change := range []string{"head", "task", "claim", "findings", "block"} {
				clone := *current
				clone.Artifacts = append([]contract.Artifact(nil), current.Artifacts...)
				switch change {
				case "head":
					clone.HeadSHA = "other"
				case "task":
					clone.TaskDigest = "other"
				case "claim":
					clone.Lease.ClaimID = "other"
				default:
					last := len(clone.Artifacts) - 1
					md := map[string]string{}
					for k, v := range clone.Artifacts[last].Metadata {
						md[k] = v
					}
					clone.Artifacts[last].Metadata = md
					snapshot := validation.AcceptanceSnapshotFromMetadata(md)
					if change == "findings" {
						snapshot.Findings += "changed"
					} else {
						snapshot.Block = strings.ReplaceAll(snapshot.Block, "fixture observation", "changed")
					}
					validation.StoreAcceptanceSnapshot(md, snapshot)
				}
				if len(lease.KnownGaps(&clone, unit)) != 0 {
					t.Fatalf("ack survives %s", change)
				}
			}
		})
	}
}

type gapFailedPublication struct{ tracker.Tracker }

func (g gapFailedPublication) CreateDraftPR(context.Context, tracker.DraftPRRequest) (*tracker.PullRequest, error) {
	return nil, fmt.Errorf("injected publication failure")
}

func TestSubmitGapAcknowledgmentUnauthorized(t *testing.T) {
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
	before := f.state(t, issue)
	for _, mode := range []string{"denied", "blank", "fork"} {
		t.Run(mode, func(t *testing.T) {
			cmd := f.command(&RootOptions{trackerOverride: gapDeniedTracker{Tracker: f.tracker, blank: mode == "blank"}})
			args := []string{"--root", f.root, "issue", "submit", n, "--acknowledge-gap", "1"}
			if mode == "fork" {
				args = append([]string{"--fork"}, args...)
			}
			cmd.SetArgs(args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err == nil {
				t.Fatal("unauthorized acknowledgment")
			}
			after := f.state(t, issue)
			if after.PRNumber != 0 || after.HeadSHA != before.HeadSHA || after.Lease.Status != before.Lease.Status {
				t.Fatal("rejection published")
			}
		})
	}
}

type gapDeniedTracker struct {
	tracker.Tracker
	blank bool
}

func (g gapDeniedTracker) CurrentUser(ctx context.Context) (string, error) {
	if g.blank {
		return "", nil
	}
	return g.Tracker.CurrentUser(ctx)
}
func (g gapDeniedTracker) Authorize(context.Context, string) error {
	return fmt.Errorf("denied approve-task")
}

// Exercise admission through the real fork transport and authoritative ledger.
func TestIssueSubmitForkTrustedEvidenceAdmission(t *testing.T) {
	t.Run("direct-missing", func(t *testing.T) {
		f := newCLIFixture(t)
		issue := f.create(t, "agent")
		n := strconv.Itoa(issue)
		f.must(t, "issue", "approve", n)
		f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
		out, err := f.run("issue", "submit", n, "--progress=false")
		st := f.state(t, issue)
		if err == nil || !strings.Contains(err.Error(), "acceptance evidence blocks") || st.PRNumber != 0 || st.SubmittedHeadSHA != "" || st.Lease.Status == "completed" {
			t.Fatal(err, out)
		}
	})

	for _, mode := range []string{"trusted", "missing", "imported", "head_sha", "base_sha", "validation_claim", "validation_tree", "validation_policy", "validation_trusted_owner", "validation_trusted_claim", "newer-missing", "newer-malformed", "candidate-changed", "interleaved"} {
		t.Run(mode, func(t *testing.T) {
			f := newUpstreamRemoteFixture(t)
			alice := f.aliceClone("alice")
			opts := f.as("alice", alice)
			run := func(args ...string) (string, error) {
				return f.run(opts, append([]string{"--upstream-remote", "upstream", "--fork"}, args...)...)
			}
			if out, err := run("issue", "work", "42", "--agent", "mock", "--headless"); err == nil || !strings.Contains(err.Error(), "approval") {
				t.Fatal(err, out)
			}
			if out, err := f.run(RootOptions{RepoRoot: f.upstreamRoot, trackerOverride: f.maintainer}, "issue", "review-plan", "42", "--approve"); err != nil {
				t.Fatal(err, out)
			}
			if out, err := run("issue", "work", "42", "--agent", "mock", "--headless", "--phase", "implement"); err != nil {
				t.Fatal(err, out)
			}
			if mode != "missing" {
				seedGitIssueAcceptance(t, alice, f.upstreamRoot, f.maintainer, 42, f.now)
			}
			store := trustedLedgerStore(t, f.upstreamRoot, "")
			raw, rev, err := store.Load(t.Context(), 42)
			if err != nil {
				t.Fatal(err)
			}
			var st lease.IssueState
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatal(err)
			}
			for i := len(st.Artifacts) - 1; i >= 0; i-- {
				a := &st.Artifacts[i]
				if a.Type != contract.ArtifactValidation {
					continue
				}
				switch mode {
				case "trusted", "candidate-changed", "interleaved":
					dst := contract.Artifact{Metadata: map[string]string{}}
					for k, v := range a.Metadata {
						if k != validation.AcceptanceEvidenceKey {
							dst.Metadata[k] = v
						}
					}
					lease.PreserveAcceptanceEvidence(&st, &dst)
					if validation.AcceptanceSnapshotFromMetadata(dst.Metadata).State != "valid" {
						t.Fatalf("seed ineligible: %+v", dst.Metadata)
					}
				case "imported":
					for k := range a.Metadata {
						if strings.HasPrefix(k, "validation_trusted_") {
							delete(a.Metadata, k)
						}
					}
				case "newer-missing", "newer-malformed":
					// Preserve the older met report to detect accidental history searches.
					copyArt := *a
					copyArt.Metadata = map[string]string{}
					for k, v := range a.Metadata {
						copyArt.Metadata[k] = v
					}
					copyArt.Version++
					delete(copyArt.Metadata, validation.AcceptanceEvidenceKey)
					if mode == "newer-malformed" {
						copyArt.Metadata[validation.AcceptanceEvidenceKey] = "invalid"
					}
					st.Artifacts = append(st.Artifacts, copyArt)
				case "missing":
				default:
					a.Metadata[mode] = "different"
				}
				break
			}
			raw, err = json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.CompareAndSwap(t.Context(), 42, rev, raw); err != nil {
				t.Fatal(err)
			}
			if mode == "candidate-changed" {
				if err := os.WriteFile(filepath.Join(workspace.WorktreePath(alice, 42), "changed.txt"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var out string
			if mode == "interleaved" {
				opts.forkPollInterval = 5 * time.Millisecond
				opts.forkPollTimeout = 30 * time.Second
				opts.nowFn = func() time.Time { return f.now }
				opts.lookupEnv = f.env.lookup()
				inserted := false
				opts.forkDispatcher = func(ctx context.Context, issue int, c *tracker.Comment) error {
					if !inserted && strings.Contains(c.Body, "update-artifact") {
						inserted = true
						raw, rev, e := store.Load(ctx, 42)
						if e != nil {
							return e
						}
						var concurrent lease.IssueState
						if e = json.Unmarshal(raw, &concurrent); e != nil {
							return e
						}
						for i := len(concurrent.Artifacts) - 1; i >= 0; i-- {
							a := &concurrent.Artifacts[i]
							if a.Type != contract.ArtifactValidation {
								continue
							}
							a.Version++
							delete(a.Metadata, validation.AcceptanceEvidenceKey)
							break
						}
						raw, e = json.Marshal(concurrent)
						if e != nil {
							return e
						}
						if _, e = store.CompareAndSwap(ctx, 42, rev, raw); e != nil {
							return e
						}
					}
					return f.dispatch(ctx, issue, c)
				}
				cmd := newRootCmdWithOptions(&opts)
				var output bytes.Buffer
				cmd.SetOut(&output)
				cmd.SetErr(&output)
				cmd.SetArgs([]string{"--upstream-remote", "upstream", "--fork", "issue", "submit", "42", "--progress=false"})
				err = cmd.ExecuteContext(t.Context())
				out = output.String()
				if !inserted {
					t.Fatal("interleaved validation was not inserted", err, out)
				}
			} else {
				out, err = run("issue", "submit", "42", "--progress=false")
			}
			final := f.state()
			f.mu.Lock()
			prs := len(f.prs)
			f.mu.Unlock()
			if mode == "trusted" {
				if err != nil || prs != 1 || final.PRNumber == 0 || final.Lease.Status != "completed" || final.SubmittedHeadSHA == "" || final.SubmittedHeadSHA != final.HeadSHA {
					t.Fatal(err, out, final)
				}
			} else if err == nil || prs != 0 || final.PRNumber != 0 || final.SubmittedHeadSHA != "" || final.Lease.Status == "completed" {
				t.Fatal("unexpected publication", err, out, final)
			}
			// Recording replaces the first validation slot, including when the
			// fixture retains a later invalid report to test history refusal.
			for i := 0; i < len(final.Artifacts); i++ {
				a := final.Artifacts[i]
				if a.Type != contract.ArtifactValidation {
					continue
				}
				for k := range a.Metadata {
					if strings.HasPrefix(k, "validation_trusted_") {
						t.Fatalf("worker report gained authority: %s", k)
					}
				}
				break
			}
			finalRaw, _, loadErr := store.Load(t.Context(), 42)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if strings.Contains(string(finalRaw), "gap_acknowledgment") {
				t.Fatal("gap acknowledgment introduced")
			}
		})
	}
}

// The CLI constructs gateways only with --fork; exercise the independent
// HasWorkerGateway admission branch directly with real fenced artifact writes.
type submitEvidenceGateway struct{ coordinator *lease.Manager }

func (g submitEvidenceGateway) Apply(ctx context.Context, req lease.ForkRequest) (*lease.IssueState, error) {
	if req.Operation != "update-artifact" || req.Artifact == nil {
		return nil, fmt.Errorf("unexpected gateway operation %s", req.Operation)
	}
	return g.coordinator.UpdateArtifact(ctx, req.IssueNumber, "alice", *req.Artifact)
}

func TestIssueSubmitForkTrustedEvidenceAdmissionGateway(t *testing.T) {
	for _, mode := range []string{"trusted", "missing", "imported", "failed", "owner-changed", "claim-changed", "task-changed", "policy-changed", "base-changed", "head-changed", "interleaved-before", "interleaved-after", "different-report", "identity-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
			if mode != "missing" {
				seedCompleteIssueAcceptance(t, f, issue)
			}
			before := f.state(t, issue)
			iss, err := f.tracker.GetIssue(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			unit, err := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
			if err != nil {
				t.Fatal(err)
			}
			latest := latestSubmitValidation(before)
			if latest == nil {
				t.Fatal("missing fixture validation")
			}
			art := *latest
			art.Metadata = map[string]string{}
			for k, v := range latest.Metadata {
				art.Metadata[k] = v
			}
			if mode == "imported" {
				for k := range before.Artifacts {
					for key := range before.Artifacts[k].Metadata {
						if strings.HasPrefix(key, "validation_trusted_") {
							delete(before.Artifacts[k].Metadata, key)
						}
					}
				}
			}
			report := &validation.ValidationReport{Passed: mode != "failed", HeadSHA: before.HeadSHA, BaseSHA: before.BaseSHA, Artifact: art}
			if mode == "failed" {
				report.Artifact.Verdict = "FAIL"
			}
			identity := map[string]string{}
			for k, v := range art.Metadata {
				identity[k] = v
			}
			if mode == "identity-failure" {
				delete(identity, "validation_tree")
			}
			preserved := captureSubmitAcceptance(before, identity, report)
			if mode == "trusted" && preserved == nil {
				t.Fatal("trusted fixture not eligible")
			}
			if (mode == "missing" || mode == "imported" || mode == "identity-failure") && preserved != nil {
				t.Fatal("ineligible snapshot captured")
			}
			cfg, err := harness.LoadConfig(f.root)
			if err != nil {
				t.Fatal(err)
			}
			f.manager.BindClaim(issue, before.Lease.ClaimID)
			worker := lease.NewManager(f.tracker, cfg.Security)
			worker.SetStore(f.root, f.manager.Store())
			worker.BindClaim(issue, before.Lease.ClaimID)
			worker.SetWorkerGateway(submitEvidenceGateway{f.manager})
			if mode == "interleaved-before" {
				if _, err := f.manager.UpdateArtifact(t.Context(), issue, "alice", art); err != nil {
					t.Fatal(err)
				}
			}
			after, err := worker.RecordTrustedValidation(t.Context(), issue, "alice", report.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "interleaved-after" {
				after, err = f.manager.UpdateArtifact(t.Context(), issue, "alice", art)
				if err != nil {
					t.Fatal(err)
				}
			}
			for k := range latestSubmitValidation(after).Metadata {
				if strings.HasPrefix(k, "validation_trusted_") {
					t.Fatal("worker report gained trust", k)
				}
			}
			switch mode {
			case "owner-changed":
				after.Lease.Owner = "other"
			case "claim-changed":
				after.Lease.ClaimID = "other"
			case "task-changed":
				after.TaskDigest = "other"
			case "policy-changed":
				after.PolicyDigest = "other"
			case "base-changed":
				after.BaseSHA = "other"
			case "head-changed":
				after.HeadSHA = "other"
			case "different-report":
				latestSubmitValidation(after).Body = "other"
			}
			err = issueGapAdmission(t.Context(), worker, issue, "alice", before.HeadSHA, unit, after, nil, false, false, preserved)
			if (err == nil) != (mode == "trusted") {
				t.Fatalf("admission %s: %v", mode, err)
			}
			final := f.state(t, issue)
			if final.PRNumber != 0 || final.SubmittedHeadSHA != "" || final.Lease.Status == "completed" {
				t.Fatal("admission published a PR")
			}
		})
	}
}
