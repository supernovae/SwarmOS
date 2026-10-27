package lease

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestHumanPlanKeptOnPolicyChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keep   bool
		reason string
	}{
		{"kept", true, "policy files changed"},
		{"agent verdict", false, "policy files changed"},
		{"nonancestor", false, "base not an ancestor"},
		{"WorkUnit edit", false, "WorkUnit changed"},
		{"gate changes to agent", false, "policy files changed"},
		{"linked PR", false, "policy files changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, lt, issue := testManager(t)
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.test"}, args...)...)
				cmd.Dir = m.repoRoot
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			commit := func(name, body string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(filepath.Join(m.repoRoot, name)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(m.repoRoot, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				git("add", name)
				git("commit", "-m", "change "+name)
			}
			workflow := ".swarm/workflows/feature.yaml"
			if tc.name == "gate changes to agent" {
				// The human gate comes from the workflow, so the new policy can lower it.
				commit(workflow, "name: feature\ndefault_gate: human\nsteps:\n  - name: design\n    role: planner\n    artifact: design\n")
				issue.Body = strings.Replace(issue.Body, "gate: human\n", "", 1)
				if _, err := lt.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
					t.Fatal(err)
				}
				if st, err := m.ApproveTask(ctx, issue.Number, "local-dev"); err != nil || st.EffectiveGate != contract.GateHuman {
					t.Fatalf("workflow human gate: %v", err)
				}
			}
			if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
				t.Fatal(err)
			}
			for _, a := range []contract.Artifact{{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "plan body"}, {Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "test body"}} {
				if _, err := m.UpdateArtifact(ctx, issue.Number, "local-dev", a); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.RecordHumanReview(ctx, issue.Number, "APPROVED", "reviewed"); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Release(ctx, issue.Number, "local-dev", "test"); err != nil {
				t.Fatal(err)
			}
			st, rev, err := m.load(ctx, issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "agent verdict":
				st.Artifacts[2].Metadata["kind"] = "agent"
				st.Artifacts[2].Metadata["review_source"] = "authenticated-agent"
			case "linked PR":
				st.PRNumber = 37
			}
			st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactImplementation, Body: "old implementation"}, contract.Artifact{Type: contract.ArtifactValidation, Body: "old validation"})
			if _, err := m.save(ctx, issue.Number, rev, st); err != nil {
				t.Fatal(err)
			}
			if tc.name == "nonancestor" {
				git("checkout", "--orphan", "sibling")
			}
			if tc.name == "gate changes to agent" {
				commit(workflow, "name: feature\ndefault_gate: agent\nsteps:\n  - name: design\n    role: planner\n    artifact: design\n")
			} else {
				commit(".swarm/rules/new.md", "new rule\n")
			}
			if tc.name == "WorkUnit edit" {
				issue.Body = strings.Replace(issue.Body, "Harden coordination state", "Changed task", 1)
				if _, err := lt.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
					t.Fatal(err)
				}
			}
			st, err = m.ApproveTask(ctx, issue.Number, "local-dev")
			if err != nil {
				t.Fatal(err)
			}
			wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
			if err != nil {
				t.Fatal(err)
			}
			if st.CarryReason != tc.reason {
				t.Fatalf("carry reason = %q, want %q", st.CarryReason, tc.reason)
			}
			if !errors.Is(VerifyApproval(st, wu), ErrApprovalRequired) {
				t.Fatal("stale approval accepted on the new policy")
			}
			if _, superseded := SupersededReview(st); superseded != tc.keep {
				t.Fatalf("superseded = %t, want %t", superseded, tc.keep)
			}
			if tc.name == "linked PR" {
				if len(st.Artifacts) != 3 {
					t.Fatalf("PR branch changed: %+v", st.Artifacts)
				}
				return
			}
			if !tc.keep {
				if len(st.Artifacts) != 0 || len(st.Attestations) != 0 || st.ApprovalTransition != nil {
					t.Fatalf("ineligible evidence retained: %+v", st)
				}
				return
			}

			if len(st.Artifacts) != 3 || st.Artifacts[0].Body != "plan body" || st.Artifacts[1].Body != "test body" || st.Artifacts[2].Type != contract.ArtifactReview {
				t.Fatalf("plan not kept byte-for-byte: %+v", st.Artifacts)
			}
			if st.Artifacts[2].Metadata["superseded_by"] != "policy files changed" || st.ApprovalTransition != nil {
				t.Fatalf("review not superseded: %+v", st.Artifacts[2].Metadata)
			}
			if !slices.Contains(st.ChangedPolicyPaths, ".swarm/rules/new.md") {
				t.Fatalf("changed paths = %v", st.ChangedPolicyPaths)
			}
			var human *HumanPlanApprovalError
			if !errors.As(VerifyApproval(st, wu), &human) {
				t.Fatal("kept plan did not ask for a human review")
			}
			rebound := func(mutate func(md map[string]string)) error {
				raw, _ := json.Marshal(st)
				var copy IssueState
				if err := json.Unmarshal(raw, &copy); err != nil {
					t.Fatal(err)
				}
				md := copy.Artifacts[2].Metadata
				md["base_sha"], md["policy_digest"], md["policy_tree_digest"] = copy.BaseSHA, copy.PolicyDigest, copy.PolicyTreeDigest
				mutate(md)
				return VerifyApproval(&copy, wu)
			}
			if err := rebound(func(map[string]string) {}); !errors.Is(err, ErrApprovalRequired) {
				t.Fatal("superseded marker alone did not reject a rebound review")
			}
			if err := rebound(func(md map[string]string) { delete(md, "superseded_by") }); err != nil {
				t.Fatalf("control: rebound review without marker: %v", err)
			}
			if err := rebound(func(md map[string]string) {
				delete(md, "superseded_by")
				md["kind"], md["review_source"] = "agent", "authenticated-agent"
			}); !errors.Is(err, ErrApprovalRequired) {
				t.Fatal("agent verdict satisfied the human gate")
			}

			st, err = m.RecordHumanReview(ctx, issue.Number, "APPROVED", "re-reviewed")
			if err != nil {
				t.Fatal(err)
			}
			if _, superseded := SupersededReview(st); superseded || st.CarryReason != "" || VerifyApproval(st, wu) != nil {
				t.Fatalf("fresh human review did not satisfy the gate: %+v", st)
			}
		})
	}
}
