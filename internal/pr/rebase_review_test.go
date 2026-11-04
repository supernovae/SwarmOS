package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput()
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

func recoveryFixture(t *testing.T, hosted, offbase bool) (*Service, int, int, *Result) {
	t.Helper()
	s, issue, number, _ := fixture(t)
	ctx := context.Background()
	opts := Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan", Gate: contract.GateAgent, Headless: true}}
	r, err := s.Work(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Worktree, "patch.txt"), []byte("pending patch"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Worktree, "odd path\n.txt"), []byte("odd"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Worktree, "AGENTS.md"), []byte("# Contributor instructions\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("patch.txt", filepath.Join(r.Worktree, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	revisionGit(t, r.Worktree, "add", "patch.txt", "odd path\n.txt", "tracked-link", "AGENTS.md")
	revisionGit(t, r.Worktree, "commit", "-qm", "pending patch")
	head := revisionGit(t, r.Worktree, "rev-parse", "HEAD")
	st, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Manager.RecordPRHead(ctx, issue, "alice", number, st.PRRevision.Branch, head); err != nil {
		t.Fatal(err)
	}
	opts.Work.Phase = "review"
	if _, err = s.Work(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Manager.Release(ctx, issue, "alice", "advance"); err != nil {
		t.Fatal(err)
	}
	if offbase {
		revisionGit(t, s.Root, "commit", "--allow-empty", "-qm", "advanced base")
		iss, e := s.Tracker.GetIssue(ctx, issue)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Tracker.UpdateIssueBody(ctx, issue, strings.Replace(strings.Replace(iss.Body, "Revise linked PR", "Revise linked PR with new objective", 1), "gate: none", "gate: agent", 1)); e != nil {
			t.Fatal(e)
		}
		st, e = s.Manager.ApproveTask(ctx, issue, "alice")
		if e != nil {
			t.Fatal(e)
		}
		if len(st.Artifacts) != 0 || st.CompletePlan != nil || st.CarryReason != "WorkUnit changed" || st.PRRevision.LocalHeadSHA != head {
			t.Fatalf("invalidation: %+v", st)
		}
	}
	if hosted {
		pr, e := s.Tracker.GetPullRequest(ctx, number)
		if e != nil {
			t.Fatal(e)
		}
		yes := true
		pr.Mergeable = &yes
		pr.MergeableState = "clean"
		s.Tracker = &workTracker{Tracker: s.Tracker, observations: []tracker.PullRequest{*pr}}
	}
	r, err = s.Status(ctx, number)
	if err != nil {
		t.Fatal(err)
	}
	return s, issue, number, r
}

func TestRebaseWorkUnitEditRecovery(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		t.Run(fmt.Sprint(hosted), func(t *testing.T) {
			s, issue, number, r := recoveryFixture(t, hosted, true)
			ctx := context.Background()
			before := *r.State.PRRevision
			remote := r.PR.HeadSHA
			if r.WorkPreflight == nil || len(r.WorkPreflight.Commands) != 3 {
				t.Fatalf("commands: %+v", r.WorkPreflight)
			}
			for _, phase := range []string{"plan", "review"} {
				_, err := s.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: phase, Gate: contract.GateAgent, Headless: true}})
				if err != nil {
					t.Fatalf("%s: %v", phase, err)
				}
				st, err := s.Manager.GetCurrentState(ctx, issue)
				if err != nil || !reflect.DeepEqual(*st.PRRevision, before) {
					t.Fatalf("revision changed: %+v %v", st.PRRevision, err)
				}
			}
			verified, _ := s.Manager.GetCurrentState(ctx, issue)
			for _, art := range verified.Artifacts {
				if art.Type == contract.ArtifactReview || art.Type == contract.ArtifactDesign {
					if art.Metadata["agent"] != "mock" || art.Metadata["model"] != "default" {
						t.Fatalf("lost provenance: %v", art.Metadata)
					}
				}
			}
			if _, err := s.Validate(ctx, number); err == nil {
				t.Fatal("off-base validation admitted")
			}
			if _, err := s.Submit(ctx, number); err == nil {
				t.Fatal("off-base submit admitted")
			}
			if _, err := s.Rebase(ctx, Options{Number: number, Resolve: true, Work: workflow.WorkOptions{AgentName: "mock"}}); err != nil {
				t.Fatal(err)
			}
			st, err := s.Manager.GetCurrentState(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			iss, _ := s.Tracker.GetIssue(ctx, issue)
			unit, _ := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
			if err := lease.VerifyApproval(st, unit); err != nil {
				t.Fatal(err)
			}
			ancestor, err := workspace.BaseIsAncestor(ctx, s.Root, st.BaseSHA, st.PRRevision.LocalHeadSHA)
			if err != nil || !ancestor {
				t.Fatalf("ancestry: %v %v", ancestor, err)
			}
			data, err := os.ReadFile(filepath.Join(r.Worktree, "patch.txt"))
			if err != nil || string(data) != "pending patch" {
				t.Fatalf("patch lost: %q %v", data, err)
			}
			pr, _ := s.Tracker.GetPullRequest(ctx, number)
			if pr.HeadSHA != remote {
				t.Fatal("published")
			}
		})
	}
}

func TestPRRecoverySourcePreservation(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		for _, offbase := range []bool{false, true} {
			for _, phase := range []string{"plan", "review"} {
				for _, mutation := range []string{"write", "delete", "mode", "index", "head", "symlink", "odd-path", "instruction", "readback", "error", "cancel", "unchanged"} {
					t.Run(fmt.Sprintf("%v/%v/%s/%s", hosted, offbase, phase, mutation), func(t *testing.T) {
						s, issue, number, r := recoveryFixture(t, hosted, offbase)
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						if phase == "review" && offbase {
							if _, err := s.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan", Gate: contract.GateAgent, Headless: true}}); err != nil {
								t.Fatal(err)
							}
						}
						before, err := s.Manager.GetCurrentState(ctx, issue)
						if err != nil {
							t.Fatal(err)
						}
						sourceBefore, snapshotErr := prSourceSnapshot(ctx, r.Worktree)
						if snapshotErr != nil {
							t.Fatal(snapshotErr)
						}
						ran := false
						s.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
							return &pausePRRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
								ran = true
								path := filepath.Join(r.Worktree, "patch.txt")
								switch mutation {
								case "write", "error", "cancel":
									if err := os.WriteFile(path, []byte("hostile"), 0644); err != nil {
										t.Fatal(err)
									}
								case "delete":
									if err := os.Remove(path); err != nil {
										t.Fatal(err)
									}
								case "mode":
									if err := os.Chmod(path, 0755); err != nil {
										t.Fatal(err)
									}
								case "symlink":
									if err := os.Remove(filepath.Join(r.Worktree, "tracked-link")); err != nil {
										t.Fatal(err)
									}
									if err := os.Symlink("elsewhere", filepath.Join(r.Worktree, "tracked-link")); err != nil {
										t.Fatal(err)
									}
								case "odd-path":
									if err := os.WriteFile(filepath.Join(r.Worktree, "odd path\n.txt"), []byte("hostile"), 0644); err != nil {
										t.Fatal(err)
									}
								case "instruction":
									if err := os.WriteFile(filepath.Join(r.Worktree, "AGENTS.md"), []byte("# hostile\n"), 0644); err != nil {
										t.Fatal(err)
									}
								case "readback":
									if err := os.Rename(filepath.Join(r.Worktree, ".git"), filepath.Join(r.Worktree, ".git.saved")); err != nil {
										t.Fatal(err)
									}
								case "index":
									revisionGit(t, r.Worktree, "update-index", "--force-remove", "patch.txt")
								case "head":
									revisionGit(t, r.Worktree, "commit", "--allow-empty", "-qm", "hostile")
								}
								if mutation == "error" {
									return nil, errors.New("agent failure")
								}
								if mutation == "cancel" {
									cancel()
									return nil, ctx.Err()
								}
								return (&agent.MockRunner{}).Run(ctx, req)
							}}, nil
						}
						wrapped, wrapErr := s.preservingRunnerFactory(ctx, r.Worktree)("mock", nil, true)
						if wrapErr != nil {
							t.Fatal(wrapErr)
						}
						mock := &agent.MockRunner{}
						if wrapped.Name() != mock.Name() || !reflect.DeepEqual(wrapped.Provenance("planner", false), mock.Provenance("planner", false)) {
							t.Fatal("runner identity changed")
						}
						bin, args := wrapped.BuildCommand(agent.RunRequest{})
						wantBin, wantArgs := mock.BuildCommand(agent.RunRequest{})
						if bin != wantBin || !reflect.DeepEqual(args, wantArgs) {
							t.Fatal("runner command changed")
						}
						gotPath, gotAvailable := wrapped.Available()
						wantPath, wantAvailable := mock.Available()
						if gotPath != wantPath || gotAvailable != wantAvailable {
							t.Fatal("runner availability changed")
						}
						_, err = s.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: phase, Gate: contract.GateAgent, Headless: true}})
						if !ran {
							t.Fatalf("runner not reached: %v", err)
						}
						if mutation == "unchanged" {
							if err != nil {
								t.Fatal(err)
							}
							return
						}
						if err == nil || !strings.Contains(err.Error(), "preservation") {
							t.Fatalf("mutation accepted: %v", err)
						}
						sourceAfter, readErr := prSourceSnapshot(context.Background(), r.Worktree)
						if readErr == nil && sourceBefore == sourceAfter {
							t.Fatal("agent mutation was restored or never occurred")
						}
						after, e := s.Manager.GetCurrentState(context.Background(), issue)
						if e != nil {
							t.Fatal(e)
						}
						if !reflect.DeepEqual(before.PRRevision, after.PRRevision) {
							t.Fatal("recorded head changed")
						}
						if !reflect.DeepEqual(before.Artifacts, after.Artifacts) {
							t.Fatal("artifacts changed on guard failure")
						}
					})
				}
			}
		}
	}
}

func TestPRRecoveryPhaseBoundary(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		for _, phase := range []string{"", "all", "implement", "test", "plan", "review"} {
			t.Run(fmt.Sprintf("%v/%s", hosted, phase), func(t *testing.T) {
				s, issue, number, r := recoveryFixture(t, hosted, true)
				if phase == "review" {
					if _, err := s.Work(context.Background(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan", Gate: contract.GateAgent, Headless: true}}); err != nil {
						t.Fatal(err)
					}
				}
				if tr, ok := s.Tracker.(*workTracker); ok {
					no := false
					tr.observations[0].Mergeable = &no
					tr.observations[0].MergeableState = "dirty"
				}
				before, err := prSourceSnapshot(context.Background(), r.Worktree)
				if err != nil {
					t.Fatal(err)
				}
				st, _ := s.Manager.GetCurrentState(context.Background(), issue)
				roles := []string{}
				s.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
					return &pausePRRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
						roles = append(roles, req.Role)
						return (&agent.MockRunner{}).Run(ctx, req)
					}}, nil
				}
				_, err = s.Work(context.Background(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: phase, Gate: contract.GateAgent, Headless: true}})
				if phase == "plan" || phase == "review" {
					expected := "planner"
					if phase == "review" {
						expected = "reviewer"
					}
					if err != nil || !reflect.DeepEqual(roles, []string{expected}) {
						t.Fatalf("%v %v", roles, err)
					}
				} else if err == nil || len(roles) != 0 {
					t.Fatalf("code phase admitted: %v %v", roles, err)
				}
				after, e := prSourceSnapshot(context.Background(), r.Worktree)
				if e != nil || before != after {
					t.Fatal("source changed", e)
				}
				updated, _ := s.Manager.GetCurrentState(context.Background(), issue)
				if !reflect.DeepEqual(st.PRRevision, updated.PRRevision) {
					t.Fatal("revision changed")
				}
			})
		}
	}
}

func TestPRRecoveryHumanGate(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		t.Run(fmt.Sprint(hosted), func(t *testing.T) {
			s, issue, number, r := recoveryFixture(t, hosted, true)
			revisionState(t, s, issue, func(st *lease.IssueState) { st.EffectiveGate = contract.GateHuman })
			_, err := s.Rebase(context.Background(), Options{Number: number, Resolve: true, Work: workflow.WorkOptions{AgentName: "mock"}})
			var human *lease.HumanPlanApprovalError
			if !errors.As(err, &human) {
				t.Fatalf("human refusal: %v", err)
			}
			iss, _ := s.Tracker.GetIssue(context.Background(), issue)
			unit, _ := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
			// Simulate a human gate that became effective after the advisory preflight.
			st, _ := s.Manager.GetCurrentState(context.Background(), issue)
			_, err = s.reviewRebasePlan(context.Background(), r, "alice", st, Options{}, &resolver{unit: unit})
			if !errors.As(err, &human) {
				t.Fatalf("execution human refusal: %v", err)
			}
			opts := Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan", Gate: contract.GateHuman, Headless: true}}
			if _, err = s.Work(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			opts.Work.Phase = "review"
			_, _ = s.Work(context.Background(), opts)
			st, _ = s.Manager.GetCurrentState(context.Background(), issue)
			if lease.VerifyApproval(st, unit) == nil {
				t.Fatal("human approval fabricated")
			}
		})
	}
}

func TestPRRecoveryCommandSuggestions(t *testing.T) {
	t.Run("moved-target", testPRRecoveryMovedTarget)
	for _, hosted := range []bool{false, true} {
		for _, offbase := range []bool{false, true} {
			for _, evidence := range []string{"missing", "pair", "approved"} {
				t.Run(fmt.Sprintf("%v/%v/%s", hosted, offbase, evidence), func(t *testing.T) {
					// The real WorkUnit edit clears all plan evidence.
					s, issue, number, r := recoveryFixture(t, hosted, true)
					ctx := context.Background()
					if !offbase {
						// Reapprove at the pending head only for this ancestry classification fixture.
						revisionGit(t, s.Root, "reset", "--hard", r.State.PRRevision.LocalHeadSHA)
						if tr, ok := s.Tracker.(*workTracker); ok {
							tr.observations[0].BaseSHA = r.State.PRRevision.LocalHeadSHA
						}
						if _, err := s.Manager.ApproveTask(ctx, issue, "alice"); err != nil {
							t.Fatal(err)
						}
					}
					opts := Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan", Gate: contract.GateAgent, Headless: true}}
					if evidence != "missing" {
						if _, err := s.Work(ctx, opts); err != nil {
							t.Fatal(err)
						}
					}
					if evidence == "approved" {
						opts.Work.Phase = "review"
						if _, err := s.Work(ctx, opts); err != nil {
							t.Fatal(err)
						}
					}
					r, err := s.Status(ctx, number)
					if err != nil {
						t.Fatal(err)
					}
					expected := []string{}
					if evidence == "missing" {
						expected = append(expected, s.workCommand(r, "work", "--phase plan"))
					}
					if evidence != "approved" {
						expected = append(expected, s.workCommand(r, "work", "--phase review"))
					}
					if offbase {
						expected = append(expected, s.workCommand(r, "rebase", "--resolve"))
					}
					for _, command := range r.WorkPreflight.Commands {
						if strings.Contains(command, "--backend") {
							t.Fatalf("removed selector in recovery: %s", command)
						}
					}
					if !reflect.DeepEqual(r.WorkPreflight.Commands, expected) {
						t.Fatalf("got %v want %v", r.WorkPreflight.Commands, expected)
					}
					opts.Work.InspectOnly = true
					opts.Work.Phase = ""
					inspection, err := s.Work(ctx, opts)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(inspection.WorkPreflight.Commands, expected) {
						t.Fatal("inspection differs")
					}
					if offbase {
						_, err := s.Work(ctx, Options{Number: number})
						if err == nil {
							t.Fatal("default admitted")
						}
						for _, cmd := range expected {
							if !strings.Contains(err.Error(), cmd) {
								t.Fatalf("missing %s: %v", cmd, err)
							}
						}
					}
					// Execute the suggested plan/review prerequisites and the eventual rebase.
					opts.Work.InspectOnly = false
					if evidence == "missing" {
						opts.Work.Phase = "plan"
						if _, err := s.Work(ctx, opts); err != nil {
							t.Fatal(err)
						}
					}
					if evidence != "approved" {
						opts.Work.Phase = "review"
						if _, err := s.Work(ctx, opts); err != nil {
							t.Fatal(err)
						}
					}
					if offbase {
						if _, err := s.Rebase(ctx, Options{Number: number, Resolve: true, Work: workflow.WorkOptions{AgentName: "mock"}}); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		}
	}
}

// recoveryRaceStore injects a real successor immediately before a selected
// public service write. The stale CAS retries against the successor's fence.
type recoveryRaceStore struct {
	coordination.Store
	trigger func(*lease.IssueState) bool
	race    func()
}

func (s *recoveryRaceStore) CompareAndSwap(ctx context.Context, n int, old string, next []byte) (string, error) {
	var st lease.IssueState
	if err := json.Unmarshal(next, &st); err != nil {
		return "", err
	}
	if s.trigger != nil && s.trigger(&st) {
		s.trigger = nil
		s.race()
	}
	return s.Store.CompareAndSwap(ctx, n, old, next)
}

func TestPRRecoveryClaimFence(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		for _, phase := range []string{"plan", "review", "rebase"} {
			t.Run(fmt.Sprintf("%v/%s", hosted, phase), func(t *testing.T) {
				s, issue, number, r := recoveryFixture(t, hosted, true)
				ctx := context.Background()
				opts := Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan", Gate: contract.GateAgent, Headless: true}}
				if phase != "plan" {
					if _, err := s.Work(ctx, opts); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "rebase" {
					opts.Work.Phase = "review"
					if _, err := s.Work(ctx, opts); err != nil {
						t.Fatal(err)
					}
				}
				original := s.Manager.Store()
				baseline, _ := s.Manager.GetCurrentState(ctx, issue)
				var successor *lease.IssueState
				wrapper := &recoveryRaceStore{Store: original}
				wrapper.trigger = func(st *lease.IssueState) bool {
					if phase == "rebase" {
						return st.PRRevision != nil && st.PRRevision.LocalHeadSHA != baseline.PRRevision.LocalHeadSHA
					}
					typ := contract.ArtifactDesign
					if phase == "review" {
						typ = contract.ArtifactReview
					}
					for _, a := range st.Artifacts {
						if a.Type == typ {
							for _, b := range baseline.Artifacts {
								if b.Type == typ && reflect.DeepEqual(a, b) {
									return false
								}
							}
							return true
						}
					}
					return false
				}
				wrapper.race = func() {
					// Expire the original claim in authoritative state, then acquire through
					// the normal manager API rather than manufacturing the successor lease.
					b, rev, err := original.Load(ctx, issue)
					if err != nil {
						t.Fatal(err)
					}
					var st lease.IssueState
					if err = json.Unmarshal(b, &st); err != nil {
						t.Fatal(err)
					}
					st.Lease.ExpiresAt = time.Now().Add(-time.Hour)
					b, err = json.Marshal(&st)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = original.CompareAndSwap(ctx, issue, rev, b); err != nil {
						t.Fatal(err)
					}
					other := lease.NewManager(s.Tracker, contract.SecurityConfig{})
					other.SetStore(s.Root, original)
					successor, err = other.AcquirePR(ctx, issue, number, "alice", "mock", "all", time.Hour, 3)
					if err != nil {
						t.Fatal(err)
					}
				}
				s.Manager.SetStore(s.Root, wrapper)
				var err error
				if phase == "rebase" {
					_, err = s.Rebase(ctx, Options{Number: number, Resolve: true, Work: workflow.WorkOptions{AgentName: "mock"}})
				} else {
					opts.Work.Phase = phase
					_, err = s.Work(ctx, opts)
				}
				if successor == nil {
					t.Fatalf("race not reached: %v", err)
				}
				if !errors.Is(err, lease.ErrClaimFence) {
					t.Fatalf("stale write: %v", err)
				}
				after, _ := s.Manager.GetCurrentState(ctx, issue)
				if !reflect.DeepEqual(successor.PRRevision, after.PRRevision) || !reflect.DeepEqual(successor.Artifacts, after.Artifacts) || successor.Lease.ClaimID != after.Lease.ClaimID {
					t.Fatal("successor overwritten")
				}
				pr, _ := s.Tracker.GetPullRequest(ctx, number)
				if pr.HeadSHA != r.PR.HeadSHA {
					t.Fatal("published")
				}
			})
		}
	}
}

func TestPRRecoveryRefusals(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		for _, kind := range []string{"half-pair", "local-plan", "unavailable", "rejected", "dirty", "target", "rebase-intent", "task-edit", "ancestry-error"} {
			t.Run(fmt.Sprintf("%v/%s", hosted, kind), func(t *testing.T) {
				s, issue, number, r := recoveryFixture(t, hosted, true)
				ctx := context.Background()
				opts := Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan", Gate: contract.GateAgent, Headless: true}}
				if _, err := s.Work(ctx, opts); err != nil {
					t.Fatal(err)
				}
				opts.Work.Phase = "review"
				switch kind {
				case "half-pair":
					revisionState(t, s, issue, func(st *lease.IssueState) {
						for i, a := range st.Artifacts {
							if a.Type == contract.ArtifactTestPlan {
								st.Artifacts = append(st.Artifacts[:i], st.Artifacts[i+1:]...)
								break
							}
						}
					})
				case "local-plan":
					if err := os.WriteFile(filepath.Join(r.Worktree, ".swarm/active/PLAN.md"), []byte("local mismatch"), 0644); err != nil {
						t.Fatal(err)
					}
				case "unavailable":
					opts.Work.ReviewerAgent = "missing-reviewer"
				case "dirty":
					if err := os.WriteFile(filepath.Join(r.Worktree, "patch.txt"), []byte("dirty"), 0644); err != nil {
						t.Fatal(err)
					}
				case "target":
					revisionGit(t, s.Root, "commit", "--allow-empty", "-qm", "target moved again")
					if tr, ok := s.Tracker.(*workTracker); ok {
						tr.observations[0].BaseSHA = revisionGit(t, s.Root, "rev-parse", "HEAD")
					}
				case "task-edit":
					iss, _ := s.Tracker.GetIssue(ctx, issue)
					if _, err := s.Tracker.UpdateIssueBody(ctx, issue, strings.Replace(iss.Body, "new objective", "unapproved objective", 1)); err != nil {
						t.Fatal(err)
					}
				case "rebase-intent":
					revisionState(t, s, issue, func(st *lease.IssueState) { st.PRRevision.Intent = "rebase" })
				case "ancestry-error":
					revisionState(t, s, issue, func(st *lease.IssueState) { st.PRRevision.LocalHeadSHA = strings.Repeat("a", 40) })
				case "rejected":
					s.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
						return &pausePRRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
							result, err := (&agent.MockRunner{}).Run(ctx, req)
							if err != nil {
								return result, err
							}
							err = os.WriteFile(filepath.Join(req.WorktreePath, ".swarm/active/REVIEW.md"), []byte("VERDICT: CHANGES_REQUESTED\nRevise the plan.\n"), 0644)
							return result, err
						}}, nil
					}
				}
				before, _ := s.Manager.GetCurrentState(ctx, issue)
				_, err := s.Work(ctx, opts)
				after, _ := s.Manager.GetCurrentState(ctx, issue)
				iss, _ := s.Tracker.GetIssue(ctx, issue)
				unit, _ := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
				if err == nil && kind != "rejected" {
					t.Fatal("refusal missing")
				}
				if lease.VerifyApproval(after, unit) == nil {
					t.Fatal("approval created")
				}
				if !reflect.DeepEqual(before.PRRevision, after.PRRevision) {
					t.Fatal("revision changed")
				}
				pr, _ := s.Tracker.GetPullRequest(ctx, number)
				if pr.HeadSHA != r.PR.HeadSHA {
					t.Fatal("published")
				}
			})
		}
	}
}

func testPRRecoveryMovedTarget(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		t.Run(fmt.Sprint(hosted), func(t *testing.T) {
			s, issue, number, _ := recoveryFixture(t, hosted, true)
			ctx := context.Background()
			opts := Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: "plan", Gate: contract.GateAgent, Headless: true}}
			if _, err := s.Work(ctx, opts); err != nil {
				t.Fatal(err)
			}
			revisionGit(t, s.Root, "commit", "--allow-empty", "-qm", "target moved")
			target := revisionGit(t, s.Root, "rev-parse", "HEAD")
			if tr, ok := s.Tracker.(*workTracker); ok {
				tr.observations[0].BaseSHA = target
			}
			r, err := s.Status(ctx, number)
			if err != nil {
				t.Fatal(err)
			}
			commands := r.WorkPreflight.Commands
			if len(commands) != 5 || !strings.Contains(commands[0], "issue release") || !strings.Contains(commands[1], "issue approve") || !strings.Contains(commands[2], "--phase plan") || !strings.Contains(commands[3], "--phase review") {
				t.Fatalf("moved guidance: %v", commands)
			}
			before, _, _ := s.Manager.Store().Load(ctx, issue)
			if _, err = s.Rebase(ctx, Options{Number: number, Resolve: true, Work: workflow.WorkOptions{AgentName: "mock"}}); err == nil {
				t.Fatal("invalid approval carried")
			}
			after, _, _ := s.Manager.Store().Load(ctx, issue)
			if string(before) != string(after) {
				t.Fatal("rebase changed approval before refusal")
			}
			if _, err = s.Manager.Release(ctx, issue, "alice", "approve target"); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Manager.ApproveTask(ctx, issue, "alice"); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Work(ctx, opts); err != nil {
				t.Fatal(err)
			}
			opts.Work.Phase = "review"
			if _, err = s.Work(ctx, opts); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Rebase(ctx, Options{Number: number, Resolve: true, Work: workflow.WorkOptions{AgentName: "mock"}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
