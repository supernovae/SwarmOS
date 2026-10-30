package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/claimpause"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
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
	if r.opts.Phase == "test" || r.testerResume {
		if err := r.checkTestRetry(); err != nil {
			return false, err
		}
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
		if !r.forkMode && r.checkTestRetry() == nil {
			command := fmt.Sprintf("swarm issue work %d --phase test", r.opts.IssueNumber)
			if r.opts.PRNumber != 0 {
				command = fmt.Sprintf("swarm pr work %d --phase test", r.opts.PRNumber)
			}
			return false, &TestRetryAvailableError{Err: fmt.Errorf("rerun with %s: %w", command, err)}
		}
		return false, err
	}
	if err := r.check(); err != nil {
		return false, err
	}
	if err := r.checkWorkApproval(r.ctx); err != nil {
		return false, err
	}
	testerBody, _ := readRequiredArtifact(r.wtPath, "VALIDATION.md")
	if outcome := parseTesterOutcome(testerBody); outcome == testerFail || outcome == testerBlocked {
		command := fmt.Sprintf("swarm issue work %d --phase implement", r.opts.IssueNumber)
		if r.opts.PRNumber != 0 {
			command = fmt.Sprintf("swarm pr work %d --phase implement", r.opts.PRNumber)
		}
		return false, fmt.Errorf("tester declared %s in .swarm/active/VALIDATION.md; complete implementation and rerun with %s", outcome, command)
	}
	r.progress.next("required-checks")
	version := r.opts.Version
	if (r.opts.Phase == "test" || r.testerResume) && version == "" {
		version = "dev"
	}
	report, err := validation.Validate(r.ctx, r.wtPath, r.pre.WorkUnit, validation.ValidateOptions{Version: version, ClaimID: r.st.Lease.ClaimID, BaseSHA: r.st.BaseSHA, ForkMode: r.forkMode, OnProgress: r.progress.observeCheck, LookupEnv: r.e.LookupEnv, DependencyPolicy: r.e.DependencyPolicy})
	if context.Cause(r.ctx) == claimpause.ErrBudget {
		return true, r.e.pauseWork(r.coordinatorCtx, r.res, r.opts, r.pre.WorkUnit, "implement", true, r.forkMode, r.forkRemote)
	}
	if report != nil {
		r.res.Validation = report
		observeRun(testerRun, r.st, false)
		report.Artifact.Metadata = runMetadata(testerRun, report.Artifact.Metadata)
		validation.StoreAcceptanceSnapshot(report.Artifact.Metadata, validation.CaptureAcceptanceEvidence(testerBody))
		report.Artifact.Body += "\n" + testerBody
		report.Artifact.Body = guidedReport(report.Artifact.Body, testerRun)
		if _, writeErr := recordGuidance(r.wtPath, "VALIDATION.md", canonicalReport(report.Artifact.Body), testerRun); writeErr != nil {
			return false, writeErr
		}
		if r.opts.Phase == "test" || r.testerResume {
			if err := r.checkTestRetry(); err != nil {
				return false, err
			}
		}
		record := r.e.LeaseManager.RecordTrustedValidation
		if r.opts.Phase == "test" || r.testerResume {
			record = func(ctx context.Context, n int, owner string, art contract.Artifact) (*lease.IssueState, error) {
				tree, err := workspace.CheckpointTree(ctx, r.wtPath)
				if err != nil {
					return nil, err
				}
				return r.e.LeaseManager.RecordRetryValidation(ctx, n, owner, art, r.res.HeadSHA, tree, r.branch)
			}
		}
		updated, updateErr := record(r.ctx, r.opts.IssueNumber, r.opts.Owner, report.Artifact)
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
	if r.continuation() && r.opts.instructionsFor("developer") == "" && !hasFailedValidationArtifact(r.st) && hasTrustedImplementation(r.st) {
		if err := r.prepareTestRetry(); err == nil {
			r.testerResume = true
			report, reuseErr := validation.Reuse(r.ctx, r.wtPath, validation.ValidateOptions{Version: r.opts.Version, ClaimID: r.st.Lease.ClaimID, BaseSHA: r.st.BaseSHA}, r.opts.Owner, r.st.Artifacts)
			if reuseErr != nil {
				return r.res, reuseErr
			}
			if report != nil {
				r.res.Validation, r.res.ValidationPassed = report, true
				return r.finish()
			}
		} else {
			return r.res, err
		}
	}
	initialRound := 0
	if !r.testerResume && (!implementationProven(r.st, r.res.HeadSHA) || r.opts.instructionsFor("developer") != "") {
		if r.continuation() && hasFailedValidationArtifact(r.st) && r.opts.instructionsFor("developer") == "" {
			if repair := validation.RepairFromArtifact(r.st.Artifacts, r.st.BaseSHA); repair != nil && repair.AllPreExisting {
				return r.res, ErrPreExistingFailures
			}
			if reserveErr := r.reserveRepair(); reserveErr != nil {
				return r.res, reserveErr
			}
			initialRound = 1
		}
		if paused, err := r.developerRound(initialRound, nil); paused || err != nil {
			return r.res, err
		}
	}
	paused, err := r.validationRound(initialRound)
	var previous []string
	for round := initialRound + 1; err != nil && !paused && repairableValidation(err, r.res.Validation); round++ {
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
		if !r.forkMode && r.opts.PRNumber == 0 {
			if reserveErr := r.reserveRepair(); reserveErr != nil {
				return r.res, fmt.Errorf("%w: %w", reserveErr, validationErr)
			}
		}
		r.testerResume = false
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

func (r *workRun) reserveRepair() error {
	if err := r.admit("implement"); err != nil {
		return err
	}
	st, err := r.e.LeaseManager.ReserveValidationRepair(r.ctx, r.opts.IssueNumber, r.opts.Owner)
	if err != nil {
		return fmt.Errorf("%w: %w; run %s", ErrValidationRepairExhausted, err, r.rerun)
	}
	r.st, r.res.State = st, st
	r.progress.retry("validation-repair", st.ValidationRepairCount, r.repairLimit)
	return nil
}

func hasTrustedImplementation(st *lease.IssueState) bool {
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactImplementation && a.Verdict == "PASS" && a.Metadata["implementation_trusted_origin"] == "coordinator" {
			return true
		}
	}
	return false
}
