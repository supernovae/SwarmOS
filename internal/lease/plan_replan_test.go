package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	if st.PlanReviewSequence != 2 || len(st.PlanReviewHistory) != 2 {
		t.Fatal("advisory review entered history")
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
	if err != nil || st.PlanReplanCount != 1 || st.PlanReviewSequence != 1 || len(st.PlanReviewHistory) != 1 {
		t.Fatal("identical approval reset retries/history", err)
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
	if err != nil || st.PlanReplanCount != 0 || st.PendingPlanReview != nil || st.PlanReviewSequence != 0 || len(st.PlanReviewHistory) != 0 {
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

func TestPlanReviewHistoryAuthenticatedAndBounded(t *testing.T) {
	m, n, st := rejectedReplanFixture(t)
	ctx := context.Background()
	if len(st.PlanReviewHistory) != 1 || PlanReviewRound(st) != 1 {
		t.Fatalf("first review: %+v", st.PlanReviewHistory)
	}
	for round := 2; round <= 10; round++ {
		var err error
		st, err = m.UpdateArtifact(ctx, n, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: fmt.Sprintf("plan %d", round)})
		if err != nil {
			t.Fatal(err)
		}
		if PlanReviewRound(st) != round || len(ReviewHistory(st)) == 0 {
			t.Fatal("lost round after report replacement")
		}
		if VerifyApproval(st, &contract.WorkUnit{Gate: contract.GateAgent}) == nil {
			t.Fatal("history approved changed plan")
		}
		meta := ReviewBindings(st)
		meta["kind"] = "agent"
		st, err = m.UpdateArtifact(ctx, n, "local-dev", contract.Artifact{Type: contract.ArtifactReview, Producer: "mock:reviewer", Summary: "review", Verdict: "CHANGES_REQUESTED", Body: "VERDICT: CHANGES_REQUESTED\n\n" + strings.Repeat("界", 6000), Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(st.PlanReviewHistory) != 8 || st.PlanReviewHistory[0].Round != 3 || st.PlanReviewSequence != 10 {
		t.Fatalf("bounds: %+v", st.PlanReviewHistory)
	}
	last := st.PlanReviewHistory[7]
	if !utf8.ValidString(last.Findings) || len(last.Findings) > 16<<10 || !strings.HasSuffix(last.Findings, "[Earlier review findings truncated]") {
		t.Fatal("unbounded findings")
	}
	oldSeq := st.PlanReviewSequence
	meta := ReviewBindings(st)
	meta["kind"] = "agent"
	meta["task_digest"] = "forged"
	if _, err := m.UpdateArtifact(ctx, n, "local-dev", contract.Artifact{Type: contract.ArtifactReview, Producer: "mock:reviewer", Summary: "bad", Verdict: "APPROVED", Body: "VERDICT: APPROVED", Metadata: meta}); err == nil {
		t.Fatal("forged binding accepted")
	}
	got, err := m.GetCurrentState(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanReviewSequence != oldSeq {
		t.Fatal("failed write appended history")
	}
	st.TaskDigest = "other"
	if len(ReviewHistory(st)) != 0 {
		t.Fatal("stale projection")
	}
	dropPlanReviewHistory(st)
	if st.PlanReviewSequence != 0 || st.PlanReviewHistory != nil || PlanReviewRound(st) != 1 {
		t.Fatal("revision reset")
	}
}

func TestPlanReviewHistoryHumanReset(t *testing.T) {
	m, n, _ := rejectedReplanFixture(t)
	st, err := m.RecordHumanReview(context.Background(), n, "CHANGES_REQUESTED", "narrow the plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PlanReviewHistory) != 2 || PlanReviewRound(st) != 2 || st.PlanReplanCount != 0 {
		t.Fatalf("human history: %+v", st)
	}
}

func TestPlanReviewHistoryRevisionInvalidation(t *testing.T) {
	for _, change := range []string{"task", "policy", "base"} {
		t.Run(change, func(t *testing.T) {
			m, n, _ := rejectedReplanFixture(t)
			ctx := context.Background()
			if _, err := m.Release(ctx, n, "local-dev", "reapprove"); err != nil {
				t.Fatal(err)
			}
			st, rev, err := m.load(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			// Independently exercise each approved identity comparison without changing
			// the policy tree, which would conflate policy and base invalidation.
			switch change {
			case "task":
				st.TaskDigest = "old-task"
			case "policy":
				st.PolicyDigest = "old-policy"
			case "base":
				st.BaseSHA = "old-base"
			}
			if _, err := m.save(ctx, n, rev, st); err != nil {
				t.Fatal(err)
			}
			got, err := m.ApproveTask(ctx, n, "local-dev")
			if err != nil {
				t.Fatal(err)
			}
			if len(got.PlanReviewHistory) != 0 || got.PlanReviewSequence != 0 || PlanReviewRound(got) != 1 {
				t.Fatal("revision retained history")
			}
		})
	}
}

func TestPlanReviewHistoryLegacyAndUntrustedWrites(t *testing.T) {
	m, n, st := rejectedReplanFixture(t)
	ctx := context.Background()
	// An old active artifact is never promoted from metadata into history.
	var legacy IssueState
	if err := json.Unmarshal([]byte(`{"artifacts":[{"type":"review","metadata":{"review_source":"authenticated-agent"}}]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if PlanReviewRound(&legacy) != 1 || len(ReviewHistory(&legacy)) != 0 {
		t.Fatal("legacy artifact promoted")
	}
	meta := ReviewBindings(st)
	meta["kind"] = "agent"
	meta["review_source"] = "authenticated-agent"
	art := contract.Artifact{Type: contract.ArtifactReview, Producer: "mock", Summary: "imported", Verdict: "CHANGES_REQUESTED", Body: "VERDICT: CHANGES_REQUESTED", Metadata: meta}
	// Advisory/imported evidence cannot authenticate itself through metadata.
	got, err := m.UpdateAdvisoryReview(ctx, n, "local-dev", art)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanReviewSequence != 1 {
		t.Fatal("advisory history")
	}
	token := m.token(n)
	m.BindClaim(n, "stale")
	if _, err := m.UpdateArtifact(ctx, n, "local-dev", art); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("stale write: %v", err)
	}
	m.BindClaim(n, token)
	original := m.tracker
	m.tracker = tracker.NewLocalTracker(m.repoRoot).WithUser("outsider", "NONE")
	if _, err := m.RecordHumanReview(ctx, n, "CHANGES_REQUESTED", "forged"); err == nil {
		t.Fatal("unauthorized human accepted")
	}
	m.tracker = original
	got, err = m.GetCurrentState(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanReviewSequence != 1 {
		t.Fatal("failed writes appended history")
	}
}

func TestPlanReviewHistoryReapproveRebase(t *testing.T) {
	m, tr, issue := testManager(t)
	ctx := context.Background()
	// A no-gate task lets this test isolate base identity and persistence from
	// the independently tested carried-review protocol.
	issue.Body = strings.Replace(issue.Body, "gate: human", "gate: none", 1)
	if _, err := tr.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
		t.Fatal(err)
	}
	_, err := m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, m.repoRoot, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("branch", "proposal")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "advance base")
	target := git("rev-parse", "HEAD")
	pr, err := tr.CreateDraftPR(ctx, tracker.DraftPRRequest{IssueNumber: issue.Number, Title: "review", Head: "proposal", Base: git("symbolic-ref", "--short", "HEAD")})
	if err != nil {
		t.Fatal(err)
	}
	st, rev, err := m.load(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	st.PRNumber = pr.Number
	st.EffectiveGate = contract.GateNone
	st.PlanReviewSequence = 5
	st.PlanReviewHistory = []workspace.PlanReviewEvidence{{Round: 5, Findings: "old findings"}}
	if _, err := m.save(ctx, issue.Number, rev, st); err != nil {
		t.Fatal(err)
	}
	got, err := m.ReapproveRebase(ctx, issue.Number, pr.Number, st.BaseSHA, target, pr.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PlanReviewHistory) != 0 || got.PlanReviewSequence != 0 {
		t.Fatal("rebase retained history")
	}
	persisted, err := m.GetCurrentState(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.PlanReviewHistory) != 0 || persisted.PlanReviewSequence != 0 {
		t.Fatal("reset not persisted")
	}
}
