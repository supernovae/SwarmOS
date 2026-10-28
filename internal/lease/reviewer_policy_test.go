package lease

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
)

// reviewerPolicyManager commits a policy with the given security additions and
// returns a manager for local-dev holding a planned, claimed issue.
func reviewerPolicyManager(t *testing.T, security string) (*Manager, *tracker.LocalTracker, *tracker.Issue, string) {
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
	mustWrite(".swarm/config.yaml", "version: 1\ndefault_agent: claude\ndefault_gate: agent\nlease:\n  duration: 4h\n  renewable: true\n  max_renewals: 3\nsecurity:\n  require_ready_label: true\n  ready_label: swarm:ready\n"+security+"agents:\n  claude:\n    binary: claude\n  codex:\n    binary: codex\n    roles: [reviewer]\n")
	mustWrite(".swarm/workflows/feature.yaml", "name: feature\ndefault_gate: agent\nsteps:\n  - name: design\n    role: planner\n    artifact: design\n")
	for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture"}} {
		cmd := gitfixture.Command(t, "", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	ctx := context.Background()
	lt := tracker.NewLocalTracker(root).WithUser("local-dev", "OWNER")
	iss, err := lt.CreateIssue(ctx, "test", testWorkUnit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(lt, contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
	if err := m.ConfigureStore(root); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "claude", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	planner := map[string]string{"agent": "claude", "harness": "claude", "model": "default"}
	for _, art := range []contract.Artifact{
		{Type: contract.ArtifactDesign, Producer: "claude:planner", Summary: "plan", Body: "plan", Metadata: planner},
		{Type: contract.ArtifactTestPlan, Producer: "claude:planner", Summary: "tests", Body: "tests", Metadata: planner},
	} {
		if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", art); err != nil {
			t.Fatal(err)
		}
	}
	return m, lt, iss, root
}

func agentReview(t *testing.T, m *Manager, n int, agent, model string) contract.Artifact {
	t.Helper()
	st, err := m.GetCurrentState(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	meta := ReviewBindings(st)
	meta["kind"], meta["agent"], meta["harness"], meta["model"] = "agent", agent, agent, model
	return contract.Artifact{Type: contract.ArtifactReview, Producer: agent + ":reviewer", Verdict: "APPROVED", Summary: "review", Body: "VERDICT: APPROVED\n", Metadata: meta}
}

func TestReviewerAgentsPinWhichAgentSatisfiesAgentGate(t *testing.T) {
	t.Parallel()
	m, _, iss, root := reviewerPolicyManager(t, "  reviewer_agents: [codex]\n")
	ctx := context.Background()

	// A candidate checkout that widens the allowlist has no effect: policy loads from base_sha.
	cfgPath := filepath.Join(root, ".swarm", "config.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(data), "reviewer_agents: [codex]", "reviewer_agents: [codex, claude]", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", agentReview(t, m, iss.Number, "claude", "other")); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("unpinned reviewer agent satisfied gate: %v", err)
	}
	st, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", agentReview(t, m, iss.Number, "codex", "default"))
	if err != nil {
		t.Fatalf("pinned reviewer agent rejected: %v", err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	st.EffectiveGate = contract.GateAgent
	if err := VerifyApproval(st, wu); err != nil {
		t.Fatalf("pinned agent review did not satisfy gate: %v", err)
	}
}

func TestReviewerAgentsUnsetKeepsAnyConfiguredReviewer(t *testing.T) {
	t.Parallel()
	m, _, iss, _ := reviewerPolicyManager(t, "")
	if _, err := m.UpdateArtifact(context.Background(), iss.Number, "local-dev", agentReview(t, m, iss.Number, "claude", "default")); err != nil {
		t.Fatalf("default policy rejected same-agent review: %v", err)
	}
}

func TestIndependentReviewRejectsPlannerAsAgentReviewer(t *testing.T) {
	t.Parallel()
	m, _, iss, _ := reviewerPolicyManager(t, "  independent_review: true\n")
	ctx := context.Background()
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", agentReview(t, m, iss.Number, "claude", "default")); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("planner harness and model reviewed its own plan: %v", err)
	}
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", agentReview(t, m, iss.Number, "claude", "a-different-model")); err != nil {
		t.Fatalf("different model rejected: %v", err)
	}
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", agentReview(t, m, iss.Number, "codex", "default")); err != nil {
		t.Fatalf("different harness rejected: %v", err)
	}
}

func TestIndependentReviewRejectsClaimOwnerHumanApproval(t *testing.T) {
	t.Parallel()
	m, lt, iss, _ := reviewerPolicyManager(t, "  independent_review: true\n")
	ctx := context.Background()
	if _, err := m.RecordHumanReview(ctx, iss.Number, "APPROVED", "self review"); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("claim owner approved their own plan: %v", err)
	}
	m.tracker = lt.WithUser("maintainer", "OWNER")
	if _, err := m.RecordHumanReview(ctx, iss.Number, "APPROVED", "independent review"); err != nil {
		t.Fatalf("independent maintainer rejected: %v", err)
	}
}

func TestClaimOwnerMayApproveWithoutIndependentReview(t *testing.T) {
	t.Parallel()
	m, _, iss, _ := reviewerPolicyManager(t, "")
	if _, err := m.RecordHumanReview(context.Background(), iss.Number, "APPROVED", "solo maintainer"); err != nil {
		t.Fatalf("default policy blocked single-maintainer approval: %v", err)
	}
}

func TestNotifyPlanReviewersAssignsAndMentionsOncePerPlan(t *testing.T) {
	t.Parallel()
	m, lt, iss, _ := reviewerPolicyManager(t, "  reviewer_collaborators: [alice, local-dev]\n  independent_review: true\n")
	ctx := context.Background()
	marker := "<!-- swarm:plan-review-request:v1"
	// A forged marker from someone else must not suppress the real request.
	if _, err := lt.WithUser("attacker", "NONE").CreateComment(ctx, iss.Number, marker+" plan=x -->"); err != nil {
		t.Fatal(err)
	}
	countRequests := func() int {
		comments, err := lt.ListComments(ctx, iss.Number)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, c := range comments {
			if c.Author == "local-dev" && strings.Contains(c.Body, marker) {
				if !strings.Contains(c.Body, "@alice") || strings.Contains(c.Body, "@local-dev") {
					t.Fatalf("request mentions wrong reviewers: %q", c.Body)
				}
				n++
			}
		}
		return n
	}
	reviewers, err := m.NotifyPlanReviewers(ctx, iss.Number)
	if err != nil || len(reviewers) != 1 || reviewers[0] != "alice" {
		t.Fatalf("reviewers=%v err=%v", reviewers, err)
	}
	got, err := lt.GetIssue(ctx, iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Assignees) != 1 || got.Assignees[0] != "alice" {
		t.Fatalf("assignees=%v", got.Assignees)
	}
	if _, err := m.NotifyPlanReviewers(ctx, iss.Number); err != nil {
		t.Fatal(err)
	}
	if n := countRequests(); n != 1 {
		t.Fatalf("expected one review request for the same plan, got %d", n)
	}
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "claude:planner", Summary: "plan", Body: "revised plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.NotifyPlanReviewers(ctx, iss.Number); err != nil {
		t.Fatal(err)
	}
	if n := countRequests(); n != 2 {
		t.Fatalf("expected a new review request after the plan changed, got %d", n)
	}
}

func TestNotifyPlanReviewersIsQuietWithoutNamedReviewers(t *testing.T) {
	t.Parallel()
	m, lt, iss, _ := reviewerPolicyManager(t, "")
	reviewers, err := m.NotifyPlanReviewers(context.Background(), iss.Number)
	if err != nil || len(reviewers) != 0 {
		t.Fatalf("reviewers=%v err=%v", reviewers, err)
	}
	comments, err := lt.ListComments(context.Background(), iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range comments {
		if strings.Contains(c.Body, "swarm:plan-review-request") {
			t.Fatal("posted a review request with no reviewers configured")
		}
	}
}
