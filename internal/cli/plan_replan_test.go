package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
	if initial.PlanReplanCount != 0 || initial.PendingPlanReview == nil {
		t.Fatal("initial plan consumed budget or lost rejection")
	}
	for i := 1; i <= 2; i++ {
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
	status := f.must(t, "issue", "status", n)
	if !strings.Contains(status, "Automatic plan retries: 2/2") || !strings.Contains(status, "CHANGES_REQUESTED") {
		t.Fatal(status)
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
	if !errors.Is(err, workflow.ErrPlanReviewRejected) || !strings.Contains(out, "pr work "+strconv.Itoa(number)+" --phase plan") {
		t.Fatalf("PR recovery: %v %s", err, out)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.run(args...); err == nil {
			t.Fatal("unchanged retry succeeded")
		}
	}
	if _, err := f.run(args...); !errors.Is(err, lease.ErrPlanReplanLimit) {
		t.Fatalf("PR cap: %v", err)
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
				if out, err := exec.Command("git", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
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
