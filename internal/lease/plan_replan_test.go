package lease

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

func rejectedReplanFixture(t *testing.T) (*Manager, int, *IssueState) {
	t.Helper()
	m, _, issue := testManager(t)
	ctx := context.Background()
	st, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []contract.ArtifactType{contract.ArtifactDesign, contract.ArtifactTestPlan} {
		st, err = m.UpdateArtifact(ctx, issue.Number, "local-dev", contract.Artifact{Type: kind, Producer: "mock", Summary: "report", Body: "report"})
		if err != nil {
			t.Fatal(err)
		}
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
	for _, verdict := range []string{"APPROVED", "CHANGES_REQUESTED"} {
		t.Run(verdict, func(t *testing.T) {
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
	for _, mode := range []string{"auto", "agent", "interactive"} {
		t.Run(mode, func(t *testing.T) {
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
				cmd := exec.Command("git", args...)
				cmd.Dir = m.repoRoot
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatal(err, string(out))
				}
			}
			if _, err := m.ApproveTask(ctx, n, "local-dev"); err != nil {
				t.Fatal(err)
			}
			st, err := m.Acquire(ctx, n, "local-dev", "mock", "all", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			for _, kind := range []contract.ArtifactType{contract.ArtifactDesign, contract.ArtifactTestPlan} {
				st, err = m.UpdateArtifact(ctx, n, "local-dev", contract.Artifact{Type: kind, Producer: "mock", Summary: "report", Body: "report"})
				if err != nil {
					t.Fatal(err)
				}
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
				if mode == "agent" && strings.Contains(err.Error(), " stage ") {
					t.Fatal("agent-only policy offered staging", err)
				}
			}
		})
	}
}

func TestForkStageCannotResetBudget(t *testing.T) {
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
	m, _, st := rejectedReplanFixture(t)
	st.PlanReplanCount = 100
	st.PRRevision = &PRRevision{Number: 37}
	err := m.CheckPlanReplan(st)
	if !errors.Is(err, ErrPlanReplanLimit) || !strings.Contains(err.Error(), "swarm pr work 37 --phase review") || strings.Contains(err.Error(), "issue review-plan") {
		t.Fatal(err)
	}
	if !strings.Contains(err.Error(), "swarm pr stage 37 --phase plan") {
		t.Fatal(err)
	}
}
