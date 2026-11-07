package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/claimpause"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/gitcredential"
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
	if err := compileSetupBundleWithCapabilities(r.ctx, r.opts.SetupProgress, r.wtPath, r.pre.WorkUnit, r.wf, r.st, "tester", r.opts.PRNumber, r.roleCapabilityReport("tester")); err != nil {
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
			return false, &TestRetryAvailableError{Err: err}
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
	if !r.forkMode && r.opts.PRNumber == 0 && (r.opts.Phase == "test" || r.testerResume) {
		if guardErr := r.checkTestRetry(); guardErr != nil {
			return false, r.retryCandidateError(guardErr, testerBody)
		}
	}
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
			if guardErr := r.checkTestRetry(); guardErr != nil {
				if !r.forkMode && r.opts.PRNumber == 0 {
					return false, errors.Join(r.retryCandidateError(guardErr, testerBody), err,
						fmt.Errorf("failing checks: %s", boundedList(failingIdentities(report), maxRepairIdentities)))
				}
				return false, guardErr
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
		if err != nil && report.Repair != nil && report.Repair.OutsideCandidate() {
			return false, fmt.Errorf("%w: %w", ErrOutsideCandidateFailures, err)
		}
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
			if repair := validation.RepairFromArtifact(r.st.Artifacts, r.st.BaseSHA); repair != nil && repair.OutsideCandidate() {
				return r.res, fmt.Errorf("%w: %w", ErrOutsideCandidateFailures, validation.ErrRequiredCheckFailed)
			}
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
				err = fmt.Errorf("issue #%d: %w after %d of %d repair rounds; %s: %s; automatic allowance exhausted; explicit --phase implement or --developer-instructions may continue subject to approval: %w", r.opts.IssueNumber, ErrValidationRepairExhausted, r.repairLimit, r.repairLimit, detail, boundedList(failing, maxRepairIdentities), err)
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
		return fmt.Errorf("%w: %w; automatic allowance exhausted; explicit --phase implement or --developer-instructions may continue subject to approval", ErrValidationRepairExhausted, err)
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

// A rejected retry report must never drive automatic repair, even if checks failed.
var errRetryCandidateRejected = errors.New("tester retry candidate rejected")

func (r *workRun) retryCandidateError(cause error, finding string) error {
	detail := "changed paths unavailable"
	cmd, _, err := gitcredential.Command(r.ctx, r.wtPath, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err == nil {
		if out, statusErr := cmd.Output(); statusErr == nil {
			detail = "changed paths: " + boundedList(retryChangedPaths(string(out)), maxRepairIdentities)
		}
	}
	// Keep a bounded finding in the diagnostic as well as the local artifact:
	// a subsequent developer setup may refresh local role artifacts.
	finding = strings.TrimSpace(finding)
	if len(finding) > 1024 {
		finding = finding[:1024] + "..."
	}
	return fmt.Errorf("%w: %w; %s; tester finding retained in .swarm/active/VALIDATION.md (%s); run swarm issue work %d --phase implement",
		errRetryCandidateRejected, cause, detail, strconv.Quote(finding), r.opts.IssueNumber)
}

func retryChangedPaths(status string) []string {
	entries := strings.Split(status, "\x00")
	seen := map[string]bool{}
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		seen[strconv.Quote(entry[3:])] = true
		if strings.ContainsAny(entry[:2], "RC") && i+1 < len(entries) {
			i++
			seen[strconv.Quote(entries[i])] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
