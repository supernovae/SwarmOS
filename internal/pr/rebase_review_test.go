package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workflow"
)

func revisionState(t *testing.T, s *Service, issue int, edit func(*lease.IssueState)) {
	t.Helper()
	b, rev, err := s.Manager.Store().Load(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	var st lease.IssueState
	if err = json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	edit(&st)
	b, err = json.Marshal(&st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Manager.Store().CompareAndSwap(context.Background(), issue, rev, b); err != nil {
		t.Fatal(err)
	}
}

func revisionGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRebaseReviewRefusalPreservesApprovalAndRevision(t *testing.T) {
	for _, gate := range []contract.GateMode{contract.GateAgent, contract.GateHuman} {
		t.Run(string(gate), func(t *testing.T) {
			s, issue, number, base := fixture(t)
			ctx := context.Background()
			if _, err := s.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Manager.Release(ctx, issue, "alice", "review"); err != nil {
				t.Fatal(err)
			}
			revisionState(t, s, issue, func(st *lease.IssueState) { st.EffectiveGate = gate })
			revisionGit(t, s.Root, "commit", "--allow-empty", "-qm", "target")
			before, _, err := s.Manager.Store().Load(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			pr, err := s.Tracker.GetPullRequest(ctx, number)
			if err != nil {
				t.Fatal(err)
			}
			branchHead := revisionGit(t, s.Root, "rev-parse", pr.Head)
			_, err = s.Rebase(ctx, Options{Number: number, Resolve: true, Work: workflow.WorkOptions{AgentName: "mock"}})
			if err == nil {
				t.Fatal("review refusal missing")
			}
			if gate == contract.GateAgent && !strings.Contains(err.Error(), fmt.Sprintf("swarm pr work %d --phase review", number)) {
				t.Fatal(err)
			}
			if gate == contract.GateHuman {
				var human *lease.HumanPlanApprovalError
				if !errors.As(err, &human) {
					t.Fatal(err)
				}
			}
			after, _, err := s.Manager.Store().Load(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("review refusal changed serialized issue/revision state")
			}
			latest, err := s.Tracker.GetPullRequest(ctx, number)
			if err != nil {
				t.Fatal(err)
			}
			if latest.HeadSHA != pr.HeadSHA || revisionGit(t, s.Root, "rev-parse", pr.Head) != branchHead || branchHead != base {
				t.Fatal("refusal moved PR head")
			}
		})
	}
}

func TestRebaseReviewHintsUsePRCommands(t *testing.T) {
	s, issue, number, _ := fixture(t)
	r, err := s.Status(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	r.State.EffectiveGate = contract.GateAgent
	iss, err := s.Tracker.GetIssue(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
	if err != nil {
		t.Fatal(err)
	}
	rv := &resolver{unit: wu}
	for _, review := range []bool{false, true} {
		var e error
		if review {
			_, e = s.reviewRebasePlan(context.Background(), r, "alice", r.State, Options{}, rv)
		} else {
			e = s.preflightRebaseReview(context.Background(), r, Options{}, rv)
		}
		if e == nil || !strings.Contains(e.Error(), fmt.Sprintf("swarm pr work %d --phase review", number)) || strings.Contains(e.Error(), "issue review-plan") {
			t.Fatalf("hint: %v", e)
		}
	}
}

func TestRebaseRoutineReapprovalCarry(t *testing.T) {
	for _, kind := range []string{"agent", "human", "legacy", "policy moved", "reviewer unavailable"} {
		t.Run(kind, func(t *testing.T) {
			s, issue, number, base := fixture(t)
			ctx := context.Background()
			if _, err := s.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan", Gate: contract.GateAgent, Headless: true}}); err != nil {
				t.Fatal(err)
			}
			if kind == "human" {
				if _, err := s.Manager.RecordHumanReview(ctx, issue, "APPROVED", "reviewed"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "review", Gate: contract.GateAgent, Headless: true}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Manager.Release(ctx, issue, "alice", "advance"); err != nil {
				t.Fatal(err)
			}
			if kind == "legacy" {
				revisionState(t, s, issue, func(st *lease.IssueState) {
					for i := range st.Artifacts {
						if st.Artifacts[i].Type == contract.ArtifactReview {
							delete(st.Artifacts[i].Metadata, "policy_tree_digest")
						}
					}
				})
			}
			iss, err := s.Tracker.GetIssue(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			wu, err := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
			if err != nil {
				t.Fatal(err)
			}
			original, err := s.Manager.GetCurrentState(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			if err = lease.VerifyApproval(original, wu); err != nil {
				t.Fatalf("original approval: %v", err)
			}
			if kind == "policy moved" || kind == "reviewer unavailable" {
				if err := os.WriteFile(filepath.Join(s.Root, ".swarm/project.md"), []byte("# Updated project\n"), 0600); err != nil {
					t.Fatal(err)
				}
				revisionGit(t, s.Root, "add", ".swarm/project.md")
				revisionGit(t, s.Root, "commit", "-qm", "policy")
				if _, err := s.Manager.ApproveTask(ctx, issue, "alice"); err != nil {
					t.Fatal(err)
				}
				revisionState(t, s, issue, func(st *lease.IssueState) { st.EffectiveGate = contract.GateAgent })
				pending, e := s.Manager.GetCurrentState(ctx, issue)
				if e != nil || !policyReviewNeeded(pending, wu) {
					t.Fatalf("policy review setup: %+v %v", pending, e)
				}

			}
			revisionGit(t, s.Root, "commit", "--allow-empty", "-qm", "target")
			pr, err := s.Tracker.GetPullRequest(ctx, number)
			if err != nil {
				t.Fatal(err)
			}
			before, _, err := s.Manager.Store().Load(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "policy moved" || kind == "reviewer unavailable" {
				opts := Options{Number: number, Resolve: true, Work: workflow.WorkOptions{AgentName: "mock"}}
				if kind == "reviewer unavailable" {
					opts.Work.ReviewerAgent = "unknown-reviewer"
				}
				r, e := s.Status(ctx, number)
				if e != nil {
					t.Fatal(e)
				}
				r.BaseSHA = strings.Repeat("0", 40)
				if e = s.preflightRebaseReview(ctx, r, opts, &resolver{unit: wu}); e == nil {
					t.Fatal("configuration failure accepted")
				}
				_, err = s.Rebase(ctx, opts)
			} else {
				_, err = s.Manager.ReapproveRebase(ctx, issue, number, base, pr.BaseSHA, pr.HeadSHA)
			}
			if kind == "legacy" || kind == "policy moved" || kind == "reviewer unavailable" {
				if err == nil {
					t.Fatal("carry refusal missing")
				}
				if kind != "reviewer unavailable" && !strings.Contains(err.Error(), fmt.Sprintf("swarm pr work %d --phase review", number)) {
					t.Fatal(err)
				}
				after, _, e := s.Manager.Store().Load(ctx, issue)
				if e != nil {
					t.Fatal(e)
				}
				if string(before) != string(after) {
					t.Fatal("carry refusal persisted candidate")
				}
				if revisionGit(t, s.Root, "rev-parse", pr.Head) != base {
					t.Fatal("carry refusal rewrote branch")
				}
				if kind == "legacy" {
					_, e = s.Rebase(ctx, Options{Number: number, Resolve: true, Work: workflow.WorkOptions{AgentName: "mock"}})
					if e == nil {
						t.Fatal("service accepted invalid carried approval")
					}
					after, _, e = s.Manager.Store().Load(ctx, issue)
					if e != nil {
						t.Fatal(e)
					}
					if string(before) != string(after) {
						t.Fatal("service persisted invalid carried approval")
					}
				}

			} else {
				if err != nil {
					t.Fatal(err)
				}
				st, e := s.Manager.GetCurrentState(ctx, issue)
				if e != nil {
					t.Fatal(e)
				}
				if st.BaseSHA != pr.BaseSHA || lease.VerifyApproval(st, wu) != nil {
					t.Fatal("carried approval invalid")
				}
			}
		})
	}
}
