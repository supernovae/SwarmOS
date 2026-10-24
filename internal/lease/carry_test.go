package lease

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestApprovalCarryForward(t *testing.T) {
	for _, tc := range []string{"descendant", "policy edit", "WorkUnit edit", "nonancestor", "legacy review", "invalid review", "repeated carry", "same base", "same-base WorkUnit edit", "missing base", "policy paths unavailable"} {
		t.Run(tc, func(t *testing.T) {
			ctx := context.Background()
			m, lt, issue := testManager(t)
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
			original, err := m.RecordHumanReview(ctx, issue.Number, "APPROVED", "reviewed")
			if err != nil {
				t.Fatal(err)
			}
			origin := original.BaseSHA
			if _, err := m.Release(ctx, issue.Number, "local-dev", "test"); err != nil {
				t.Fatal(err)
			}
			st, rev, err := m.load(ctx, issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "legacy review":
				delete(st.Artifacts[2].Metadata, "policy_tree_digest")
			case "invalid review":
				st.Artifacts[2].Body += "tampered"
			}
			if tc == "missing base" || tc == "policy paths unavailable" {
				st.BaseSHA = strings.Repeat("0", 40)
			}
			st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactImplementation, Body: "old implementation"}, contract.Artifact{Type: contract.ArtifactValidation, Body: "old validation"})
			st.Attestations = nil
			for _, a := range st.Artifacts {
				st.Attestations = append(st.Attestations, contract.RoleAttestation{ArtifactType: string(a.Type), ArtifactDigest: hash(a.Body)})
			}
			if _, err := m.save(ctx, issue.Number, rev, st); err != nil {
				t.Fatal(err)
			}
			if tc == "nonancestor" {
				git("checkout", "--orphan", "sibling")
			}
			name := "source.txt"
			if tc == "policy edit" || tc == "policy paths unavailable" {
				name = ".swarm/rules/new.md"
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(m.repoRoot, name)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(m.repoRoot, name), []byte("new"), 0600); err != nil {
				t.Fatal(err)
			}
			if tc != "same base" && tc != "same-base WorkUnit edit" {
				git("add", name)
				git("-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "new base")
			}
			if tc == "WorkUnit edit" || tc == "same-base WorkUnit edit" {
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
			if tc == "same base" {
				if len(st.Artifacts) != 5 || len(st.Attestations) != 5 || VerifyApproval(st, wu) != nil {
					t.Fatal("unchanged reapproval discarded evidence")
				}
				return
			}
			carry := tc == "descendant" || tc == "repeated carry"
			if !carry {
				if tc == "policy paths unavailable" && !strings.Contains(st.CarryReason, "paths unavailable") {
					t.Fatalf("missing path lookup failure explanation: %q", st.CarryReason)
				}
				if len(st.Artifacts) != 0 || len(st.Attestations) != 0 || st.ApprovalTransition != nil {
					t.Fatalf("ineligible evidence retained: %+v", st)
				}
				if !errors.Is(VerifyApproval(st, wu), ErrApprovalRequired) {
					t.Fatal("ineligible review accepted")
				}
				return
			}
			if len(st.Artifacts) != 3 || len(st.Attestations) != 3 || VerifyApproval(st, wu) != nil || st.Artifacts[2].Metadata["base_sha"] != origin {
				t.Fatalf("carry lost approval or rewrote origin: %+v", st)
			}
			if tc == "repeated carry" {
				git("-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "another base")
				st, err = m.ApproveTask(ctx, issue.Number, "local-dev")
				if err != nil || VerifyApproval(st, wu) != nil || st.ApprovalTransition.OriginBase != origin {
					t.Fatalf("repeated carry: %v", err)
				}
			}
			successor, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			m.BindClaim(issue.Number, original.Lease.ClaimID)
			for _, mutate := range []func() error{
				func() error {
					_, e := m.UpdateArtifact(ctx, issue.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Body: "stale"})
					return e
				},
				func() error {
					_, e := m.RecordCheckpoint(ctx, issue.Number, "local-dev", workspace.BranchName(issue.Number, "test"), "head")
					return e
				},
				func() error {
					_, e := m.RecordPRHead(ctx, issue.Number, "local-dev", 37, "swarm/pr-37", "head")
					return e
				},
				func() error { _, e := m.RecordSubmission(ctx, issue.Number, "local-dev", 37, "local://37"); return e },
			} {
				if err := mutate(); !errors.Is(err, ErrClaimFence) {
					t.Fatalf("stale worker after baseline move: %v", err)
				}
			}
			m.BindClaim(issue.Number, successor.Lease.ClaimID)
			for _, field := range []string{"origin", "target", "policy", "tree", "task", "review", "body", "plan", "test", "source", "kind"} {
				t.Run(field, func(t *testing.T) {
					raw, _ := json.Marshal(st)
					var copy IssueState
					if err := json.Unmarshal(raw, &copy); err != nil {
						t.Fatal(err)
					}
					switch field {
					case "origin":
						copy.ApprovalTransition.OriginBase = "wrong"
					case "target":
						copy.ApprovalTransition.TargetBase = "wrong"
					case "policy":
						copy.PolicyDigest = "wrong"
					case "tree":
						copy.PolicyTreeDigest = "wrong"
					case "task":
						copy.TaskDigest = "wrong"
					case "review":
						copy.ApprovalTransition.ReviewDigest = "wrong"
					case "body":
						copy.Artifacts[2].Body += "wrong"
					case "plan":
						copy.Artifacts[0].Body += "wrong"
					case "test":
						copy.Artifacts[1].Body += "wrong"
					case "source":
						copy.Artifacts[2].Metadata["review_source"] = "contributor"
					case "kind":
						copy.Artifacts[2].Metadata["kind"] = "agent"
					}
					if !errors.Is(VerifyApproval(&copy, wu), ErrApprovalRequired) {
						t.Fatal("tampered carry accepted")
					}
				})
			}
		})
	}
}
