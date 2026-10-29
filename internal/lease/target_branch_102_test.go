package lease

import (
	"context"
	"encoding/json"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIssue102ApprovalSelectionAndBinding(t *testing.T) {
	for _, scenario := range []string{"default", "config", "override", "missing", "dirty"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			m, tr, issue := testManager(t)
			git := func(args ...string) string {
				t.Helper()
				out, err := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			initial := git("rev-parse", "HEAD")
			git("branch", "release/1.x")
			git("branch", "lts")
			cfgPath := filepath.Join(m.repoRoot, ".swarm/config.yaml")
			b, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "default" {
				if err := os.WriteFile(cfgPath, append([]byte("base_branch: release/1.x\n"), b...), 0600); err != nil {
					t.Fatal(err)
				}
				if scenario != "dirty" {
					git("add", ".swarm/config.yaml")
					git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "select release")
				}
			}
			if scenario == "override" || scenario == "missing" {
				branch := "lts"
				if scenario == "missing" {
					branch = "missing-102"
				}
				issue.Body = strings.Replace(issue.Body, "workflow: feature", "base_branch: "+branch+"\nworkflow: feature", 1)
				if _, err := tr.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
					t.Fatal(err)
				}
			}
			before, _, err := m.load(ctx, issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			rawBefore, _ := json.Marshal(before)
			st, err := m.ApproveTask(ctx, issue.Number, "local-dev")
			if scenario == "missing" {
				if err == nil || !strings.Contains(err.Error(), "missing-102") {
					t.Fatalf("missing accepted: %v", err)
				}
				after, _, _ := m.load(ctx, issue.Number)
				rawAfter, _ := json.Marshal(after)
				if string(rawBefore) != string(rawAfter) {
					t.Fatal("failed approval mutated state")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := ""
			if scenario == "config" {
				expected = "release/1.x"
			}
			if scenario == "override" {
				expected = "lts"
			}
			if st.BaseBranch != expected || st.BaseSHA != initial {
				t.Fatalf("selection: %s %s", st.BaseBranch, st.BaseSHA)
			}
			cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
			if err != nil {
				t.Fatal(err)
			}
			wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !TaskApprovalCurrent(st, wu, cfg) {
				t.Fatal("approval not current")
			}
			if expected != "" {
				if st.PolicyDigest == contract.DigestApprovedPolicy(st.BaseSHA, cfg) {
					t.Fatal("old binary accepts explicit target")
				}
				raw, err := json.Marshal(st)
				if err != nil {
					t.Fatal(err)
				}
				var legacy map[string]json.RawMessage
				if err := json.Unmarshal(raw, &legacy); err != nil {
					t.Fatal(err)
				}
				delete(legacy, "base_branch")
				raw, err = json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				var downgraded IssueState
				if err := json.Unmarshal(raw, &downgraded); err != nil {
					t.Fatal(err)
				}
				if TaskApprovalCurrent(&downgraded, wu, cfg) {
					t.Fatal("old decoder preserved explicit approval authority")
				}
				_, rev, err := m.load(ctx, issue.Number)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := m.save(ctx, issue.Number, rev, &downgraded); err != nil {
					t.Fatal(err)
				}
				before, _, err := m.load(ctx, issue.Number)
				if err != nil {
					t.Fatal(err)
				}
				beforeBytes, err := json.Marshal(before)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "all", time.Hour, 3); err == nil {
					t.Fatal("downgraded approval authorized mutation")
				}
				after, _, err := m.load(ctx, issue.Number)
				if err != nil {
					t.Fatal(err)
				}
				afterBytes, err := json.Marshal(after)
				if err != nil {
					t.Fatal(err)
				}
				if string(beforeBytes) != string(afterBytes) {
					t.Fatal("downgrade refusal changed state")
				}
				st.BaseBranch = ""
				if TaskApprovalCurrent(st, wu, cfg) {
					t.Fatal("clearing target accepted")
				}
			}
		})
	}
}

func TestIssue102FreshClaimKeepsRecordedBranch(t *testing.T) {
	ctx := context.Background()
	m, tr, issue := testManager(t)
	git := func(args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	base := git("rev-parse", "HEAD")
	git("branch", "release")
	issue.Body = strings.Replace(issue.Body, "gate: human", "gate: none", 1)
	issue.Body = strings.Replace(issue.Body, "workflow: feature", "base_branch: release\nworkflow: feature", 1)
	if _, err := tr.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
		t.Fatal(err)
	}
	st, err := m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "default advanced")
	wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, base)
	if err != nil {
		t.Fatal(err)
	}
	next, err := m.prepareClaimBase(ctx, issue.Number, st, wu, cfg)
	if err != nil || next.BaseSHA != base {
		t.Fatalf("repinned to default: %v", err)
	}
	git("update-ref", "-d", "refs/heads/release")
	if _, err := m.prepareClaimBase(ctx, issue.Number, st, wu, cfg); err == nil {
		t.Fatal("deleted target accepted")
	}
}

func TestIssue102SameSHATargetChangeClearsEvidence(t *testing.T) {
	ctx := context.Background()
	m, tr, issue := testManager(t)
	git := func(args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("branch", "release")
	git("branch", "lts")
	issue.Body = strings.Replace(issue.Body, "workflow: feature", "base_branch: release\nworkflow: feature", 1)
	if _, err := tr.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
		t.Fatal(err)
	}
	st, err := m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.BaseBranch != "release" || st.BaseSHA != git("rev-parse", "refs/heads/release") {
		t.Fatalf("initial approval has wrong target: %+v", st)
	}
	st, rev, err := m.load(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	st.Artifacts = []contract.Artifact{{Type: contract.ArtifactDesign, Body: "old plan"}, {Type: contract.ArtifactTestPlan, Body: "old tests"}, {Type: contract.ArtifactReview, Body: "APPROVED"}, {Type: contract.ArtifactImplementation, Body: "old implementation"}}
	if _, err := m.save(ctx, issue.Number, rev, st); err != nil {
		t.Fatal(err)
	}
	oldBase := st.BaseSHA
	issue.Body = strings.Replace(issue.Body, "base_branch: release", "base_branch: lts", 1)
	if _, err := tr.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
		t.Fatal(err)
	}
	st, err = m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.BaseSHA != oldBase || st.BaseBranch != "lts" || len(st.Artifacts) != 0 || st.ApprovalTransition != nil {
		t.Fatalf("carried evidence across target names: %+v", st)
	}
}

func TestIssue102PolicyFromTargetNotSelectionConfig(t *testing.T) {
	ctx := context.Background()
	m, _, issue := testManager(t)
	git := func(args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	defaultName := git("branch", "--show-current")
	git("checkout", "-b", "release")
	path := filepath.Join(m.repoRoot, ".swarm/config.yaml")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	releaseConfig := strings.Replace(string(original), "default_gate: agent", "default_gate: none", 1)
	if err := os.WriteFile(path, []byte("base_branch: nonexistent-redirect\n"+releaseConfig), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".swarm/config.yaml")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "release policy")
	release := git("rev-parse", "HEAD")
	git("checkout", defaultName)
	if err := os.WriteFile(path, append([]byte("base_branch: release\n"), original...), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".swarm/config.yaml")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "select release")
	st, err := m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.BaseSHA != release || st.BaseBranch != "release" {
		t.Fatalf("target config redirected approval: %+v", st)
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultGate != contract.GateNone || st.PolicyDigest != contract.DigestApprovedBranchPolicy(release, cfg, "release") {
		t.Fatal("policy did not come from selected SHA")
	}
}

func TestIssue102LegacyStateGolden(t *testing.T) {
	st := &IssueState{TaskDigest: "task", PolicyDigest: "policy", BaseSHA: "base"}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"plan_replan_count":0,"lease":{"version":0,"issue_number":0,"claim_id":"","owner":"","phase":"","status":"","claimed_at":"0001-01-01T00:00:00Z","expires_at":"0001-01-01T00:00:00Z","renewals":0,"max_renewals":0},"task_digest":"task","policy_digest":"policy","base_sha":"base","approved_at":"0001-01-01T00:00:00Z"}`
	if string(b) != want {
		t.Fatalf("legacy state bytes changed: %s", b)
	}
}

func TestIssue102MissingTrustedConfigRefusesCandidateSelection(t *testing.T) {
	ctx := context.Background()
	m, tr, issue := testManager(t)
	git := func(args ...string) {
		t.Helper()
		if out, err := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	path := filepath.Join(m.repoRoot, ".swarm/config.yaml")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	git("rm", ".swarm/config.yaml")
	git("-c", "user.name=T", "-c", "user.email=t@example.invalid", "commit", "-m", "missing trusted config")
	// The uncommitted candidate config is valid but cannot replace trusted bytes.
	if err := os.WriteFile(path, append([]byte("base_branch: attacker-target\n"), original...), 0600); err != nil {
		t.Fatal(err)
	}
	before, _, err := m.load(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	stateBefore, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	projectionBefore, err := tr.ListComments(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	issueBefore, err := tr.GetIssue(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApproveTask(ctx, issue.Number, "local-dev"); err == nil || !strings.Contains(err.Error(), "trusted default config") {
		t.Fatalf("candidate config accepted: %v", err)
	}
	after, _, err := m.load(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	stateAfter, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	projectionAfter, err := tr.ListComments(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	issueAfter, err := tr.GetIssue(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	beforeProjection, err := json.Marshal([]any{projectionBefore, issueBefore})
	if err != nil {
		t.Fatal(err)
	}
	afterProjection, err := json.Marshal([]any{projectionAfter, issueAfter})
	if err != nil {
		t.Fatal(err)
	}
	if string(stateBefore) != string(stateAfter) || string(beforeProjection) != string(afterProjection) {
		t.Fatal("refused approval changed state or projections")
	}
}
