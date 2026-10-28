package workflow

import (
	"fmt"
	"strings"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
)

func (r *workRun) plan() (bool, error) {
	var err error
	r.rejectedPlan, r.rejectedTest, r.rejectedFindings = rejectedPlanContext(r.st)
	r.runPlan = r.opts.Phase == "all" || r.opts.Phase == "plan"
	if r.opts.Phase == "all" && (hasDesignAndTestPlanArtifacts(r.st) && !hasRejectedReview(r.st)) {
		r.runPlan = false
	}
	if r.runPlan {
		if err := r.admit("plan"); err != nil {
			return true, err
		}
		r.progress.next("plan")
		if err := r.check(); err != nil {
			return true, err
		}
		if strings.EqualFold(r.cfg.PlanMode, "interactive") {
			r.res.RoleAgentsUsed["planner"] = "interactive"
			if err := compileSetupBundle(r.ctx, r.opts.SetupProgress, r.wtPath, r.pre.WorkUnit, r.wf, r.st, "planner", r.opts.PRNumber); err != nil {
				return true, err
			}
			if r.st, err = r.e.LeaseManager.UpdatePhase(r.ctx, r.opts.IssueNumber, r.opts.Owner, "plan", "active"); err != nil {
				return true, err
			}
			r.res.State = r.st
			r.res.PausedForHuman = true
			r.res.PhaseCompleted = "awaiting-plan"
			return true, nil
		}
		planner := r.pre.Agents["planner"]
		r.res.RoleAgentsUsed["planner"] = planner
		runner, err := r.e.configuredRunner(r.ctx, planner, r.cfg, r.e.Tracker.BackendName() == "local")
		if err != nil {
			return true, err
		}
		if _, err := r.e.LeaseManager.UpdatePhase(r.ctx, r.opts.IssueNumber, r.opts.Owner, "plan", "active"); err != nil {
			return true, err
		}
		if err := compileSetupBundle(r.ctx, r.opts.SetupProgress, r.wtPath, r.pre.WorkUnit, r.wf, r.st, "planner", r.opts.PRNumber); err != nil {
			return true, err
		}
		planPrompt := "Execute /swarm-plan and write .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md."
		if r.opts.instructionsFor("planner") != "" {
			planPrompt += "\nOperator instructions: " + r.opts.instructionsFor("planner")
		}
		if r.rejectedFindings != "" {
			planPrompt += "\nAddress the previous CHANGES_REQUESTED findings and revise PLAN.md or TEST_PLAN.md:\n" + r.rejectedFindings
		}
		if err := r.admit("plan"); err != nil {
			return true, err
		}
		planRun, err := runner.Run(r.ctx, agent.RunRequest{Agent: planner, Role: "planner", Mode: r.mode, WorktreePath: r.wtPath, SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md", TaskPrompt: revisionTaskPrompt(planPrompt, r.st, r.opts.PRNumber), OperatorInstructions: r.opts.instructionsFor("planner"), Dependencies: r.deps})
		if paused, pauseErr := r.pauseInterrupted(err, "plan", false); paused {
			return true, pauseErr
		}
		if err != nil {
			r.st, err = r.e.failedRun(r.ctx, r.st, r.opts, planRun, err, "planner")
			r.res.State = r.st
			return true, err
		}
		plan, err := readRequiredArtifact(r.wtPath, "PLAN.md")
		if err != nil {
			return true, fmt.Errorf("planner agent %q: %w", planner, err)
		}
		testPlan, err := readRequiredArtifact(r.wtPath, "TEST_PLAN.md")
		if err != nil {
			return true, fmt.Errorf("planner agent %q: %w", planner, err)
		}
		if err := r.check(); err != nil {
			return true, err
		}
		if r.rejectedFindings != "" && canonicalReport(plan) == r.rejectedPlan && canonicalReport(testPlan) == r.rejectedTest {
			return true, fmt.Errorf("issue #%d re-plan is unchanged after CHANGES_REQUESTED; revise PLAN.md or TEST_PLAN.md before review", r.opts.IssueNumber)
		}
		// Keep canonical plan files independent of steering evidence in artifacts.
		plan = guidedReport(plan, planRun)
		testPlan = guidedReport(testPlan, planRun)
		observeRun(planRun, r.st, false)
		planMeta := runMetadata(planRun, nil)
		if _, err = r.e.LeaseManager.UpdateArtifact(r.ctx, r.opts.IssueNumber, r.opts.Owner, contract.Artifact{Type: contract.ArtifactDesign, Producer: planner + ":planner", Summary: fmt.Sprintf("Implementation plan for #%d", r.opts.IssueNumber), Body: plan, Metadata: planMeta}); err != nil {
			return true, err
		}
		testMeta := runMetadata(planRun, nil)
		if r.st, err = r.e.LeaseManager.UpdateArtifact(r.ctx, r.opts.IssueNumber, r.opts.Owner, contract.Artifact{Type: contract.ArtifactTestPlan, Producer: planner + ":planner", Summary: fmt.Sprintf("Test plan covering %d acceptance criteria", len(r.pre.WorkUnit.Acceptance)), Body: testPlan, Metadata: testMeta}); err != nil {
			return true, err
		}
		r.res.State, r.res.PhaseCompleted = r.st, "plan"
		if r.stopForSplit() {
			return true, nil
		}
		if done, err := r.finishPlanHandoff(); done {
			return true, err
		}
	}
	return false, nil
}

func (r *workRun) finishPlanHandoff() (bool, error) {
	var err error
	if r.opts.Phase == "plan" {
		if r.opts.Handoff {
			r.progress.next("handoff")
			next := "implement"
			if r.gate != contract.GateNone {
				next = "review"
			}
			if err := r.check(); err != nil {
				return true, err
			}
			if r.st, err = r.e.LeaseManager.Handoff(r.ctx, r.opts.IssueNumber, r.opts.Owner, "plan", next); err != nil {
				return true, err
			}
			r.res.State, r.res.HandedOffToPhase = r.st, next
		}
		return true, nil
	}
	return false, nil
}
