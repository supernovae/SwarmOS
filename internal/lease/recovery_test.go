package lease

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestApprovalDropsContainedCheckpoint(t *testing.T) {
	for _, scenario := range []string{"contained", "intermediate", "unique", "missing", "policy changed", "linked PR", "fork", "pending divergent base"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			m, _, issue := testManager(t)
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = m.repoRoot
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
				t.Fatal(err)
			}
			for _, a := range []contract.Artifact{{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "plan"}, {Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "tests"}} {
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
			old := st.BaseSHA
			branch := workspace.BranchName(issue.Number, issue.Title)
			st.HeadSHA = old
			st.Branch = branch
			if scenario == "intermediate" {
				git("-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "contained checkpoint")
				st.HeadSHA = git("rev-parse", "HEAD")
			}
			if scenario == "unique" {
				git("checkout", "-b", "unique")
				git("-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "unique")
				st.HeadSHA = git("rev-parse", "HEAD")
				git("checkout", "-")
			}
			if scenario == "missing" {
				st.HeadSHA = strings.Repeat("1", 40)
			}
			if scenario == "linked PR" {
				st.PRNumber = 7
			}
			if scenario == "fork" {
				st.ForkRepo = "owner/fork"
			}
			st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactImplementation, Body: "old"}, contract.Artifact{Type: contract.ArtifactValidation, Body: "old"})
			if _, err := m.save(ctx, issue.Number, rev, st); err != nil {
				t.Fatal(err)
			}
			if scenario == "policy changed" {
				p := filepath.Join(m.repoRoot, ".swarm/rules/new.md")
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("new policy"), 0644); err != nil {
					t.Fatal(err)
				}
				git("add", p)
			}
			git("-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "new base")
			result, err := m.ApproveTask(ctx, issue.Number, "local-dev")
			if err != nil {
				t.Fatal(err)
			}
			drop := scenario == "intermediate" || scenario == "contained" || scenario == "policy changed" || scenario == "pending divergent base"
			if !drop {
				if result.DroppedCheckpoint != nil || result.HeadSHA != st.HeadSHA || result.Branch != branch {
					t.Fatal("lost unique or excluded checkpoint")
				}
				if scenario == "unique" {
					if _, _, err := workspace.ProvisionAt(m.repoRoot, issue.Number, issue.Title, result.BaseSHA, result.Branch, result.HeadSHA); err == nil || !strings.Contains(err.Error(), "reconcile the branch without resetting it") {
						t.Fatalf("unique checkpoint was not refused: %v", err)
					}
				}
				return
			}
			if result.HeadSHA != "" || result.Branch != "" || result.DroppedCheckpoint == nil || result.DroppedCheckpoint.HeadSHA != st.HeadSHA || !result.CheckpointDropped {
				t.Fatalf("checkpoint not retired: %+v", result)
			}
			if scenario == "policy changed" {
				if reason, superseded := SupersededReview(result); len(result.Artifacts) != 3 || !superseded || reason != "policy files changed" {
					t.Fatalf("policy change did not keep the plan for human re-review: %v", result.Artifacts)
				}
			} else {
				if len(result.Artifacts) != 3 {
					t.Fatalf("plan evidence not retained: %v", result.Artifacts)
				}
			}
			again, err := m.ApproveTask(ctx, issue.Number, "local-dev")
			if err != nil {
				t.Fatal(err)
			}
			if again.CheckpointDropped || again.DroppedCheckpoint == nil {
				t.Fatal("repeat approval lost identity or reported a new drop")
			}
			if scenario == "pending divergent base" {
				git("checkout", "--orphan", "unrelated")
				git("-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "unrelated")
				restored, err := m.ApproveTask(ctx, issue.Number, "local-dev")
				if err != nil {
					t.Fatal(err)
				}
				if restored.HeadSHA != old || restored.Branch != branch || restored.DroppedCheckpoint != nil {
					t.Fatal("pending checkpoint not restored")
				}
			}
		})
	}
}
