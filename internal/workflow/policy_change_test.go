package workflow

import (
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workspace"
)

// TestPolicyChangeKeepsHumanPlan re-approves a human-gated issue on a base
// whose policy changed: work pauses for a fresh human review of the kept plan
// instead of failing, then implements on the new base.
func TestPolicyChangeKeepsHumanPlan(t *testing.T) {
	e, _, issue, spy := splitEngine(t, contract.GateHuman)
	ctx := t.Context()
	m := e.LeaseManager
	opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "plan", Headless: true}
	if _, err := e.ExecuteWork(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RecordHumanReview(ctx, issue.Number, "APPROVED", "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, issue.Number, "maintainer", "test"); err != nil {
		t.Fatal(err)
	}
	before, err := m.GetCurrentState(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	var plan string
	for _, a := range before.Artifacts {
		if a.Type == contract.ArtifactDesign {
			plan = a.Body
		}
	}

	path := filepath.Join(e.repoRoot, ".swarm", "rules", "new.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "policy change"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", e.repoRoot}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	st, err := m.ApproveTask(ctx, issue.Number, "maintainer")
	if err != nil {
		t.Fatal(err)
	}
	newBase := st.BaseSHA
	if newBase == before.BaseSHA || st.CarryReason != "policy files changed" {
		t.Fatalf("re-approval did not move to the policy change: %+v", st)
	}
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactDesign && a.Body != plan {
			t.Fatal("re-approval changed the kept plan")
		}
	}
	spy.take()

	opts.Phase = "all"
	res, err := e.ExecuteWork(ctx, opts)
	if err != nil {
		t.Fatalf("kept plan failed instead of pausing: %v", err)
	}
	if roles := spy.take(); !res.PausedForHuman || slices.Contains(roles, "planner") || slices.Contains(roles, "developer") {
		t.Fatalf("expected pause at human gate: %+v roles=%v", res, roles)
	}
	if _, err := m.Release(ctx, issue.Number, "maintainer", "test"); err != nil {
		t.Fatal(err)
	}

	st, err = m.RecordHumanReview(ctx, issue.Number, "APPROVED", "re-reviewed under new policy")
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactReview {
			md := a.Metadata
			if md["base_sha"] != newBase || md["policy_digest"] != st.PolicyDigest || md["policy_tree_digest"] != st.PolicyTreeDigest || md["superseded_by"] != "" {
				t.Fatalf("fresh review not bound to new policy: %v", md)
			}
		}
	}
	if err := lease.VerifyApproval(st, wu); err != nil {
		t.Fatalf("fresh human review rejected: %v", err)
	}

	res, err = e.ExecuteWork(ctx, opts)
	if err != nil {
		t.Fatalf("implementation after re-review: %v", err)
	}
	if roles := spy.take(); res.PausedForHuman || !slices.Contains(roles, "developer") || !slices.Contains(roles, "tester") {
		t.Fatalf("implementation did not run: %+v roles=%v", res, roles)
	}
	got, err := os.ReadFile(filepath.Join(workspace.WorktreePath(e.repoRoot, issue.Number), ".swarm", "active", "PLAN.md"))
	if err != nil || strings.TrimSpace(string(got)) != strings.TrimSpace(plan) {
		t.Fatalf("kept plan not used for implementation: %v\n%s", err, got)
	}
	st, err = m.GetCurrentState(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	if st.BaseSHA != newBase {
		t.Fatalf("implemented on %s, want %s", st.BaseSHA, newBase)
	}
	if err := lease.VerifyApproval(st, wu); err != nil {
		t.Fatalf("approval lost after implementation: %v", err)
	}
}
