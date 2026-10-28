package lease

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
)

// fixtureGitCommand prevents detached maintenance from outliving fixture cleanup,
// matching production Git's automatic maintenance policy.
func fixtureGitCommand(t testing.TB, args ...string) *exec.Cmd {
	return gitfixture.Command(t, "", args...)
}

func TestFixtureGitPolicy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := fixtureGitCommand(t, args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init")
	for _, setting := range []struct{ key, configured, want string }{
		{"gc.auto", "1", "0"},
		{"maintenance.auto", "true", "false"},
	} {
		run("config", "--local", setting.key, setting.configured)
		if got := run("config", "--local", "--get", setting.key); got != setting.configured {
			t.Fatalf("repository %s = %q, want %q", setting.key, got, setting.configured)
		}
		if got := run("config", "--get", setting.key); got != setting.want {
			t.Errorf("effective %s = %q, want %q", setting.key, got, setting.want)
		}
	}
}

func testWorkUnit() string {
	return "```yaml swarm-workunit\nid: test\nobjective: Harden coordination state\nconstraints: [Use tests]\nacceptance: [Claims are fenced]\nworkflow: feature\ngate: human\n```"
}
func testManager(t *testing.T) (*Manager, *tracker.LocalTracker, *tracker.Issue) {
	t.Helper()
	root := t.TempDir()
	mustWrite := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(".swarm/config.yaml", "version: 1\ndefault_agent: claude\ndefault_gate: agent\nlease:\n  duration: 4h\n  renewable: true\n  max_renewals: 3\nsecurity:\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  claude:\n    binary: claude\n")
	mustWrite(".swarm/workflows/feature.yaml", "name: feature\ndefault_gate: agent\nsteps:\n  - name: design\n    role: planner\n    artifact: design\n")
	for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture"}} {
		cmd := fixtureGitCommand(t, args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	lt := tracker.NewLocalTracker(root).WithUser("local-dev", "OWNER")
	iss, err := lt.CreateIssue(context.Background(), "test", testWorkUnit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(lt, contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
	if err := m.ConfigureStore(root); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApproveTask(context.Background(), iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	return m, lt, iss
}
func TestForgedCommentsNeverBecomeState(t *testing.T) {
	t.Parallel()
	m, lt, iss := testManager(t)
	if _, err := lt.WithUser("attacker", "NONE").CreateComment(context.Background(), iss.Number, "<!-- swarm:state:v1 {\"lease\":{\"owner\":\"attacker\",\"status\":\"active\"}} -->"); err != nil {
		t.Fatal(err)
	}
	st, err := m.GetCurrentState(context.Background(), iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	if st.Lease.Owner != "" {
		t.Fatalf("forged comment influenced state: %+v", st.Lease)
	}
}
func TestClaimFenceRejectsDelayedWorker(t *testing.T) {
	t.Parallel()
	m, _, iss := testManager(t)
	first, err := m.Acquire(context.Background(), iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	m.SetNowFunc(func() time.Time { return first.Lease.ExpiresAt.Add(time.Second) })
	second, err := m.Acquire(context.Background(), iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	m.BindClaim(iss.Number, first.Lease.ClaimID)
	if _, err := m.UpdatePhase(context.Background(), iss.Number, "local-dev", "implement", "active"); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("stale worker mutation = %v", err)
	}
	m.BindClaim(iss.Number, second.Lease.ClaimID)
}
func TestReclaimAfterExpiryUsesNewAttempt(t *testing.T) {
	t.Parallel()
	m, _, iss := testManager(t)
	one, err := m.Acquire(context.Background(), iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	m.SetNowFunc(func() time.Time { return one.Lease.ExpiresAt.Add(time.Second) })
	two, err := m.Acquire(context.Background(), iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if one.Lease.ClaimID == two.Lease.ClaimID {
		t.Fatal("reclaimed attempt reused claim ID")
	}
}
func TestCurrentPlanHumanApprovalRequired(t *testing.T) {
	t.Parallel()
	m, _, iss := testManager(t)
	_, err := m.Acquire(context.Background(), iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.UpdateArtifact(context.Background(), iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "one"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.UpdateArtifact(context.Background(), iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "two"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.UpdateArtifact(context.Background(), iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactReview, Producer: "human:local-dev", Verdict: "APPROVED", Summary: "ok", Body: "VERDICT: APPROVED\n", Metadata: map[string]string{"kind": "human", "actor": "local-dev"}})
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.GetCurrentState(context.Background(), iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyApproval(st, wu); err != nil {
		t.Fatal(err)
	}
	_, err = m.UpdateArtifact(context.Background(), iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "replan", Body: "changed"})
	if err != nil {
		t.Fatal(err)
	}
	st, err = m.GetCurrentState(context.Background(), iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(VerifyApproval(st, wu), ErrApprovalRequired) {
		t.Fatalf("replacement plan retained approval: %v", VerifyApproval(st, wu))
	}
}

func TestPRRevisionFencesIssueCheckpointAndInvalidatesApproval(t *testing.T) {
	t.Parallel()
	m, _, issue := testManager(t)
	ctx := context.Background()
	if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	for _, art := range []contract.Artifact{
		{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "plan"},
		{Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "tests"},
	} {
		if _, err := m.UpdateArtifact(ctx, issue.Number, "local-dev", art); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.RecordHumanReview(ctx, issue.Number, "APPROVED", "verified"); err != nil {
		t.Fatal(err)
	}
	st, revision, err := m.load(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	st.PRNumber = 37
	if _, err := m.save(ctx, issue.Number, revision, st); err != nil {
		t.Fatal(err)
	}
	remoteHead, localHead := strings.Repeat("a", 40), strings.Repeat("b", 40)
	st, err = m.BeginPRRevision(ctx, issue.Number, "local-dev", 37, remoteHead, remoteHead, "swarm/pr-37")
	if err != nil {
		t.Fatal(err)
	}
	if st.RevisionPlan == nil || st.RevisionPlan.Original.Plan != "plan" || st.RevisionPlan.Original.TestPlan != "tests" {
		t.Fatal("lost approved original", st)
	}
	if st.PRRevision == nil || len(st.Artifacts) != 0 || len(st.Attestations) != 0 {
		t.Fatalf("revision retained stale evidence: %+v", st)
	}
	if _, err := m.RecordCheckpoint(ctx, issue.Number, "local-dev", "swarm/issue-1-test", remoteHead); err == nil {
		t.Fatal("issue checkpoint bypassed pending PR revision")
	}
	if _, err := m.RecordPRHead(ctx, issue.Number, "local-dev", 38, "swarm/pr-37", localHead); err == nil {
		t.Fatal("unlinked PR checkpoint accepted")
	}
	if _, err := m.RecordPRHead(ctx, issue.Number, "local-dev", 37, "swarm/pr-37", localHead); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompletePRRevision(ctx, issue.Number, "local-dev", 37, remoteHead, remoteHead); err == nil {
		t.Fatal("stale local head completed revision")
	}
	m.BindClaim(issue.Number, "stale-token")
	if _, err := m.CompletePRRevision(ctx, issue.Number, "local-dev", 37, remoteHead, localHead); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("stale claim completion = %v", err)
	}
	m.BindClaim(issue.Number, st.Lease.ClaimID)
	if _, err := m.CompletePRRevision(ctx, issue.Number, "local-dev", 37, remoteHead, localHead); err == nil {
		t.Fatal("origin completion recorded a publication without an atomic publishing store")
	}
	if st, err = m.CompletePRRevisionLocal(ctx, issue.Number, "local-dev", 37, remoteHead, localHead); err != nil || st.PRRevision != nil {
		t.Fatalf("complete revision: %+v, %v", st, err)
	}
}

func TestLivingCommentProjectionAndPIIFreeProvenance(t *testing.T) {
	t.Parallel()
	m, lt, iss := testManager(t)
	ctx := context.Background()
	m.SetNodeProfile(&contract.NodeProfile{NodeID: "secret-node-host-01", Capabilities: []string{"coding"}})
	_ = lt.AddLabels(ctx, iss.Number, []string{"swarm:draft", "swarm:needs-scope", "swarm:blocked"})
	if _, err := m.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	refreshed, err := lt.GetIssue(ctx, iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"swarm:draft", "swarm:needs-scope", "swarm:blocked"} {
		if slices.Contains(refreshed.Labels, removed) {
			t.Fatalf("ApproveTask did not remove %q from labels: %v", removed, refreshed.Labels)
		}
	}
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "claude", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{
		"role":            "planner",
		"agent":           "claude",
		"harness":         "claude",
		"harness_version": "2.1.0",
		"model":           "claude-sonnet-4-5",
		"sandbox":         "host",
	}
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{
		Type:     contract.ArtifactDesign,
		Producer: "claude:planner",
		Summary:  "plan summary",
		Body:     "# Detailed Plan\n\nStep 1: Update contract",
		Metadata: meta,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{
		Type:     contract.ArtifactTestPlan,
		Producer: "claude:planner",
		Summary:  "test plan summary",
		Body:     "# Detailed Test Plan\n\nRun go test ./...",
		Metadata: meta,
	}); err != nil {
		t.Fatal(err)
	}

	comments, err := lt.ListComments(ctx, iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("expected exactly 1 living projection comment, got %d", len(comments))
	}
	body := comments[0].Body
	for _, want := range []string{
		"<!-- swarm:audit:v1 -->",
		"### 🐝 SwarmOS Orchestration & Provenance State",
		"#### 🔒 Harness & Model Provenance Attestation",
		"claude-sonnet-4-5",
		"<details>",
		"# Detailed Plan",
		"# Detailed Test Plan",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("living comment missing %q:\n%s", want, body)
		}
	}
	for _, pii := range []string{"local-dev", "secret-node-host-01"} {
		if strings.Contains(body, pii) {
			t.Fatalf("living comment leaked PII %q:\n%s", pii, body)
		}
	}

	st, err := m.GetCurrentState(ctx, iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	prov := BuildProvenanceAttestation(wu, st)
	rawProv, err := json.Marshal(prov)
	if err != nil {
		t.Fatal(err)
	}
	for _, pii := range []string{"local-dev", "secret-node-host-01"} {
		if strings.Contains(string(rawProv), pii) {
			t.Fatalf("provenance JSON leaked PII %q: %s", pii, string(rawProv))
		}
	}
}

func TestCollaboratorAllowlistsEnforceWorkerAndReviewerSeparation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(".swarm/config.yaml", "version: 1\ndefault_agent: claude\ndefault_gate: agent\nlease:\n  duration: 4h\n  renewable: true\n  max_renewals: 3\nsecurity:\n  require_ready_label: true\n  ready_label: swarm:ready\n  approved_collaborators: [worker-bot, reviewer-bot]\n  reviewer_collaborators: [reviewer-bot]\nagents:\n  claude:\n    binary: claude\n")
	mustWrite(".swarm/workflows/feature.yaml", "name: feature\ndefault_gate: agent\nsteps:\n  - name: design\n    role: planner\n    artifact: design\n")
	for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture"}} {
		cmd := fixtureGitCommand(t, args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	ctx := context.Background()
	adminTracker := tracker.NewLocalTracker(root).WithUser("admin-user", "OWNER")
	iss, err := adminTracker.CreateIssue(ctx, "test", testWorkUnit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	adminMgr := NewManager(adminTracker, contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
	if err := adminMgr.ConfigureStore(root); err != nil {
		t.Fatal(err)
	}
	if _, err := adminMgr.ApproveTask(ctx, iss.Number, "admin-user"); err != nil {
		t.Fatal(err)
	}

	// Unauthorized collaborator cannot acquire lease
	unlistedMgr := NewManager(tracker.NewLocalTracker(root).WithUser("outsider", "COLLABORATOR"), contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
	_ = unlistedMgr.ConfigureStore(root)
	if _, err := unlistedMgr.Acquire(ctx, iss.Number, "outsider", "mock", "plan", time.Hour, 1); err == nil {
		t.Fatal("expected unlisted collaborator to be rejected by approved_collaborators")
	}

	// Authorized worker-bot can acquire and publish plan, but cannot publish review verdict
	workerMgr := NewManager(tracker.NewLocalTracker(root).WithUser("worker-bot", "COLLABORATOR"), contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
	_ = workerMgr.ConfigureStore(root)
	if _, err := workerMgr.Acquire(ctx, iss.Number, "worker-bot", "mock", "plan", time.Hour, 1); err != nil {
		t.Fatalf("worker-bot acquire failed: %v", err)
	}
	if _, err := workerMgr.UpdateArtifact(ctx, iss.Number, "worker-bot", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "p", Body: "plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workerMgr.UpdateArtifact(ctx, iss.Number, "worker-bot", contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "t", Body: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workerMgr.UpdateArtifact(ctx, iss.Number, "worker-bot", contract.Artifact{
		Type: contract.ArtifactReview, Producer: "mock:reviewer", Verdict: "APPROVED", Summary: "r", Body: "VERDICT: APPROVED",
		Metadata: map[string]string{"kind": "agent", "actor": "worker-bot"},
	}); err == nil {
		t.Fatal("expected worker-bot to be rejected by reviewer_collaborators when publishing review")
	}

	// Authorized reviewer-bot can record review
	reviewerMgr := NewManager(tracker.NewLocalTracker(root).WithUser("reviewer-bot", "COLLABORATOR"), contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
	_ = reviewerMgr.ConfigureStore(root)
	if _, err := reviewerMgr.RecordHumanReview(ctx, iss.Number, "APPROVED", "LGTM"); err != nil {
		t.Fatalf("reviewer-bot review failed: %v", err)
	}
}

func TestEpicIssuesRejectedAtApproveAcquireAndCheckApprovedState(t *testing.T) {
	t.Parallel()
	m, lt, iss := testManager(t)
	ctx := context.Background()

	// Create a typed Epic whose body is formatted like a valid WorkUnit and mislabeled swarm:ready
	epic, err := lt.CreateTypedIssue(ctx, "WorkUnit-shaped Epic", testWorkUnit(), []string{"swarm:ready"}, "Epic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApproveTask(ctx, epic.Number, "local-dev"); err == nil {
		t.Fatal("ApproveTask must reject Epic issue")
	}
	if _, err := m.Acquire(ctx, epic.Number, "local-dev", "claude", "plan", time.Hour, 1); err == nil {
		t.Fatal("Acquire must reject Epic issue")
	}

	// Acquire a normal approved issue, then retype it to Epic in the local state and verify checkApprovedState fences it
	claimed, err := m.Acquire(ctx, iss.Number, "local-dev", "claude", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(m.repoRoot, ".swarm", "local-tracker", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	issuesMap := st["issues"].(map[string]any)
	issObj := issuesMap["1"].(map[string]any)
	issObj["type"] = "Epic"
	updatedRaw, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(statePath, updatedRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	m.BindClaim(iss.Number, claimed.Lease.ClaimID)
	if err := m.CheckClaim(ctx, iss.Number, "local-dev"); err == nil {
		t.Fatal("CheckClaim must reject issue retyped to Epic")
	}
	if _, err := m.UpdatePhase(ctx, iss.Number, "local-dev", "implement", "active"); err == nil {
		t.Fatal("UpdatePhase must reject issue retyped to Epic")
	}
	if _, err := m.RecordCheckpoint(ctx, iss.Number, "local-dev", "swarm/issue-1-test", claimed.BaseSHA); err == nil {
		t.Fatal("RecordCheckpoint must reject issue retyped to Epic")
	}
}

func TestForkAcquireAndTakeoverClearsCheckpointAndHeadArtifacts(t *testing.T) {
	t.Parallel()
	m, lt, iss := testManager(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	m.SetNowFunc(func() time.Time { return now })

	st1, err := m.AcquireFork(ctx, iss.Number, "local-dev", "claude", "all", "local-dev/swarm", 24*time.Hour, 99)
	if err != nil {
		t.Fatalf("AcquireFork failed: %v", err)
	}
	if st1.ForkRepo != "local-dev/swarm" || st1.Lease.ExpiresAt.Sub(now) != 4*time.Hour || st1.Lease.MaxRenewals != 3 {
		t.Fatalf("AcquireFork did not enforce approved policy duration/max_renewals: %+v", st1.Lease)
	}
	branch1 := "swarm/issue-1-" + st1.Lease.ClaimID
	for _, art := range []contract.Artifact{
		{Type: contract.ArtifactDesign, Producer: "claude:planner", Summary: "Plan", Body: "# Plan\n"},
		{Type: contract.ArtifactTestPlan, Producer: "claude:planner", Summary: "Tests", Body: "# Tests\n"},
		{Type: contract.ArtifactImplementation, Producer: "claude:developer", Verdict: "PASS", Summary: "Impl", Body: "# Impl\n"},
	} {
		if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", art); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.RecordCheckpoint(ctx, iss.Number, "local-dev", branch1, st1.BaseSHA); err != nil {
		t.Fatalf("RecordCheckpoint failed: %v", err)
	}

	// Advance past expiry and acquire replacement fork claim
	now = now.Add(5 * time.Hour)
	m2 := NewManager(lt.WithUser("bob", "NONE"), m.security)
	m2.SetStore(m.repoRoot, m.Store())
	m2.SetNowFunc(func() time.Time { return now })
	st2, err := m2.AcquireFork(ctx, iss.Number, "bob", "claude", "implement", "bob/swarm", time.Hour, 3)
	if err != nil {
		t.Fatalf("bob AcquireFork takeover failed: %v", err)
	}
	if st2.ForkRepo != "bob/swarm" || st2.Branch != "" || st2.HeadSHA != "" || st2.Lease.ClaimID == st1.Lease.ClaimID {
		t.Fatalf("takeover did not reset fork checkpoint identity: %+v", st2)
	}
	if len(st2.Artifacts) != 2 {
		t.Fatalf("expected only design and test-plan artifacts retained after takeover, got %+v", st2.Artifacts)
	}

	// Displaced claim token fails with ErrClaimFence
	m.BindClaim(iss.Number, st1.Lease.ClaimID)
	if _, err := m.Renew(ctx, iss.Number, "local-dev", time.Hour); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("expected ErrClaimFence for displaced claim, got %v", err)
	}
}

func TestReviewApprovalRequiresAuthenticatedConsistentEvidence(t *testing.T) {
	t.Parallel()
	m, lt, iss := testManager(t)
	ctx := context.Background()
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "claude", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	for _, art := range []contract.Artifact{
		{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "plan"},
		{Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "tests"},
	} {
		if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", art); err != nil {
			t.Fatal(err)
		}
	}
	wu, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	makeReview := func() contract.Artifact {
		return contract.Artifact{Type: contract.ArtifactReview, Producer: "mock:reviewer", Verdict: "APPROVED", Summary: "review", Body: "VERDICT: APPROVED\n", Metadata: map[string]string{"kind": "agent", "actor": "spoofed-maintainer", "review_source": "authenticated-agent"}}
	}
	st, err := m.UpdateAdvisoryReview(ctx, iss.Number, "local-dev", makeReview())
	if err != nil {
		t.Fatal(err)
	}
	st.EffectiveGate = contract.GateAgent
	if !errors.Is(VerifyApproval(st, wu), ErrApprovalRequired) {
		t.Fatal("advisory review satisfied agent gate")
	}
	for _, art := range st.Artifacts {
		if art.Type == contract.ArtifactReview && (art.Metadata["actor"] != "local-dev" || art.Metadata["review_source"] != "contributor") {
			t.Fatalf("trusted client metadata: %+v", art.Metadata)
		}
	}
	m.tracker = lt.WithUser("local-dev", "NONE")
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", makeReview()); err == nil {
		t.Fatal("unauthorized actor recorded trusted agent review")
	}
	m.tracker = lt
	for _, body := range []string{"VERDICT: CHANGES_REQUESTED\n", "VERDICT: APPROVED\nVERDICT: APPROVED", "unstructured approval"} {
		art := makeReview()
		art.Body = body
		if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", art); err == nil {
			t.Fatalf("accepted inconsistent review body %q", body)
		}
	}
	st, err = m.RecordHumanReview(ctx, iss.Number, "APPROVED", "Reviewed the current plan")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyApproval(st, wu); err != nil {
		t.Fatalf("authenticated human approval failed: %v", err)
	}
	for name, tamper := range map[string]func(*IssueState){
		"missing source":    func(s *IssueState) { delete(s.Artifacts[2].Metadata, "review_source") },
		"advisory source":   func(s *IssueState) { s.Artifacts[2].Metadata["review_source"] = "contributor" },
		"body changed":      func(s *IssueState) { s.Artifacts[2].Body += "tampered" },
		"base changed":      func(s *IssueState) { s.BaseSHA = "different" },
		"policy changed":    func(s *IssueState) { s.PolicyDigest = "different" },
		"plan changed":      func(s *IssueState) { s.Artifacts[0].Body = "different" },
		"test plan changed": func(s *IssueState) { s.Artifacts[1].Body = "different" },
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(st)
			var copy IssueState
			if err := json.Unmarshal(raw, &copy); err != nil {
				t.Fatal(err)
			}
			tamper(&copy)
			if !errors.Is(VerifyApproval(&copy, wu), ErrApprovalRequired) {
				t.Fatal("tampered review satisfied gate")
			}
		})
	}
}

func TestAgentReviewCannotBeReboundAfterInputsChange(t *testing.T) {
	t.Parallel()
	m, _, iss := testManager(t)
	ctx := context.Background()
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "claude", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	for _, art := range []contract.Artifact{
		{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "original plan"},
		{Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "tests"},
	} {
		if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", art); err != nil {
			t.Fatal(err)
		}
	}
	before, err := m.GetCurrentState(ctx, iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	metadata := ReviewBindings(before)
	metadata["kind"], metadata["actor"] = "agent", "untrusted-metadata-actor"
	review := contract.Artifact{Type: contract.ArtifactReview, Producer: "mock:reviewer", Verdict: "APPROVED", Summary: "reviewed original plan", Body: "VERDICT: APPROVED\n", Metadata: metadata}
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "new plan", Body: "different plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", review); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("stale reviewer result rebound to new plan: %v", err)
	}
	after, err := m.GetCurrentState(ctx, iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range ReviewBindings(after) {
		review.Metadata[key] = value
	}
	review.Metadata["review_claim_id"] = "previous-claim"
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", review); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("stale claim review accepted: %v", err)
	}
	review.Metadata["review_claim_id"] = after.Lease.ClaimID
	accepted, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", review)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	accepted.EffectiveGate = contract.GateAgent
	if err := VerifyApproval(accepted, wu); err != nil {
		t.Fatalf("fresh bound agent review rejected: %v", err)
	}
}

func TestAcquirePRKeepsForkPRLinkAndBlocksForkReclaim(t *testing.T) {
	t.Parallel()
	m, _, iss := testManager(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	m.SetNowFunc(func() time.Time { return now })
	forked, err := m.AcquireFork(ctx, iss.Number, "local-dev", "claude", "all", "local-dev/swarm", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	st, revision, err := m.load(ctx, iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	st.Branch, st.PRNumber, st.PRURL = "swarm/issue-1-"+forked.Lease.ClaimID, 41, "https://example.invalid/pull/41"
	st.Lease.Status = "completed"
	if _, err := m.save(ctx, iss.Number, revision, st); err != nil {
		t.Fatal(err)
	}

	claimed, err := m.AcquirePR(ctx, iss.Number, 41, "local-dev", "pr", "all", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.PRNumber != 41 || claimed.ForkRepo != "local-dev/swarm" || claimed.Branch == "" || claimed.PRURL == "" {
		t.Fatalf("PR acquisition dropped the fork PR link: %+v", claimed)
	}
	head := strings.Repeat("c", 40)
	if _, err := m.BeginPRRevision(ctx, iss.Number, "local-dev", 41, head, head, "swarm/pr-41"); err != nil {
		t.Fatalf("revision on fork-contributed PR: %v", err)
	}

	now = now.Add(2 * time.Hour)
	if _, err := m.AcquireFork(ctx, iss.Number, "local-dev", "claude", "all", "local-dev/swarm", time.Hour, 1); err == nil || !strings.Contains(err.Error(), "pending PR revision") {
		t.Fatalf("fork claim over pending PR revision = %v", err)
	}
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 1); err == nil || !strings.Contains(err.Error(), "pending PR revision") {
		t.Fatalf("issue claim over pending PR revision = %v", err)
	}
}

func TestHeartbeatRenewsAndStopsOnFenceLoss(t *testing.T) {
	t.Parallel()
	m, _, iss := testManager(t)
	ctx := context.Background()
	st, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", 3*time.Second, 3)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := m.StartHeartbeat(ctx, iss.Number, "local-dev", 3*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := m.GetCurrentState(ctx, iss.Number)
		if err != nil {
			t.Fatal(err)
		}
		if current.Lease.Renewals > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	heartbeat.Stop()
	if heartbeat.Err() != nil {
		t.Fatalf("healthy heartbeat reported %v", heartbeat.Err())
	}
	if current, _ := m.GetCurrentState(ctx, iss.Number); current.Lease.Renewals == 0 || current.Lease.ClaimID != st.Lease.ClaimID {
		t.Fatalf("heartbeat did not renew the bound claim: %+v", current.Lease)
	}

	m.BindClaim(iss.Number, "displaced")
	lost := m.StartHeartbeat(ctx, iss.Number, "local-dev", 3*time.Second)
	select {
	case <-lost.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat kept running after losing its claim")
	}
	lost.Stop()
	if !errors.Is(lost.Err(), ErrClaimFence) {
		t.Fatalf("lost heartbeat error = %v", lost.Err())
	}
}

func TestCompletePRRevisionPublishesHeadAndStateAtomically(t *testing.T) {
	t.Parallel()
	m, lt, iss := testManager(t)
	ctx := context.Background()
	root := m.repoRoot
	git := func(args ...string) string {
		t.Helper()
		out, err := fixtureGitCommand(t, append([]string{"-C", root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	if out, err := fixtureGitCommand(t, "init", "--bare", "--initial-branch=main", origin).CombinedOutput(); err != nil {
		t.Fatalf("init origin: %v: %s", err, out)
	}
	git("remote", "add", "origin", origin)
	base := git("rev-parse", "HEAD")
	branch := "swarm/issue-1-test"
	git("push", "origin", base+":refs/heads/main", base+":refs/heads/"+branch)

	gm := NewManager(lt, m.security)
	gm.SetStore(root, coordination.NewGitStore(root))
	if _, err := gm.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := gm.Acquire(ctx, iss.Number, "local-dev", "pr", "all", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	st, revision, err := gm.load(ctx, iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	st.Branch, st.PRNumber = branch, 37
	if _, err := gm.save(ctx, iss.Number, revision, st); err != nil {
		t.Fatal(err)
	}
	if _, err := gm.BeginPRRevision(ctx, iss.Number, "local-dev", 37, base, base, "swarm/pr-37"); err != nil {
		t.Fatal(err)
	}
	if _, err := gm.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "plan"}); err != nil {
		t.Fatal(err)
	}
	if st, err = gm.InvalidatePRRevisionEvidence(ctx, iss.Number, "local-dev", 37); err != nil || len(st.Artifacts) != 0 {
		t.Fatalf("invalidate revision evidence: %+v, %v", st, err)
	}
	if _, err := gm.InvalidatePRRevisionEvidence(ctx, iss.Number, "local-dev", 38); err == nil {
		t.Fatal("invalidated evidence for a PR without a pending revision")
	}
	git("checkout", "-q", "-b", "swarm/pr-37", base)
	git("commit", "-q", "--allow-empty", "-m", "PR revision")
	candidate := git("rev-parse", "HEAD")
	if _, err := gm.RecordPRHead(ctx, iss.Number, "local-dev", 37, "swarm/pr-37", candidate); err != nil {
		t.Fatal(err)
	}
	st, err = gm.CompletePRRevision(ctx, iss.Number, "local-dev", 37, base, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if st.PRRevision != nil || st.HeadSHA != candidate || st.Lease.Status != "completed" {
		t.Fatalf("completed state = %+v", st)
	}
	if got := strings.TrimSpace(func() string {
		out, _ := fixtureGitCommand(t, "-C", origin, "rev-parse", "refs/heads/"+branch).Output()
		return string(out)
	}()); got != candidate {
		t.Fatalf("origin PR branch = %s, want %s", got, candidate)
	}
	persisted, err := gm.GetCurrentState(ctx, iss.Number)
	if err != nil || persisted.PRRevision != nil || persisted.HeadSHA != candidate {
		t.Fatalf("persisted state = %+v, %v", persisted, err)
	}
}

func TestIssueEditApprovalRemainsBoundToContent(t *testing.T) {
	t.Parallel()
	m, tr, issue := testManager(t)
	ctx := context.Background()
	before, err := m.GetCurrentState(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	original, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		t.Fatal(err)
	}
	// Presentation changes outside the parsed contract preserve its approval.
	if _, err := tr.UpdateIssueBody(ctx, issue.Number, issue.Body+"\n<!-- note -->\n"); err != nil {
		t.Fatal(err)
	}
	unchanged, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body+"\n<!-- note -->\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, before.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !TaskApprovalCurrent(before, unchanged, cfg) {
		t.Fatal("unchanged content lost task approval")
	}
	objective := "Revised coordination contract"
	_, edited, err := contract.EditWorkUnitIssue(issue.Number, issue.Title, issue.Body, contract.WorkUnitEdit{Objective: &objective})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.UpdateIssueBody(ctx, issue.Number, edited); err != nil {
		t.Fatal(err)
	}
	changed, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyApproval(before, changed); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("changed contract retained approval: %v", err)
	}
	if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 3); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("changed contract acquired: %v", err)
	}
	current, err := m.GetCurrentState(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	if current.TaskDigest != contract.DigestWorkUnit(original) {
		t.Fatal("editing minted approval")
	}
	renewed, err := m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if !TaskApprovalCurrent(renewed, changed, cfg) {
		t.Fatal("explicit approval did not authorize edited content")
	}
	if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 3); err != nil {
		t.Fatal(err)
	}
}

func freshPlanRebaseFixture(t *testing.T) (*Manager, int, *IssueState) {
	t.Helper()
	m, _, issue := testManager(t)
	ctx := context.Background()
	if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	st, rev, err := m.load(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	st.PRNumber = 37
	st.PRRevision = &PRRevision{Number: 37, Intent: "rebase", RemoteHeadSHA: strings.Repeat("a", 40), LocalHeadSHA: strings.Repeat("b", 40), Branch: "swarm/pr-37"}
	st.ApprovalTransition = &ApprovalTransition{OriginBase: st.BaseSHA}
	st.LastRebase = &RebaseEvidence{Verified: true, NewHead: st.PRRevision.LocalHeadSHA}
	for _, typ := range []contract.ArtifactType{contract.ArtifactDesign, contract.ArtifactTestPlan, contract.ArtifactReview, contract.ArtifactImplementation, contract.ArtifactValidation} {
		st.Artifacts = append(st.Artifacts, contract.Artifact{Type: typ, Body: "old evidence"})
	}
	st.Attestations = []contract.RoleAttestation{{Role: "tester", Harness: "old"}}
	if _, err := m.save(ctx, issue.Number, rev, st); err != nil {
		t.Fatal(err)
	}
	return m, issue.Number, st
}

func TestPRRevisionFreshPlan(t *testing.T) {
	t.Parallel()
	for _, first := range []contract.ArtifactType{contract.ArtifactDesign, contract.ArtifactTestPlan} {
		t.Run(string(first), func(t *testing.T) {
			t.Parallel()
			m, n, before := freshPlanRebaseFixture(t)
			ctx := context.Background()
			other := contract.ArtifactDesign
			if first == other {
				other = contract.ArtifactTestPlan
			}
			for i, typ := range []contract.ArtifactType{first, other, first} {
				st, err := m.UpdateArtifact(ctx, n, "local-dev", contract.Artifact{Type: typ, Producer: "mock:planner", Summary: "fresh", Body: "fresh " + string(typ), Metadata: map[string]string{"harness": "mock"}})
				if err != nil {
					t.Fatal(err)
				}
				identity := *before.PRRevision
				identity.Intent = "work"
				if *st.PRRevision != identity || st.EffectiveGate != before.EffectiveGate || st.LastRebase == nil {
					t.Fatalf("revision identity changed: %+v", st)
				}
				want := 2
				if i == 0 {
					want = 1
				}
				if len(st.Artifacts) != want || st.ApprovalTransition != nil {
					t.Fatalf("old evidence retained: %+v", st)
				}
				for _, a := range st.Attestations {
					if a.Harness != "mock" || a.Role != "planner" {
						t.Fatalf("old attestation retained: %+v", a)
					}
				}
				if len(st.Attestations) == 0 {
					t.Fatal("new planner provenance lost")
				}
				if i == 0 {
					if _, err := m.RecordHumanReview(ctx, n, "APPROVED", "partial plan"); err == nil {
						t.Fatal("partial staging accepted review")
					}
				}
				wu, err := contract.ParseWorkUnitFromIssue(n, "test", testWorkUnit())
				if err != nil {
					t.Fatal(err)
				}
				if !errors.Is(VerifyApproval(st, wu), ErrApprovalRequired) {
					t.Fatal("fresh plan inherited approval")
				}
			}
			st, err := m.BeginPRRevision(ctx, n, "local-dev", 37, before.PRRevision.RemoteHeadSHA, before.PRRevision.LocalHeadSHA, before.PRRevision.Branch)
			if err != nil || len(st.Artifacts) != 2 {
				t.Fatalf("resume lost fresh artifacts: %v %+v", err, st)
			}
		})
	}
}

func TestPRRevisionFreshPlanRejectedWrites(t *testing.T) {
	t.Parallel()
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid", true: "stale claim"}[stale], func(t *testing.T) {
			t.Parallel()
			m, n, before := freshPlanRebaseFixture(t)
			art := contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "fresh", Body: "fresh"}
			if stale {
				m.BindClaim(n, "stale")
			} else {
				art.Type = "invalid"
			}
			_, err := m.UpdateArtifact(context.Background(), n, "local-dev", art)
			if err == nil || (stale && !errors.Is(err, ErrClaimFence)) {
				t.Fatalf("rejected write: %v", err)
			}
			after, err := m.GetCurrentState(context.Background(), n)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if string(a) != string(b) {
				t.Fatal("rejected write changed evidence")
			}
		})
	}
}

func TestPRRevisionUnstagedRebaseResumeInvalidatesEvidence(t *testing.T) {
	t.Parallel()
	m, n, before := freshPlanRebaseFixture(t)
	st, err := m.BeginPRRevision(context.Background(), n, "local-dev", 37, before.PRRevision.RemoteHeadSHA, before.PRRevision.LocalHeadSHA, before.PRRevision.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if st.PRRevision.Intent != "work" || len(st.Artifacts) != 0 || len(st.Attestations) != 0 || st.ApprovalTransition != nil {
		t.Fatalf("unstaged resume retained evidence: %+v", st)
	}
}

func TestPRRevisionFreshPlanOrdinaryUpdatesRetainEvidence(t *testing.T) {
	t.Parallel()
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "issue", true: "work revision"}[pending], func(t *testing.T) {
			t.Parallel()
			m, n, before := freshPlanRebaseFixture(t)
			st, rev, err := m.load(context.Background(), n)
			if err != nil {
				t.Fatal(err)
			}
			if pending {
				st.PRRevision.Intent = "work"
			} else {
				st.PRRevision = nil
			}
			if _, err := m.save(context.Background(), n, rev, st); err != nil {
				t.Fatal(err)
			}
			after, err := m.UpdateArtifact(context.Background(), n, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "new plan"})
			if err != nil {
				t.Fatal(err)
			}
			// Ordinary plan edits remove review artifacts, but do not reset all revision evidence.
			if len(after.Artifacts) != len(before.Artifacts)-1 || len(after.Attestations) != len(before.Attestations) || after.ApprovalTransition == nil {
				t.Fatal("ordinary update unexpectedly reset revision evidence")
			}
		})
	}
}

func TestArtifactProvenanceRecordsEffort(t *testing.T) {
	t.Parallel()
	m, _, iss := testManager(t)
	ctx := context.Background()
	if _, err := m.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "claude", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	att := contract.RoleAttestation{Role: "planner", Agent: "claude-planner", Harness: "claude", Model: "claude-opus", Effort: "high", Sandbox: "host"}
	st, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{
		Type:     contract.ArtifactDesign,
		Producer: "claude:planner",
		Summary:  "plan summary",
		Body:     "# Plan",
		Metadata: att.Metadata(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(st.Attestations, func(a contract.RoleAttestation) bool { return a.Role == "planner" })
	if i < 0 || st.Attestations[i].Model != "claude-opus" || st.Attestations[i].Effort != "high" {
		t.Fatalf("attestations = %+v", st.Attestations)
	}

	var plain IssueState
	RecordRoleAttestation(&plain, contract.RoleAttestation{Role: "developer", Agent: "codex", Harness: "codex", Model: "gpt"})
	raw, err := json.Marshal(plain.Attestations)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "effort") {
		t.Fatalf("attestation without effort changed serialization: %s", raw)
	}
}
