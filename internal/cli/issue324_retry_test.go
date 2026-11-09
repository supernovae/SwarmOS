package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIssue324NoopRetry(t *testing.T) {
	f := newCLIFixture(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
	prGit(t, f.root, "remote", "add", "origin", remote)
	prGit(t, f.root, "push", "origin", "main")
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", ":")
	out, err := f.run("issue", "work", n, "--agent", "mock", "--progress=false")
	if err == nil || !strings.Contains(err.Error(), "developer produced no implementation beyond starting checkpoint") {
		t.Fatalf("expected real developer no-op failure: %v\n%s", err, out)
	}
	st := f.state(t, number)
	wt := workspace.WorktreePath(f.root, number)
	if st.HeadSHA != st.BaseSHA || prGit(t, wt, "rev-parse", "HEAD") != st.BaseSHA || prGit(t, wt, "branch", "--show-current") != st.Branch {
		t.Fatalf("no-op checkpoint identity: %+v", st)
	}
	if ref := prGit(t, f.root, "ls-remote", "--refs", "origin", "refs/heads/"+st.Branch); ref != "" {
		t.Fatalf("published: %s", ref)
	}
	for _, a := range st.Artifacts {
		if (a.Type == contract.ArtifactImplementation || a.Type == contract.ArtifactValidation) && a.Verdict == "PASS" {
			t.Fatalf("no-op granted completion: %+v", a)
		}
	}
	// Status and dry-run planning must leave authority, tokens, refs and index alone.
	snapshot := func() string {
		t.Helper()
		var b strings.Builder
		for _, name := range []string{filepath.Join(".swarm", "local-tracker", "state.json"), filepath.Join(".swarm", "coordination", "issue-"+n+".json")} {
			data, err := os.ReadFile(filepath.Join(f.root, name))
			if err != nil {
				t.Fatal(err)
			}
			b.Write(data)
		}
		b.WriteString(prGit(t, f.root, "show-ref"))
		b.WriteString(prGit(t, wt, "diff", "--binary", "HEAD"))
		return b.String()
	}
	before := snapshot()
	f.must(t, "issue", "status", n, "--json")
	f.must(t, "issue", "work", n, "--phase", "plan", "--agent", "mock", "--dry-run")
	seedCompleteIssueAcceptance(t, f, mustIssueNumber(n))
	if after := snapshot(); after != before {
		t.Fatal("read-only commands changed state or source")
	}
	f.must(t, "issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false")
	seedCompleteIssueAcceptance(t, f, mustIssueNumber(n))
	// Reacquire through the normal CLI as well as preserving the existing-claim inspection.
	f.must(t, "issue", "release", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	out, err = f.run("issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false")
	if err != nil {
		t.Fatalf("normal plan retry after real no-op: %v\n%s", err, out)
	}
	if head := prGit(t, wt, "rev-parse", "HEAD"); head != st.HeadSHA {
		t.Fatalf("source moved: %s", head)
	}
	if ref := prGit(t, f.root, "ls-remote", "--refs", "origin", "refs/heads/"+st.Branch); ref != "" {
		t.Fatalf("retry published: %s", ref)
	}
}

func TestIssue324RecordedImplementationResume(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) {
			f := newCLIFixture(t)
			// Direct GitHub doubles use the actual GitStore atomic publication path.
			if backend == "github" {
				p := filepath.Join(f.root, ".swarm/config.yaml")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, append(b, "\n  mock:\n    binary: git\n    harness: mock\n"...), 0644); err != nil {
					t.Fatal(err)
				}
				prGit(t, f.root, "commit", "-am", "allowlist mock")
			}
			remote := filepath.Join(t.TempDir(), "origin.git")
			prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
			prGit(t, f.root, "remote", "add", "origin", remote)
			prGit(t, f.root, "push", "origin", "main")
			number := f.create(t, "agent")
			n := strconv.Itoa(number)
			run := f.run
			if backend == "github" {
				f.manager.SetStore(f.root, trustedLedgerStore(t, f.root, ""))
				tr := checkpointCLITracker{f.tracker}
				run = func(args ...string) (string, error) {
					cmd := f.command(&RootOptions{RepoRoot: f.root, Repo: "test/repo", trackerOverride: tr})
					var out bytes.Buffer
					cmd.SetOut(&out)
					cmd.SetErr(&out)
					cmd.SetArgs(args)
					err := cmd.ExecuteContext(t.Context())
					return out.String(), err
				}
			}
			must := func(args ...string) {
				t.Helper()
				if out, err := run(args...); err != nil {
					t.Fatalf("swarm %v: %v\n%s", args, err, out)
				}
			}
			must("issue", "approve", n)
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'real implementation\\n' > feature.txt")
			must("issue", "work", n, "--agent", "mock", "--headless", "--progress=false")
			st := f.state(t, number)
			wt := workspace.WorktreePath(f.root, number)
			if st.HeadSHA == st.BaseSHA || st.HeadSHA == "" {
				t.Fatal("implementation did not create real source checkpoint")
			}
			remoteRef := prGit(t, f.root, "ls-remote", "--refs", "origin", "refs/heads/"+st.Branch)
			if backend == "local" && remoteRef != "" || backend == "github" && !strings.HasPrefix(remoteRef, st.HeadSHA) {
				t.Fatalf("unexpected publication: %s", remoteRef)
			}
			must("issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false")
			if prGit(t, wt, "rev-parse", "HEAD") != st.HeadSHA {
				t.Fatal("resume lost implementation")
			}
			if b, err := os.ReadFile(filepath.Join(wt, "feature.txt")); err != nil || string(b) != "real implementation\n" {
				t.Fatal("source lost")
			}
			if backend == "github" {
				prGit(t, f.root, "push", "origin", ":refs/heads/"+st.Branch)
				if out, err := run("issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false"); err == nil {
					t.Fatalf("GitStore deleted checkpoint admitted: %s", out)
				}
			}
		})
	}
}

// These doubles fail at real production PR boundaries, after checkpointing.
type issue324PublicationTracker struct {
	*tracker.LocalTracker
	fail  string
	calls int
}

func (tr *issue324PublicationTracker) CreateDraftPR(ctx context.Context, req tracker.DraftPRRequest) (*tracker.PullRequest, error) {
	tr.calls++
	switch tr.fail {
	case "create error":
		return nil, errors.New("PR create failed")
	case "nil result":
		return nil, nil
	}
	return tr.LocalTracker.CreateDraftPR(ctx, req)
}
func (tr *issue324PublicationTracker) UpdateDraftPR(ctx context.Context, n int, title, body string) (*tracker.PullRequest, error) {
	tr.calls++
	if tr.fail == "update error" {
		return nil, errors.New("PR update failed")
	}
	return tr.LocalTracker.UpdateDraftPR(ctx, n, title, body)
}

func TestIssue324SubmitFailureDeniesResume(t *testing.T) {
	for _, failure := range []string{"create error", "nil result", "update error"} {
		t.Run(failure, func(t *testing.T) {
			f := newCLIFixture(t)
			remote := filepath.Join(t.TempDir(), "origin.git")
			prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
			prGit(t, f.root, "remote", "add", "origin", remote)
			prGit(t, f.root, "push", "origin", "main")
			number := f.create(t, "agent")
			n := strconv.Itoa(number)
			f.must(t, "issue", "approve", n)
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "echo implemented > feature.txt")
			f.must(t, "issue", "work", n, "--agent", "mock", "--headless", "--progress=false")
			seedCompleteIssueAcceptance(t, f, mustIssueNumber(n))
			st := f.state(t, number)
			tr := &issue324PublicationTracker{LocalTracker: f.tracker, fail: failure}
			if failure == "update error" {
				if _, err := f.tracker.CreateDraftPR(t.Context(), tracker.DraftPRRequest{IssueNumber: number, Title: "existing", Body: "existing", Head: st.Branch, Base: "main"}); err != nil {
					t.Fatal(err)
				}
			}
			cmd := f.command(&RootOptions{RepoRoot: f.root, trackerOverride: tr})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"issue", "submit", n})
			if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "PR publication/description update failed") {
				t.Fatalf("submit failure: %v\n%s", err, out.String())
			}
			if tr.calls != 1 {
				t.Fatalf("PR boundary calls: %d", tr.calls)
			}
			// No remote task ref or submitted-head record is needed to retain denial.
			if ref := prGit(t, f.root, "ls-remote", "--refs", "origin", "refs/heads/"+st.Branch); ref != "" {
				t.Fatalf("local submit published task: %s", ref)
			}
			if _, err := f.run("issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false"); err == nil {
				t.Fatal("failed PR attempt restored unpublished admission")
			}
		})
	}
}

func TestIssue324ForkReadOnlyConfiguration(t *testing.T) {
	up := newUpstreamRemoteFixture(t)
	f := newCLIFixture(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
	prGit(t, f.root, "remote", "add", "origin", remote)
	prGit(t, f.root, "push", "origin", "main")
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", ":")
	if out, err := f.run("issue", "work", n, "--agent", "mock", "--progress=false"); err == nil || !strings.Contains(err.Error(), "developer produced no implementation") {
		t.Fatalf("no-op setup: %v %s", err, out)
	}
	setFixtureTracker(t, f.root, "github")
	prGit(t, f.root, "remote", "add", "upstream", up.projectBare)
	prGit(t, f.root, "remote", "set-url", "origin", up.aliceBare)
	prGit(t, f.root, "fetch", "upstream")
	prGit(t, f.root, "remote", "set-head", "upstream", "main")
	snapshot := func() string {
		t.Helper()
		var b strings.Builder
		for _, p := range []string{".swarm/local-tracker/state.json", ".swarm/coordination/issue-" + n + ".json"} {
			data, err := os.ReadFile(filepath.Join(f.root, p))
			if err != nil {
				t.Fatal(err)
			}
			b.Write(data)
		}
		wt := workspace.WorktreePath(f.root, number)
		b.WriteString(prGit(t, f.root, "show-ref"))
		b.WriteString(prGit(t, wt, "rev-parse", "HEAD"))
		b.WriteString(prGit(t, wt, "diff", "--binary", "HEAD"))
		b.WriteString(prGit(t, wt, "ls-files", "--stage"))
		b.WriteString(up.refs())
		return b.String()
	}
	before, posted := snapshot(), up.postedComments()
	for range 2 {
		for _, args := range [][]string{{"issue", "status", "42", "--json"}, {"issue", "work", "42", "--phase", "plan", "--agent", "mock", "--dry-run"}} {
			opts := up.as("alice", f.root)
			out, err := up.run(opts, append([]string{"--upstream-remote", "upstream", "--fork"}, args...)...)
			if err != nil {
				t.Fatalf("fork inspection %v: %v\n%s", args, err, out)
			}
			if snapshot() != before {
				t.Errorf("fork read-only %v changed receipt or authority/source/refs", args)
			}
		}
	}
	if up.postedComments() != posted {
		t.Fatal("read-only commands dispatched mutation")
	}
	prGit(t, f.root, "remote", "set-url", "origin", remote)
	setFixtureTracker(t, f.root, "local")
	f.must(t, "issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false")
	seedCompleteIssueAcceptance(t, f, mustIssueNumber(n))
}

// issue466Fixture creates a real coordinator-bound implementation. Tests edit
// the local authority only to reproduce interrupted/expired runs.
func issue466Fixture(t *testing.T) (*cliFixture, int) {
	t.Helper()
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	f.must(t, "issue", "approve", strconv.Itoa(n))
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "echo implemented > feature.txt")
	f.must(t, "issue", "work", strconv.Itoa(n), "--agent", "mock", "--headless", "--progress=false")
	return f, n
}

func issue466State(t *testing.T, f *cliFixture, n int, change func(*lease.IssueState)) {
	t.Helper()
	store := coordination.NewFileStore(f.root)
	raw, rev, err := store.Load(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	var st lease.IssueState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	change(&st)
	raw, err = json.Marshal(&st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CompareAndSwap(t.Context(), n, rev, raw); err != nil {
		t.Fatal(err)
	}
}

func issue466Run(t *testing.T, f *cliFixture, now time.Time, args ...string) string {
	t.Helper()
	cmd := f.command(&RootOptions{RepoRoot: f.root, nowFn: func() time.Time { return now }})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func issue466Status(t *testing.T, raw string) issueStatus {
	t.Helper()
	var got struct {
		Issues []issueStatus `json:"issues"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil || len(got.Issues) != 1 {
		t.Fatalf("status JSON: %v %s", err, raw)
	}
	return got.Issues[0]
}

func TestIssue466ExpiredDeveloperStatus(t *testing.T) {
	for _, mode := range []string{"completed", "missing time", "untrusted", "stale", "failed tester"} {
		t.Run(mode, func(t *testing.T) {
			f, n := issue466Fixture(t)
			completed := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
			expiry := completed.Add(time.Hour)
			issue466State(t, f, n, func(st *lease.IssueState) {
				var arts []contract.Artifact
				for _, a := range st.Artifacts {
					if a.Type == contract.ArtifactValidation {
						if mode == "failed tester" {
							a.Verdict = "FAIL"
							arts = append(arts, a)
						}
						continue
					}
					if a.Type == contract.ArtifactImplementation {
						a.CreatedAt = completed
						switch mode {
						case "missing time":
							a.CreatedAt = time.Time{}
						case "untrusted":
							delete(a.Metadata, "implementation_trusted_origin")
						case "stale":
							a.Metadata["implementation_trusted_head"] = "other"
						}
					}
					arts = append(arts, a)
				}
				st.Artifacts, st.RoleRuns = arts, nil
				st.Lease.Status, st.Lease.ExpiresAt = "active", expiry
			})
			now := expiry.Add(time.Minute)
			text := issue466Run(t, f, now, "issue", "status", strconv.Itoa(n))
			got := issue466Status(t, issue466Run(t, f, now, "issue", "status", strconv.Itoa(n), "--json"))
			if got.NextAction == nil || got.StoppedAt == nil {
				t.Fatal("missing continuation JSON")
			}
			trusted := mode != "untrusted" && mode != "stale"
			if (got.StoppedAt.LastCompletedRole == "developer") != trusted {
				t.Fatalf("completion: %+v", got.StoppedAt)
			}
			if trusted && mode != "missing time" && (got.StoppedAt.CompletedAt == nil || !got.StoppedAt.CompletedAt.Equal(completed)) {
				t.Fatal("timestamp not recorded completion")
			}
			if mode == "missing time" && got.StoppedAt.CompletedAt != nil {
				t.Fatal("invented timestamp")
			}
			if mode == "completed" {
				for _, want := range []string{"developer completed at " + completed.Format(time.RFC3339), "tester not run", "claim expired at " + expiry.Format(time.RFC3339), "will be replaced by a fresh claim", "Next step:", "issue work " + strconv.Itoa(n)} {
					if !strings.Contains(text, want) {
						t.Fatalf("missing %q: %s", want, text)
					}
				}
			}
			if mode == "failed tester" && got.StoppedAt.TesterStatus != "failed" {
				t.Fatal("failed tester shown as not run")
			}
			if got.StoppedAt.ClaimStatus != "expired" || !got.StoppedAt.FreshClaimRequired {
				t.Fatal("expiry projection")
			}
		})
	}
}

func TestIssue464StatusNextActionMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, reason, op string
		args             []string
		human            bool
	}{
		{"human plan", ReasonNeedsApproval, "review-plan", []string{"--approve"}, true},
		{"failed checks", ReasonValidationRepairExhausted, "work", []string{"--phase", "implement"}, false},
		{"pre-existing checks", ReasonPreExistingFailures, "status", nil, true},
		{"missing acceptance", "tester_retry_available", "work", []string{"--phase", "test"}, false},
		{"malformed acceptance", "tester_retry_available", "work", []string{"--phase", "test"}, false},
		{"unmentioned acceptance", "tester_retry_available", "work", []string{"--phase", "test"}, false},
		{"unmet acceptance", ReasonValidationRepairExhausted, "work", []string{"--phase", "implement"}, false},
		{"ready", "ready_to_submit", "submit", nil, false},
		{"expired ready", "resumable_work", "work", nil, false},
		{"submitted", "submitted", "status", nil, false},
		{"other actor", "claim_unavailable", "status", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, n := issue466Fixture(t)
			seedCompleteIssueAcceptance(t, f, n)
			now := time.Now()
			issue466State(t, f, n, func(st *lease.IssueState) {
				switch tc.name {
				case "human plan":
					st.EffectiveGate = contract.GateHuman
					st.HumanPlanReview = nil
				case "expired ready":
					st.Lease.ExpiresAt = now.Add(-time.Second)
				case "submitted":
					st.PRNumber = 42
					st.Lease.Status = "completed"
				case "other actor":
					st.Lease.Owner = "bob"
				}
				for i := range st.Artifacts {
					a := &st.Artifacts[i]
					if a.Type == contract.ArtifactValidation {
						switch tc.name {
						case "failed checks":
							a.Verdict = "FAIL"
						case "pre-existing checks":
							a.Verdict = "FAIL"
							summary := validation.RepairSummary{BaseSHA: st.BaseSHA, HeadSHA: st.HeadSHA,
								FailedChecks: []validation.RepairCheck{{Name: "test", Command: "go test", ExitCode: 1, FailingTests: []string{"TestBaseline"}}},
								Tests:        []validation.FailingTest{{Check: "test", Name: "TestBaseline", Classification: validation.TestPreExisting}}}
							raw, err := json.Marshal(summary)
							if err != nil {
								t.Fatal(err)
							}
							a.Metadata[validation.RepairMetadataKey] = string(raw)
						case "missing acceptance":
							delete(a.Metadata, validation.AcceptanceEvidenceKey)
						case "malformed acceptance":
							validation.StoreAcceptanceSnapshot(a.Metadata, validation.AcceptanceSnapshot{State: "malformed"})
						case "unmentioned acceptance":
							validation.StoreAcceptanceSnapshot(a.Metadata, validation.AcceptanceSnapshot{State: "valid", Block: `{"version":1,"criteria":[]}`})
						case "unmet acceptance":
							raw := strings.ReplaceAll(a.Metadata[validation.AcceptanceEvidenceKey], "met", "unmet")
							a.Metadata[validation.AcceptanceEvidenceKey] = raw
						}
					}
				}
			})
			raw := issue466Run(t, f, now, "issue", "status", strconv.Itoa(n), "--json")
			entry := issue466Status(t, raw)
			a := entry.NextAction
			if a == nil || a.Reason != tc.reason || a.HumanRequired != tc.human {
				t.Fatalf("action: %+v\n%s", a, raw)
			}
			resource, op, args := actionOperation(a)
			wantArgs := append([]string{strconv.Itoa(n)}, tc.args...)
			if resource != "issue" || op != tc.op || !reflect.DeepEqual(args, wantArgs) {
				t.Fatalf("argv: %v want %v", a.Argv, wantArgs)
			}
			text := issue466Run(t, f, now, "issue", "status", strconv.Itoa(n))
			if !strings.Contains(text, "Next step: "+formatCommand(a.Argv)) || strings.Count(text, "Next step:") != 1 {
				t.Fatalf("text/JSON parity: %s", text)
			}
			// Parse the literal argv through the production Cobra tree without
			// executing a potentially mutating recommendation.
			root := f.command(nil)
			found, rest, err := root.Find(a.Argv[1:])
			if err != nil {
				t.Fatal(err)
			}
			if err := found.ParseFlags(rest); err != nil {
				t.Fatal(err)
			}
			if err := found.ValidateArgs(found.Flags().Args()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIssue466WorkDryRunResume(t *testing.T) {
	for _, tc := range []struct {
		name, claim, role string
		phase             string
	}{
		{"live", "reuse", "tester", "all"},
		{"expired", "fresh", "tester", "all"},
		{"released", "fresh", "tester", "all"},
		{"other actor", "blocked", "tester", "all"},
		{"missing token", "blocked", "tester", "all"},
		{"mismatched token", "blocked", "tester", "all"},
		{"plan", "reuse", "planner", "plan"},
		{"test", "reuse", "tester", "test"},
		{"instructions", "reuse", "developer", "all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, n := issue466Fixture(t)
			now := time.Now()
			issue466State(t, f, n, func(st *lease.IssueState) {
				// The developer checkpoint survives interruption before tester.
				var arts []contract.Artifact
				for _, a := range st.Artifacts {
					if a.Type != contract.ArtifactValidation {
						arts = append(arts, a)
					}
				}
				st.Artifacts = arts
				switch tc.name {
				case "expired":
					st.Lease.ExpiresAt = now.Add(-time.Second)
				case "released":
					st.Lease.Status = "released"
				case "other actor":
					st.Lease.Owner = "bob"
				case "mismatched token":
					st.Lease.ClaimID = "different"
				}
			})
			if tc.name == "missing token" {
				config, err := os.UserConfigDir()
				if err != nil {
					t.Fatal(err)
				}
				common, err := filepath.EvalSymlinks(filepath.Join(f.root, ".git"))
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256([]byte(common))
				if err := os.Remove(filepath.Join(config, "swarm", "claims", hex.EncodeToString(digest[:]), strconv.Itoa(n)+".json")); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"issue", "work", strconv.Itoa(n), "--agent", "mock", "--phase", tc.phase, "--dry-run"}
			if tc.name == "instructions" {
				args = append(args, "--developer-instructions", "Revise implementation")
			}
			text := issue466Run(t, f, now, append(args, "--format", "text")...)
			for _, want := range []string{"Claim: " + tc.claim, "Resume at: " + tc.role, "Validation reuse: unavailable", "Submission ready:"} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %s: %s", want, text)
				}
			}
			var got struct {
				Plan workflow.PreflightResult `json:"effective_plan"`
			}
			if err := json.Unmarshal([]byte(issue466Run(t, f, now, args...)), &got); err != nil {
				t.Fatal(err)
			}
			if got.Plan.Continuation == nil || got.Plan.Continuation.Claim != tc.claim || got.Plan.Continuation.ResumeRole != tc.role {
				t.Fatalf("JSON preview: %+v", got.Plan.Continuation)
			}
		})
	}
}

func TestIssue466InspectionReadOnly(t *testing.T) {
	f, n := issue466Fixture(t)
	wt := workspace.WorktreePath(f.root, n)
	snapshot := func() string {
		t.Helper()
		var b strings.Builder
		token, err := lease.LoadClaimToken(f.root, n)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(token)
		for _, root := range []string{f.root, wt} {
			// Includes token, tracker, CAS state, raw index, worktree and refs.
			err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				b.WriteString(p)
				b.Write(data)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return b.String()
	}
	for _, expired := range []bool{false, true} {
		now := time.Now()
		if expired {
			now = f.state(t, n).Lease.ExpiresAt.Add(time.Minute)
		}
		before := snapshot()
		for _, args := range [][]string{
			{"issue", "status", strconv.Itoa(n)},
			{"issue", "status", strconv.Itoa(n), "--json"},
			{"issue", "work", strconv.Itoa(n), "--agent", "mock", "--dry-run", "--format", "text"},
			{"issue", "work", strconv.Itoa(n), "--agent", "mock", "--dry-run"},
		} {
			issue466Run(t, f, now, args...)
		}
		if after := snapshot(); before != after {
			t.Fatal("inspection changed authority, token, refs, index or worktree")
		}
	}
}
