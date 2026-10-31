package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/validation"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workflow"
)

func TestIssuePlanReplanCapAndRecovery(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	out, err := f.run("issue", "work", n, "--agent", "mock", "--progress=false")
	if !errors.Is(err, workflow.ErrPlanReviewRejected) || !strings.Contains(out, "issue work "+n+" --phase plan") || !strings.Contains(out, "review art-") {
		t.Fatalf("recovery: %v %s", err, out)
	}
	initial := f.state(t, issue)
	for _, a := range initial.Artifacts {
		if a.Type == "implementation" || a.Version != 1 {
			t.Fatalf("more than one round or implementation after rejection: %+v", a)
		}
	}
	if initial.PlanReplanCount != 1 || initial.PendingPlanReview == nil {
		t.Fatal("unchanged automatic replan did not consume budget or lost rejection")
	}
	for i := 2; i <= 2; i++ {
		out, err = f.run("issue", "work", n, "--agent", "mock", "--progress=false")
		if err == nil || !strings.Contains(err.Error(), "re-plan is unchanged") {
			t.Fatalf("retry %d: %v %s", i, err, out)
		}
		if st := f.state(t, issue); st.PlanReplanCount != i {
			t.Fatalf("count = %d", st.PlanReplanCount)
		}
	}
	out, err = f.run("issue", "work", n, "--agent", "mock", "--progress=false")
	if !errors.Is(err, lease.ErrPlanReplanLimit) || !strings.Contains(err.Error(), "review-plan") {
		t.Fatalf("cap: %v %s", err, out)
	}
	assertPlanReplanCommands326(t, f, issue, err.Error())
	status := f.must(t, "issue", "status", n)
	if !strings.Contains(status, "Automatic plan retries: 2/2") || !strings.Contains(status, "CHANGES_REQUESTED") {
		t.Fatal(status)
	}
	if !strings.Contains(status, "operator decision needed: true") || strings.Contains(status, "Next phase: plan") {
		t.Fatal(status)
	}
	var statusJSON struct {
		Issues []struct {
			PlanReview *planReviewDisplay `json:"plan_review"`
			State      *lease.IssueState  `json:"state"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(f.must(t, "issue", "status", n, "--json")), &statusJSON); err != nil {
		t.Fatal(err)
	}
	if len(statusJSON.Issues) != 1 || statusJSON.Issues[0].PlanReview == nil || !statusJSON.Issues[0].PlanReview.OperatorDecision || statusJSON.Issues[0].State.Lease.NextPhase == "plan" {
		t.Fatalf("exhausted JSON: %+v", statusJSON)
	}
	// Display suppression must not change authoritative scheduling state.
	if f.state(t, issue).PlanReplanCount != 2 {
		t.Fatal("status mutated retry count")
	}
	// The capped issue is released so the node must skip it and run another.
	f.must(t, "issue", "release", n)
	f.must(t, "node", "run", "--agent", "mock", "--max-jobs", "1")
	other := f.create(t, "agent")
	f.must(t, "issue", "approve", strconv.Itoa(other))
	f.setenv("SWARM_MOCK_VERDICT", "APPROVED")
	f.must(t, "node", "run", "--agent", "mock", "--max-jobs", "1")
	if f.state(t, other).Lease.ClaimID == "" {
		t.Fatal("node did not advance past capped issue")
	}
	if f.state(t, issue).PlanReplanCount != 2 {
		t.Fatal("node retried capped issue")
	}
	// Revised reports can still be reviewed after the final reservation, even
	// when a reviewer subsequently fails and clears its scheduling hint.
	st := f.state(t, issue)
	// The node left this issue released: acquire only to write the revised report.
	token, err := lease.LoadClaimToken(f.root, issue)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.BindClaim(issue, token)
	st, err = f.manager.Acquire(context.Background(), issue, st.Lease.Owner, "mock", "review", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.UpdateArtifact(context.Background(), issue, st.Lease.Owner, contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "revised", Body: "revised plan after final retry"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.UpdatePhase(context.Background(), issue, st.Lease.Owner, "review", "failed"); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(f.must(t, "issue", "status", n, "--json")), &statusJSON); err != nil {
		t.Fatal(err)
	}
	d := statusJSON.Issues[0].PlanReview
	if d.OperatorDecision || d.NextPhase != "review" || statusJSON.Issues[0].State.Lease.NextPhase != "review" {
		t.Fatalf("unreviewed final reports: %+v", d)
	}

}

func TestRejectedReviewJSONRecovery(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	cmd := f.command(nil)
	var out, diagnostic bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostic)
	cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "issue", "work", n, "--agent", "mock", "--progress=false", "--json"})
	err := cmd.Execute()
	if !errors.Is(err, workflow.ErrPlanReviewRejected) {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, out.String())
	}
	if value["plan_review"] == nil {
		t.Fatalf("missing round fields: %s", out.String())
	}
	if recovery, ok := value["recovery"].(string); !ok || !strings.Contains(recovery, "issue work "+n+" --phase plan") {
		t.Fatalf("missing recovery: %s", out.String())
	}
}

func TestPRPlanReplanCap(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent")
	f.setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	args := []string{"pr", "work", strconv.Itoa(number), "--agent", "mock", "--progress=false"}
	out, err := f.run(args...)
	if !errors.Is(err, workflow.ErrPlanReviewRejected) || !strings.Contains(out+fmt.Sprint(err), "pr work "+strconv.Itoa(number)+" --phase plan") {
		t.Fatalf("PR recovery: %v %s", err, out)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.run(args...); err == nil {
			t.Fatal("unchanged retry succeeded")
		}
	}
	if _, err := f.run(args...); !errors.Is(err, lease.ErrPlanReplanLimit) {
		t.Fatalf("PR cap: %v", err)
	} else {
		assertPlanReplanCommands326(t, f, issue, err.Error(), true)
		if strings.Contains(err.Error(), "swarm issue review-plan "+strconv.Itoa(number)+" ") {
			t.Fatal(err)
		}
	}
	if f.state(t, issue).PlanReplanCount != 2 {
		t.Fatal("PR attempts not persisted")
	}
}

func TestManualStageResetsPlanBudget(t *testing.T) {
	t.Parallel()
	for _, resource := range []string{"issue", "pr"} {
		t.Run(resource, func(t *testing.T) {
			var f *cliFixture
			var issue, number int
			if resource == "pr" {
				f, issue, number = submittedPRFixture(t, "agent")
			} else {
				f = newCLIFixture(t)
				issue = f.create(t, "agent")
				number = issue
				f.must(t, "issue", "approve", strconv.Itoa(issue))
			}
			f.setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
			args := []string{resource, "work", strconv.Itoa(number), "--agent", "mock", "--progress=false"}
			if _, err := f.run(args...); !errors.Is(err, workflow.ErrPlanReviewRejected) {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if _, err := f.run(args...); err == nil {
					t.Fatal("unchanged plan accepted")
				}
			}
			if f.state(t, issue).PlanReplanCount != 2 {
				t.Fatal("budget not exhausted")
			}
			// Staging shares the non-blocking lock held by automatic work.
			lock, err := lease.LockClaim(f.root, issue)
			if err != nil {
				t.Fatal(err)
			}
			_, stageErr := f.run(resource, "stage", strconv.Itoa(number), "--phase", "plan")
			lease.UnlockClaim(lock)
			if stageErr == nil || f.state(t, issue).PlanReplanCount != 2 {
				t.Fatal("competing stage reset budget")
			}
			f.must(t, resource, "stage", strconv.Itoa(number), "--phase", "plan")
			if f.state(t, issue).PlanReplanCount != 0 {
				t.Fatal("successful stage did not reset budget")
			}
		})
	}
}

func TestInteractiveWorkIgnoresExhaustedAutomaticBudget(t *testing.T) {
	t.Parallel()
	for _, resource := range []string{"issue", "node"} {
		t.Run(resource, func(t *testing.T) {
			f := newCLIFixture(t)
			path := filepath.Join(f.root, ".swarm/config.yaml")
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(content), "plan_mode: auto", "plan_mode: interactive", 1)), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "."}, {"commit", "-m", "interactive planning"}} {
				if out, err := gitfixture.Command(t, "", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
					t.Fatal(err, string(out))
				}
			}
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			st := f.state(t, issue)
			st.PlanReplanCount = 2
			st.PendingPlanReview = &lease.PlanRejection{ReviewID: "legacy-review", Version: 1, Digest: "legacy", TaskDigest: st.TaskDigest, PolicyDigest: st.PolicyDigest, BaseSHA: st.BaseSHA}
			st.NextPhaseReason = "review legacy-review returned CHANGES_REQUESTED"
			st.Lease.NextPhase = "plan"
			ctx := context.Background()
			_, rev, err := f.manager.Store().Load(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.manager.Store().CompareAndSwap(ctx, issue, rev, data); err != nil {
				t.Fatal(err)
			}
			if resource == "issue" {
				out := f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
				if !strings.Contains(out, "Interactive plan required") {
					t.Fatal(out)
				}
			} else {
				f.must(t, "node", "run", "--agent", "mock", "--max-jobs", "1")
			}
			after := f.state(t, issue)
			if after.PlanReplanCount != 2 || after.Lease.ClaimID == "" || len(after.Attestations) != 0 {
				t.Fatalf("interactive work consumed or skipped exhausted budget: %+v", after)
			}
		})
	}
}

// Check the suggested alternatives against the real registered Cobra tree.
func assertPlanReplanCommands326(t *testing.T, f *cliFixture, issue int, guidance string, commandFree ...bool) {
	t.Helper()
	for _, decision := range []string{"reject", "approve"} {
		args := []string{"issue", "review-plan", strconv.Itoa(issue), "--" + decision, "--notes", "Reviewed reports"}
		expected := "swarm " + strings.Join(args[:4], " ")
		if len(commandFree) > 0 && commandFree[0] {
			expected = map[string]string{"reject": "request a revised plan", "approve": "approve the current reports"}[decision]
		}
		if !strings.Contains(guidance, expected) {
			t.Fatal(guidance)
		}
		cmd, rest, err := f.command(nil).Find(args)
		if err != nil || cmd.Name() != "review-plan" {
			t.Fatalf("resolve: %v", err)
		}
		if err := cmd.ParseFlags(rest); err != nil {
			t.Fatal(err)
		}
		for name, kind := range map[string]string{"reject": "bool", "approve": "bool", "notes": "string"} {
			flag := cmd.Flags().Lookup(name)
			if flag == nil || flag.Value.Type() != kind {
				t.Fatalf("incorrect registered flag %s", name)
			}
		}
	}
}

func TestPlanReplanHumanRejectionRecovery326(t *testing.T) {
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	f.run("issue", "work", n, "--agent", "mock", "--progress=false")
	for i := 0; i < 2; i++ {
		f.run("issue", "work", n, "--agent", "mock", "--progress=false")
	}
	before := f.state(t, issue)
	if before.PlanReplanCount != 2 {
		t.Fatal("not exhausted")
	}
	for _, args := range [][]string{
		{"--fork", "issue", "review-plan", n, "--reject"},
		{"--fork", "issue", "review-plan", n, "--approve"},
		{"issue", "review-plan", n, "--reject", "--approve"},
	} {
		if _, err := f.run(args...); err == nil {
			t.Fatalf("accepted forbidden decision: %v", args)
		}
		if !reflect.DeepEqual(before, f.state(t, issue)) {
			t.Fatal("refusal changed state")
		}
	}
	for _, decision := range []string{"--reject", "--approve"} {
		cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: f.tracker.WithUser("outsider", "NONE")})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"issue", "review-plan", n, decision})
		if err := cmd.Execute(); err == nil {
			t.Fatal("unauthorized decision accepted")
		}
		if !reflect.DeepEqual(before, f.state(t, issue)) {
			t.Fatal("unauthorized decision changed state")
		}
	}
	f.setenv("SWARM_USER", "independent-maintainer")
	f.must(t, "issue", "review-plan", n, "--reject", "--notes", "Revise reports")
	rejected := f.state(t, issue)
	if rejected.PlanReplanCount != 0 || rejected.PendingPlanReview != nil || rejected.NextPhaseReason != "" || rejected.Lease.NextPhase != "" {
		t.Fatal("human rejection did not clear retry state")
	}
	bindings := lease.ReviewBindings(before)
	for _, a := range rejected.Artifacts {
		if a.Type == "implementation" {
			t.Fatal("rejection implemented")
		}
		if a.Type == "review" {
			if a.Verdict != "CHANGES_REQUESTED" || a.Metadata["review_source"] != "authenticated-human" {
				t.Fatal(a)
			}
			for _, k := range []string{"task_digest", "policy_digest", "plan_digest", "test_plan_digest", "base_sha"} {
				if a.Metadata[k] != bindings[k] {
					t.Fatalf("incorrect binding %s", k)
				}
			}
		}
	}
	f.setenv("SWARM_USER", "alice")
	if _, err := f.run("issue", "work", n, "--phase", "implement", "--agent", "mock", "--progress=false"); err == nil {
		t.Fatal("rejection approved implementation")
	}
	t.Setenv("SWARM_MOCK_PLAN_APPEND", "Revised recovery plan")
	f.must(t, "issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false")
	if lease.ReviewBindings(f.state(t, issue))["plan_digest"] == bindings["plan_digest"] {
		t.Fatal("plan did not change")
	}
	if _, err := f.run("issue", "work", n, "--phase", "implement", "--agent", "mock", "--progress=false"); err == nil {
		t.Fatal("revised reports bypassed review")
	}
	f.setenv("SWARM_USER", "independent-maintainer")
	f.must(t, "issue", "review-plan", n, "--approve")
	f.setenv("SWARM_USER", "alice")
	t.Setenv("SWARM_MOCK_PLAN_APPEND", "Another revised recovery plan")
	f.must(t, "issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false")
	if _, err := f.run("issue", "work", n, "--phase", "implement", "--agent", "mock", "--progress=false"); err == nil {
		t.Fatal("approval carried to replacement reports")
	}
}

func TestCLIStatusNoPendingPlanReview(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	for _, reason := range []string{"", "implementation recovery guidance"} {
		st := f.state(t, issue)
		if !lease.DirectPlanReview(st) || st.PendingPlanReview != nil {
			t.Fatal("fixture must be a direct issue without pending review")
		}
		if reason != "" {
			st.Lease.NextPhase = "implement"
			st.NextPhaseReason = reason
		}
		_, rev, err := f.manager.Store().Load(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.manager.Store().CompareAndSwap(t.Context(), issue, rev, data); err != nil {
			t.Fatal(err)
		}
		out := f.must(t, "issue", "status", n)
		for _, command := range []string{"swarm issue review-plan", "swarm issue stage"} {
			if strings.Contains(out, command) {
				t.Fatalf("unsolicited recovery command %q: %s", command, out)
			}
		}
		if reason != "" && strings.Count(out, "Next phase: implement ("+reason+")\n") != 1 {
			t.Fatalf("missing reason-bearing next phase: %s", out)
		}
		var result struct {
			Issues []issueStatus `json:"issues"`
		}
		if err := json.Unmarshal([]byte(f.must(t, "issue", "status", n, "--json")), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Issues) != 1 {
			t.Fatalf("unexpected issues: %+v", result)
		}
		entry := result.Issues[0]
		if entry.PlanReview == nil || entry.PlanReview.OperatorDecision || len(entry.PlanReview.RecoveryCommands) != 0 {
			t.Fatalf("unexpected plan recovery: %+v", entry.PlanReview)
		}
		if entry.State.Lease.NextPhase != st.Lease.NextPhase || entry.PlanReview.NextPhase != st.Lease.NextPhase || entry.State.NextPhaseReason != reason {
			t.Fatalf("scheduling guidance changed: %+v", entry)
		}
	}
}

func TestPlanReviewRoundDisplay(t *testing.T) {
	for _, mode := range []string{"", "auto", "agent", "interactive"} {
		for _, count := range []int{0, 2} {
			t.Run("no-pending/"+mode+strconv.Itoa(count), func(t *testing.T) {
				st := &lease.IssueState{PlanReplanCount: count}
				st.Lease.NextPhase = "implement"
				d := derivePlanReviewDisplay(st, mode, 2)
				if d.OperatorDecision || len(d.RecoveryCommands) != 0 || d.NextPhase != "implement" {
					t.Fatalf("unexpected recovery without pending review: %+v", d)
				}
			})
		}
	}
	for _, mode := range []string{"", "auto", "agent", "interactive"} {
		for _, limit := range []int{0, 2} {
			t.Run(mode+strconv.Itoa(limit), func(t *testing.T) {
				st := &lease.IssueState{TaskDigest: "task", PolicyDigest: "policy", BaseSHA: "base", PlanReplanCount: limit}
				st.Lease.IssueNumber = 375
				st.Lease.NextPhase = "plan"
				art := contract.Artifact{ID: "review", Version: 1, Type: contract.ArtifactReview, Verdict: "CHANGES_REQUESTED", Body: "VERDICT: CHANGES_REQUESTED"}
				art.Metadata = lease.ReviewBindings(st)
				art.Metadata["review_source"] = "authenticated-agent"
				sum := sha256.Sum256([]byte(art.Body))
				st.PendingPlanReview = &lease.PlanRejection{ReviewID: art.ID, Version: 1, Digest: hex.EncodeToString(sum[:]), TaskDigest: "task", PolicyDigest: "policy", BaseSHA: "base"}
				st.Artifacts = []contract.Artifact{art}
				d := derivePlanReviewDisplay(st, mode, limit)
				exhausted := mode != "interactive"
				if d.OperatorDecision != exhausted || (exhausted && d.NextPhase == "plan") {
					t.Fatalf("display: %+v", d)
				}
				if strings.Contains(strings.Join(d.RecoveryCommands, ";"), "issue stage") != (mode != "agent") {
					t.Fatal("invalid staging advice")
				}
				// A failed final planner may clear NextPhase without resolving the rejection.
				st.Lease.NextPhase = ""
				if derivePlanReviewDisplay(st, mode, limit).OperatorDecision != exhausted {
					t.Fatal("phase hid exhaustion")
				}
				for _, field := range []string{"id", "version", "digest", "task", "policy", "base", "binding", "reports"} {
					copyState := *st
					p := *st.PendingPlanReview
					copyState.PendingPlanReview = &p
					switch field {
					case "id":
						p.ReviewID = "other"
					case "version":
						p.Version++
					case "digest":
						p.Digest = "other"
					case "task":
						p.TaskDigest = "other"
					case "policy":
						p.PolicyDigest = "other"
					case "base":
						p.BaseSHA = "other"
					case "binding":
						a := art
						a.Metadata = map[string]string{"review_source": "authenticated-agent"}
						copyState.Artifacts = []contract.Artifact{a}
					case "reports":
						copyState.Artifacts = nil
					}
					d := derivePlanReviewDisplay(&copyState, mode, limit)
					if d.OperatorDecision || d.NextPhase != "review" {
						t.Fatalf("%s: %+v", field, d)
					}
				}
			})
		}
	}
}

func TestPlanReviewRoundFailureEnvelope(t *testing.T) {
	for _, direct := range []bool{true, false} {
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		err := &planRecoveryError{err: workflow.ErrPlanReviewRejected, recovery: "review current reports"}
		if direct {
			err.display = &planReviewDisplay{Round: 3, RetriesUsed: 2, RetriesAllowed: 2, OperatorDecision: true, RecoveryCommands: []string{"review current reports"}}
		}
		reportJSONFailureWithReport(cmd, true, "work", err, &validation.ValidationReport{})
		var value map[string]any
		if e := json.Unmarshal(out.Bytes(), &value); e != nil {
			t.Fatal(e)
		}
		if value["kind"] != "work" || value["status"] != "failed" || value["error"] != workflow.ErrPlanReviewRejected.Error() || value["recovery"] != err.recovery || value["report"] == nil {
			t.Fatal(value)
		}
		if (value["plan_review"] != nil) != direct {
			t.Fatal("changed excluded envelope", value)
		}
	}
}
