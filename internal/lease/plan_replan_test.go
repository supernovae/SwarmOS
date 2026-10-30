package lease

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"gopkg.in/yaml.v3"
)

func rejectedReplanFixture(t *testing.T) (*Manager, int, *IssueState) {
	t.Helper()
	m, _, issue := testManager(t)
	ctx := context.Background()
	st, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil || st == nil {
		t.Fatal(err)
	}

	st, err = m.RecordPlanPair(ctx, issue.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "report", Body: "report"}, contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "report", Body: "report"})
	if err != nil {
		t.Fatal(err)
	}
	meta := ReviewBindings(st)
	meta["kind"] = "agent"
	st, err = m.UpdateArtifact(ctx, issue.Number, "local-dev", contract.Artifact{Type: contract.ArtifactReview, Producer: "mock:reviewer", Summary: "review", Body: "VERDICT: CHANGES_REQUESTED\n\nRevise the plan", Verdict: "CHANGES_REQUESTED", Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	return m, issue.Number, st
}

func TestPlanReplanBudgetSurvivesArtifactsAndClaims(t *testing.T) {
	t.Parallel()
	m, n, st := rejectedReplanFixture(t)
	ctx := context.Background()
	if st.Lease.NextPhase != "plan" || st.NextPhaseReason == "" || st.PendingPlanReview == nil || st.PlanReplanCount != 0 {
		t.Fatalf("missing rejection transition: %+v", st)
	}
	rejection := *st.PendingPlanReview
	for i := 1; i <= 2; i++ {
		var err error
		st, err = m.UpdatePhase(ctx, n, "local-dev", "plan", "active")
		if err != nil {
			t.Fatal(err)
		}
		if st.PlanReplanCount != i {
			t.Fatalf("count = %d, want %d", st.PlanReplanCount, i)
		}
		// Automatic reports, even with spoofed manual metadata, cannot reset.
		st, err = m.UpdateArtifact(ctx, n, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "interactive:planner", Summary: "plan", Body: "new plan", Metadata: map[string]string{"reset": "true", "plan_replan_count": "0"}})
		if err != nil {
			t.Fatal(err)
		}
		if st.PlanReplanCount != i || *st.PendingPlanReview != rejection {
			t.Fatal("plan write reset retry evidence")
		}
		if _, err = m.Release(ctx, n, "local-dev", "retry"); err != nil {
			t.Fatal(err)
		}
		_, err = m.Acquire(ctx, n, "local-dev", "mock", "all", time.Hour, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.UpdatePhase(ctx, n, "local-dev", "plan", "active"); !errors.Is(err, ErrPlanReplanLimit) {
		t.Fatalf("cap: %v", err)
	}
	if _, err := m.UpdatePhase(ctx, n, "local-dev", "review", "active"); err != nil {
		t.Fatalf("review-only recovery: %v", err)
	}
	m.BindClaim(n, "stale")
	if _, err := m.UpdatePhase(ctx, n, "local-dev", "plan", "active"); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("stale reservation: %v", err)
	}
}

func TestPlanReplanManualAndHumanReset(t *testing.T) {
	t.Parallel()
	for _, verdict := range []string{"APPROVED", "CHANGES_REQUESTED"} {
		t.Run(verdict, func(t *testing.T) {
			t.Parallel()
			m, n, _ := rejectedReplanFixture(t)
			ctx := context.Background()
			_, err := m.UpdatePhase(ctx, n, "local-dev", "plan", "active")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.ResetPlanReplansAfterStage(ctx, n, "local-dev", 99, 1); err == nil {
				t.Fatal("mismatched stage reset budget")
			}
			st, err := m.GetCurrentState(ctx, n)
			if err != nil || st.PlanReplanCount != 1 {
				t.Fatal("failed reset changed count", err)
			}
			st, err = m.RecordHumanReview(ctx, n, verdict, "Human decision")
			if err != nil || st.PlanReplanCount != 0 {
				t.Fatal("human review did not reset", err)
			}
		})
	}
	m, n, _ := rejectedReplanFixture(t)
	ctx := context.Background()
	if _, err := m.UpdatePhase(ctx, n, "local-dev", "plan", "active"); err != nil {
		t.Fatal(err)
	}
	st, err := m.ResetPlanReplansAfterStage(ctx, n, "local-dev", 1, 1)
	if err != nil || st.PlanReplanCount != 0 {
		t.Fatal("manual stage did not reset", err)
	}
}

func TestAdvisoryRejectionCannotSetNextPhase(t *testing.T) {
	t.Parallel()
	m, n, _ := rejectedReplanFixture(t)
	// Remove trusted rejection by authenticated human decision.
	var err error
	_, err = m.RecordHumanReview(context.Background(), n, "APPROVED", "Reviewed")
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.UpdateAdvisoryReview(context.Background(), n, "local-dev", contract.Artifact{Type: contract.ArtifactReview, Producer: "mock", Summary: "advisory", Body: "VERDICT: CHANGES_REQUESTED", Verdict: "CHANGES_REQUESTED", Metadata: map[string]string{"kind": "agent", "review_source": "authenticated-agent"}})
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingPlanReview != nil || st.NextPhaseReason != "" {
		t.Fatal("advisory review created authoritative rejection")
	}
}

func TestPlanReplanConcurrentReservation(t *testing.T) {
	t.Parallel()
	m, n, _ := rejectedReplanFixture(t)
	ctx := context.Background()
	if _, err := m.UpdatePhase(ctx, n, "local-dev", "plan", "active"); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := m.UpdatePhase(ctx, n, "local-dev", "plan", "active"); results <- err }()
	}
	accepted, refused := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			accepted++
		} else if errors.Is(err, ErrPlanReplanLimit) {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("accepted=%d refused=%d", accepted, refused)
	}
}

func TestPlanReplanRevisionApprovalReset(t *testing.T) {
	t.Parallel()
	m, n, _ := rejectedReplanFixture(t)
	ctx := context.Background()
	if _, err := m.UpdatePhase(ctx, n, "local-dev", "plan", "active"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, n, "local-dev", "approval"); err != nil {
		t.Fatal(err)
	}
	st, err := m.ApproveTask(ctx, n, "local-dev")
	if err != nil || st.PlanReplanCount != 1 {
		t.Fatal("identical approval reset retries", err)
	}
	issue, err := m.tracker.GetIssue(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	objective := "Changed approved objective"
	_, body, err := contract.EditWorkUnitIssue(n, issue.Title, issue.Body, contract.WorkUnitEdit{Objective: &objective})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.tracker.UpdateIssueBody(ctx, n, body); err != nil {
		t.Fatal(err)
	}
	st, err = m.ApproveTask(ctx, n, "local-dev")
	if err != nil || st.PlanReplanCount != 0 || st.PendingPlanReview != nil {
		t.Fatal("changed approval did not reset retries", err)
	}
}

func TestPlanReplanTrustedPolicyModes(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"auto", "agent", "interactive"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			m, n, _ := rejectedReplanFixture(t)
			ctx := context.Background()
			// Install an explicitly approved policy revision with zero retries.
			if _, err := m.Release(ctx, n, "local-dev", "policy change"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(m.repoRoot, ".swarm/config.yaml")
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body := "plan_mode: " + mode + "\n" + strings.Replace(string(content), "lease:\n", "lease:\n  plan_replan_limit: 0\n", 1)
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "policy"}} {
				cmd := gitfixture.Command(t, "", args...)
				cmd.Dir = m.repoRoot
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatal(err, string(out))
				}
			}
			if _, err := m.ApproveTask(ctx, n, "local-dev"); err != nil {
				t.Fatal(err)
			}
			st, err := m.Acquire(ctx, n, "local-dev", "mock", "all", time.Hour, 1)
			if err != nil || st == nil {
				t.Fatal(err)
			}

			st, err = m.RecordPlanPair(ctx, n, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "report"}, contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "test", Body: "report"})
			if err != nil {
				t.Fatal(err)
			}
			meta := ReviewBindings(st)
			meta["kind"] = "agent"
			st, err = m.UpdateArtifact(ctx, n, "local-dev", contract.Artifact{Type: contract.ArtifactReview, Producer: "mock", Summary: "review", Body: "VERDICT: CHANGES_REQUESTED", Verdict: "CHANGES_REQUESTED", Metadata: meta})
			if err != nil {
				t.Fatal(err)
			}
			// Candidate policy cannot expand the approved zero budget.
			if err := os.WriteFile(path, []byte(strings.Replace(body, "plan_replan_limit: 0", "plan_replan_limit: 99", 1)), 0600); err != nil {
				t.Fatal(err)
			}
			err = m.CheckPlanReplan(st)
			if mode == "interactive" {
				if err != nil {
					t.Fatal("interactive mode refused", err)
				}
				st, err = m.UpdatePhase(ctx, n, "local-dev", "plan", "active")
				if err != nil || st.PlanReplanCount != 0 {
					t.Fatal("interactive mode consumed budget", err)
				}
			} else {
				if !errors.Is(err, ErrPlanReplanLimit) {
					t.Fatal("approved zero limit ignored", err)
				}
				for _, decision := range []string{"--reject", "--approve"} {
					if !strings.Contains(err.Error(), fmt.Sprintf("swarm issue review-plan %d %s", n, decision)) {
						t.Fatal(err)
					}
				}
				if mode == "agent" && strings.Contains(err.Error(), " stage ") {
					t.Fatal("agent-only policy offered staging", err)
				}
			}
		})
	}
}

func TestForkStageCannotResetBudget(t *testing.T) {
	t.Parallel()
	m, n, _ := rejectedReplanFixture(t)
	ctx := context.Background()
	_, err := m.UpdatePhase(ctx, n, "local-dev", "plan", "active")
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.mutate(ctx, n, "local-dev", func(st *IssueState) error { st.ForkRepo = "contributor/project"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ResetPlanReplansAfterStage(ctx, n, "local-dev", 1, 1); err == nil {
		t.Fatal("fork task reset by staging")
	}
	st, err := m.UpdateArtifact(ctx, n, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "interactive:planner", Summary: "fork staged plan", Body: "fork plan", Metadata: map[string]string{"reset_plan_replans": "true"}})
	if err != nil || st.PlanReplanCount != 1 {
		t.Fatal("fork stage changed budget", err)
	}
}

func TestAgentApprovalPreservesConsumedBudget(t *testing.T) {
	t.Parallel()
	m, n, _ := rejectedReplanFixture(t)
	ctx := context.Background()
	st, err := m.UpdatePhase(ctx, n, "local-dev", "plan", "active")
	if err != nil {
		t.Fatal(err)
	}
	meta := ReviewBindings(st)
	meta["kind"] = "agent"
	st, err = m.UpdateArtifact(ctx, n, "local-dev", contract.Artifact{Type: contract.ArtifactReview, Producer: "mock", Summary: "review", Body: "VERDICT: APPROVED", Verdict: "APPROVED", Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	if st.PlanReplanCount != 1 || st.PendingPlanReview != nil || st.NextPhaseReason != "" || st.Lease.NextPhase != "" {
		t.Fatalf("approval transition: %+v", st)
	}
}

func TestPlanReplanPendingPRGuidance(t *testing.T) {
	t.Parallel()
	m, _, st := rejectedReplanFixture(t)
	st.PlanReplanCount = 100
	st.PRRevision = &PRRevision{Number: 37}
	err := m.CheckPlanReplan(st)
	if !errors.Is(err, ErrPlanReplanLimit) || !strings.Contains(err.Error(), "swarm pr work 37 --phase review") {
		t.Fatal(err)
	}
	if !strings.Contains(err.Error(), "swarm pr stage 37 --phase plan") {
		t.Fatal(err)
	}
}

func TestPlanReplanHumanGuidance(t *testing.T) {
	t.Parallel()
	m, issue, st := rejectedReplanFixture(t)
	for _, fork := range []string{"", "contributor/repo"} {
		for _, pr := range []bool{false, true} {
			st.PlanReplanCount = 2
			st.ForkRepo = fork
			st.PRRevision = nil
			if pr {
				st.PRRevision = &PRRevision{Number: 37}
			}
			before := *st
			err := m.CheckPlanReplan(st)
			if !errors.Is(err, ErrPlanReplanLimit) {
				t.Fatal(err)
			}
			for _, want := range []string{
				fmt.Sprintf("swarm issue review-plan %d --reject", issue),
				fmt.Sprintf("swarm issue review-plan %d --approve", issue),
				"request a revised plan", "does not approve implementation", "authorized independent upstream maintainer, outside --fork",
				"edit and reapprove the WorkUnit", "manually revise both reports", "managed worktree",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("missing %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "review-plan 37") || strings.Contains(err.Error(), "swarm --fork") {
				t.Fatal(err)
			}
			if fork != "" && !strings.Contains(err.Error(), "fork staging preserves the count") {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, *st) {
				t.Fatal("guidance changed state")
			}
		}
	}
}

func TestPlanReplanIndependentHumanRefusal326(t *testing.T) {
	t.Parallel()
	m, _, issue, _ := reviewerPolicyManager(t, "  independent_review: true\n")
	ctx := context.Background()
	st, err := m.GetCurrentState(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	meta := ReviewBindings(st)
	meta["kind"] = "agent"
	_, err = m.UpdateArtifact(ctx, issue.Number, "local-dev", contract.Artifact{Type: contract.ArtifactReview, Producer: "codex:reviewer", Summary: "review", Body: "VERDICT: CHANGES_REQUESTED", Verdict: "CHANGES_REQUESTED", Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	before, err := m.UpdatePhase(ctx, issue.Number, "local-dev", "plan", "active")
	if err != nil {
		t.Fatal(err)
	}
	for _, verdict := range []string{"CHANGES_REQUESTED", "APPROVED"} {
		if _, err := m.RecordHumanReview(ctx, issue.Number, verdict, "self decision"); !errors.Is(err, ErrApprovalRequired) {
			t.Fatal(err)
		}
		after, err := m.GetCurrentState(ctx, issue.Number)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("self decision changed state", err)
		}
	}
}

func TestIssueWorkRecoveryPairPredicate(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, rejected := range []bool{false, true} {
			for _, tracked := range []bool{false, true} {
				for _, complete := range []bool{false, true} {
					t.Run(fmt.Sprintf("direct=%t/rejected=%t/tracked=%t/complete=%t", direct, rejected, tracked, complete), func(t *testing.T) {
						st := &IssueState{Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Version: 1, Body: "p"}, {Type: contract.ArtifactTestPlan, Version: 1, Body: "t"}}}
						if !direct {
							st.ForkRepo = "contributor/repo"
						}
						if rejected {
							st.PendingPlanReview = &PlanRejection{}
						}
						if tracked {
							st.PlanGeneration = 1
						}
						if complete {
							st.CompletePlan = &PlanPair{Generation: st.PlanGeneration, PlanVersion: 1, TestVersion: 1, PlanDigest: hash("p"), TestDigest: hash("t")}
						}
						want := direct && (rejected || tracked) && !(tracked && complete)
						if got := IncompletePlanPair(st); got != want {
							t.Fatalf("incomplete=%t want %t", got, want)
						}
						st.Artifacts = nil
						if IncompletePlanPair(st) {
							t.Fatal("removed reports prevent authorized regeneration")
						}
					})
				}
			}
		}
	}
}
func TestIssueWorkRecoveryAtomicPair(t *testing.T) {
	m, n, st := rejectedReplanFixture(t)
	ctx := t.Context()
	if !CompletePlanPair(st) {
		t.Fatal("missing generation")
	}
	old := *st.CompletePlan
	_, err := m.UpdatePhase(ctx, n, "local-dev", "plan", "active")
	if err != nil {
		t.Fatal(err)
	}
	plan := contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "revised"}
	test := contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "test", Body: "report"}
	bad := test
	bad.Body = ""
	if _, err = m.RecordPlanPair(ctx, n, "local-dev", plan, bad); err == nil {
		t.Fatal("invalid pair accepted")
	}
	st, _ = m.GetCurrentState(ctx, n)
	if *st.CompletePlan != old {
		t.Fatal("failed write changed generation")
	}
	st, err = m.RecordPlanPair(ctx, n, "local-dev", plan, test)
	if err != nil {
		t.Fatal(err)
	}
	if !CompletePlanPair(st) || st.PlanReplanCount != 1 || st.PendingPlanReview.Findings != "VERDICT: CHANGES_REQUESTED\n\nRevise the plan" {
		t.Fatalf("lost provenance: %+v", st)
	}
	if st.CompletePlan.TestDigest != old.TestDigest {
		t.Fatal("unchanged report not preserved")
	}
	for _, versionOnly := range []bool{false, true} {
		copyState := *st
		copyState.Artifacts = append([]contract.Artifact(nil), st.Artifacts...)
		if versionOnly {
			copyState.Artifacts[0].Version++
		} else {
			copyState.Artifacts[0].Body += "changed"
		}
		if CompletePlanPair(&copyState) || !IncompletePlanPair(&copyState) {
			t.Fatalf("generation mismatch admitted (version=%t)", versionOnly)
		}
	}

	st, err = m.UpdateArtifact(ctx, n, "local-dev", plan)
	if err != nil {
		t.Fatal(err)
	}
	if !IncompletePlanPair(st) || st.PlanGeneration != 2 {
		t.Fatal("single write lost tracked state")
	}
	if _, err = m.RecordHumanReview(ctx, n, "APPROVED", "reviewed"); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("mixed approval: %v", err)
	}
}
func TestIssueWorkRecoveryHumanObligation(t *testing.T) {
	for _, gate := range []contract.GateMode{contract.GateNone, contract.GateAgent, contract.GateHuman} {
		t.Run(string(gate), func(t *testing.T) {
			m, n, _ := rejectedReplanFixture(t)
			ctx := t.Context()
			st, err := m.RecordHumanReview(ctx, n, "CHANGES_REQUESTED", "Please revise the plan")
			if err != nil {
				t.Fatal(err)
			}
			if st.HumanPlanReview == nil {
				t.Fatal("missing human obligation")
			}
			st, err = m.UpdatePhase(ctx, n, "local-dev", "plan", "active")
			if err != nil || st.HumanDraftCount != 1 {
				t.Fatal(err)
			}
			st, err = m.RecordPlanPair(ctx, n, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "revised plan"}, contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "test", Body: "report"})
			if err != nil {
				t.Fatal(err)
			}
			st.EffectiveGate = gate
			issue, loadErr := m.tracker.GetIssue(ctx, n)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			wu, parseErr := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			var humanStop *HumanPlanApprovalError
			if approvalErr := VerifyApproval(st, wu); !errors.As(approvalErr, &humanStop) {
				t.Fatalf("human obligation bypassed or wrong binding failure: %v", approvalErr)
			}
			if gate == contract.GateNone {
				control := *st
				control.HumanPlanReview = nil
				if err := VerifyApproval(&control, wu); err != nil {
					t.Fatalf("control has unrelated invalid binding: %v", err)
				}
			}
			if st.HumanDraftCount != 1 || !CompletePlanPair(st) {
				t.Fatal("draft not recorded")
			}
			st, err = m.RecordHumanReview(ctx, n, "APPROVED", "Reviewed exact reports")
			if err != nil {
				t.Fatal(err)
			}
			if st.HumanPlanReview != nil {
				t.Fatal("authenticated approval did not discharge obligation")
			}
			st.EffectiveGate = gate
			if err := VerifyApproval(st, wu); err != nil {
				t.Fatalf("fresh authenticated exact-pair approval refused: %v", err)
			}
		})
	}
}
func TestIssueWorkRecoveryPersistentRepairBudget(t *testing.T) {
	m, n, _ := rejectedReplanFixture(t)
	ctx := t.Context()
	st, err := m.ReserveValidationRepair(ctx, n, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.ValidationRepairCount != 1 || st.ValidationRepairKey == "" {
		t.Fatal("missing repair reservation")
	}
	if _, err = m.Release(ctx, n, "local-dev", "restart"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Acquire(ctx, n, "local-dev", "mock", "all", time.Hour, 1); err != nil {
		t.Fatal(err)
	}

	if _, err = m.ReserveValidationRepair(ctx, n, "local-dev"); !errors.Is(err, ErrValidationRepairLimit) {
		t.Fatalf("exhaustion: %v", err)
	}
	st, _ = m.GetCurrentState(ctx, n)
	if st.ValidationRepairCount != 1 {
		t.Fatal("claim reset allowance")
	}
	m.BindClaim(n, "stale")
	if _, err = m.ReserveValidationRepair(ctx, n, "local-dev"); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("fence: %v", err)
	}
}

func TestIssueWorkRecoveryBranchApproval(t *testing.T) {
	for _, changed := range []bool{false, true} {
		for _, limit := range []int{0, 1} {
			t.Run(fmt.Sprintf("changed=%t/limit=%d", changed, limit), func(t *testing.T) {
				m, tr, issue := testManager(t)
				ctx := t.Context()
				if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
					t.Fatal(err)
				}
				st, err := m.RecordPlanPair(ctx, issue.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "plan"}, contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "test", Body: "test"})
				if err != nil {
					t.Fatal(err)
				}
				if !CompletePlanPair(st) {
					t.Fatal("initial pair missing")
				}
				st, err = m.RecordHumanReview(ctx, issue.Number, "CHANGES_REQUESTED", "Keep these authenticated findings after the base move.")
				if err != nil {
					t.Fatal(err)
				}
				rejection := *st.HumanPlanReview
				if _, err = m.UpdatePhase(ctx, issue.Number, "local-dev", "plan", "active"); err != nil {
					t.Fatal(err)
				}
				if _, err = m.Release(ctx, issue.Number, "local-dev", "move approved branch"); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(m.repoRoot, ".swarm/config.yaml")
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var cfg map[string]any
				if err = yaml.Unmarshal(body, &cfg); err != nil {
					t.Fatal(err)
				}
				cfg["base_branch"] = "next"
				cfg["lease"].(map[string]any)["plan_replan_limit"] = limit
				body, err = yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
				git := func(args ...string) {
					t.Helper()
					cmd := gitfixture.Command(t, m.repoRoot, args...)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git %v: %v %s", args, err, out)
					}
				}
				git("add", ".swarm/config.yaml")
				git("-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "approved branch policy")
				git("branch", "next")
				if changed {
					wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
					if err != nil {
						t.Fatal(err)
					}
					wu.Objective += " with revised scope"
					if _, err = tr.UpdateIssueBody(ctx, issue.Number, contract.FormatIssueMarkdown(wu)); err != nil {
						t.Fatal(err)
					}
				}
				st, err = m.ApproveTask(ctx, issue.Number, "local-dev")
				if err != nil {
					t.Fatal(err)
				}
				if st.BaseBranch != "next" || len(st.Artifacts) != 0 || st.CompletePlan != nil {
					t.Fatalf("branch invalidation: %+v", st)
				}
				if changed {
					if st.HumanPlanReview != nil || st.HumanDraftCount != 0 || st.PlanGeneration != 0 {
						t.Fatal("changed task retained obligation")
					}
					if err = m.CheckPlanReplan(st); err != nil {
						t.Fatal("new contract cannot plan", err)
					}
				} else {
					if st.HumanPlanReview == nil || *st.HumanPlanReview != rejection || st.HumanDraftCount != 1 || st.PlanGeneration == 0 {
						t.Fatal("identical task lost obligation or consumption")
					}
					if err = m.CheckPlanReplan(st); !errors.Is(err, ErrPlanReplanLimit) {
						t.Fatal("branch move replenished allowance", err)
					}
					for _, verdict := range []string{"APPROVED", "CHANGES_REQUESTED"} {
						if _, err = m.RecordHumanReview(ctx, issue.Number, verdict, "decision"); !errors.Is(err, ErrApprovalRequired) {
							t.Fatal("missing reports acquired review authority", err)
						}
					}
				}
			})
		}
	}
}
