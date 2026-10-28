package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/claimpause"
	"github.com/supernovae/swarmos/internal/validation"
)

func (r *workRun) validationRound(round int) (paused bool, err error) {
	if err := r.admit("implement"); err != nil {
		return false, err
	}
	r.progress.next("test")
	tester := r.pre.Agents["tester"]
	r.res.RoleAgentsUsed["tester"] = tester
	testerRunner, err := r.e.configuredRunner(r.ctx, tester, r.cfg, r.e.Tracker.BackendName() == "local")
	if err != nil {
		return false, err
	}
	if err := compileSetupBundle(r.ctx, r.opts.SetupProgress, r.wtPath, r.pre.WorkUnit, r.wf, r.st, "tester", r.opts.PRNumber); err != nil {
		return false, err
	}
	if err := ensurePlanArtifactsInWorktree(r.wtPath, r.st); err != nil {
		return false, err
	}
	testPrompt := "Execute /swarm-validate and record acceptance evidence."
	if r.opts.instructionsFor("tester") != "" {
		testPrompt += "\nOperator instructions: " + r.opts.instructionsFor("tester")
	}
	if err := r.admit("implement"); err != nil {
		return false, err
	}
	testerRun, err := testerRunner.Run(r.ctx, agent.RunRequest{CheckImport: r.checkImport("tester"), PreserveRejected: r.preserveRejected("tester"), Agent: tester, Role: "tester", Mode: r.mode, WorktreePath: r.wtPath, SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md", TaskPrompt: testPrompt, OperatorInstructions: r.opts.instructionsFor("tester"), Dependencies: r.deps})
	var rejection *importRejection
	if errors.As(err, &rejection) {
		return false, r.rejectImport(err)
	}
	if paused, pauseErr := r.pauseInterrupted(err, "implement", true); paused {
		return true, pauseErr
	}
	if err != nil {
		r.st, err = r.e.failedRun(r.ctx, r.st, r.opts, testerRun, err, "tester")
		r.res.State = r.st
		return false, err
	}
	if err := r.check(); err != nil {
		return false, err
	}
	r.progress.next("required-checks")
	testerBody, _ := readRequiredArtifact(r.wtPath, "VALIDATION.md")
	report, err := validation.Validate(r.ctx, r.wtPath, r.pre.WorkUnit, validation.ValidateOptions{Version: r.opts.Version, ClaimID: r.st.Lease.ClaimID, BaseSHA: r.st.BaseSHA, ForkMode: r.forkMode, OnProgress: r.progress.observeCheck, GoFastPass: round == 0, LookupEnv: r.e.LookupEnv, DependencyPolicy: r.e.DependencyPolicy})
	if context.Cause(r.ctx) == claimpause.ErrBudget {
		return true, r.e.pauseWork(r.coordinatorCtx, r.res, r.opts, r.pre.WorkUnit, "implement", true, r.forkMode, r.forkRemote)
	}
	if report != nil {
		r.res.Validation = report
		observeRun(testerRun, r.st, false)
		report.Artifact.Metadata = runMetadata(testerRun, report.Artifact.Metadata)
		report.Artifact.Body += "\n" + testerBody
		report.Artifact.Body = guidedReport(report.Artifact.Body, testerRun)
		if _, writeErr := recordGuidance(r.wtPath, "VALIDATION.md", canonicalReport(report.Artifact.Body), testerRun); writeErr != nil {
			return false, writeErr
		}
		updated, updateErr := r.e.LeaseManager.RecordTrustedValidation(r.ctx, r.opts.IssueNumber, r.opts.Owner, report.Artifact)
		if updateErr != nil {
			return false, updateErr
		}
		r.st, r.res.State, r.res.ValidationPassed = updated, updated, report.Passed
		if err != nil && report.Repair != nil && report.Repair.AllPreExisting {
			return false, fmt.Errorf("%w: %w", ErrPreExistingFailures, err)
		}
	}
	return false, err
}

func (r *workRun) validateAndRepair() (*WorkResult, error) {
	if !implementationProven(r.st, r.res.HeadSHA) {
		if paused, err := r.developerRound(0, nil); paused || err != nil {
			return r.res, err
		}
	}
	paused, err := r.validationRound(0)
	var previous []string
	for round := 1; err != nil && !paused && repairableValidation(err, r.res.Validation); round++ {
		failing := failingIdentities(r.res.Validation)
		if round > r.repairLimit {
			if r.repairLimit > 0 {
				detail := "failing tests changed since the previous round"
				if slices.Equal(failing, previous) {
					detail = "the same tests still fail"
				}
				err = fmt.Errorf("issue #%d: %w after %d of %d repair rounds; %s: %s; rerun with %s: %w", r.opts.IssueNumber, ErrValidationRepairExhausted, r.repairLimit, r.repairLimit, detail, boundedList(failing, maxRepairIdentities), r.rerun, err)
			}
			break
		}
		validationErr := err
		if paused, err = r.developerRound(round, boundedTargets(failing)); paused || err != nil {
			if r.outcomeFailed {
				// The latest trusted report still decides the outcome.
				err = fmt.Errorf("%w: %w", err, validationErr)
			}
			return r.res, err
		}
		previous = failing
		paused, err = r.validationRound(round)
	}
	if paused || err != nil {
		return r.res, err
	}
	return r.finish()
}

func (r *workRun) finish() (*WorkResult, error) {
	r.res.PhaseCompleted = "implement"
	return r.res, nil
}
