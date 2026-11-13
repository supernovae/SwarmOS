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
	return r.validationRoundWithReuse(round, false)
}

// Only a continuation with an eligible PASS may refresh evidence while reusing checks.
func (r *workRun) validationRoundWithReuse(round int, reuseChecks bool) (paused bool, err error) {
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
	if reuseChecks {
		// Capture fresh tester output separately from the reused check report.
		if _, err := recordGuidance(r.wtPath, "VALIDATION.md", "", nil); err != nil {
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
	var report *validation.ValidationReport
	if skip, skipErr := r.skipChecksForUnmetAcceptance(round, reuseChecks, testerBody); skipErr != nil {
		return false, skipErr
	} else if skip != nil {
		report, err = skip, fmt.Errorf("%w: required checks were not run because the tester recorded unmet acceptance criteria and no repair rounds remain", errAcceptanceUnmet)
	} else {
		r.progress.next("required-checks")
	}
	version := r.opts.Version
	if version == "" && (r.opts.Phase == "test" || r.testerResume || (!r.forkMode && r.opts.PRNumber == 0)) {
		version = "dev"
	}
	if report == nil && reuseChecks {
		report, err = validation.Reuse(r.ctx, r.wtPath, validation.ValidateOptions{ForkMode: r.forkMode || r.e.LeaseManager.HasWorkerGateway(), DirectIssueReuse: !r.forkMode && r.opts.PRNumber == 0 && !r.e.LeaseManager.HasWorkerGateway(), Version: r.opts.Version, ClaimID: r.st.Lease.ClaimID, BaseSHA: r.st.BaseSHA}, r.opts.Owner, r.st.Artifacts)
		if err != nil {
			return false, err
		}
	}
	if report == nil {
		report, err = validation.Validate(r.ctx, r.wtPath, r.pre.WorkUnit, validation.ValidateOptions{Version: version, ClaimID: r.st.Lease.ClaimID, BaseSHA: r.st.BaseSHA, ForkMode: r.forkMode, OnProgress: r.progress.observeCheck, LookupEnv: r.e.LookupEnv, DependencyPolicy: r.e.DependencyPolicy})
	}
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
	if err == nil && r.acceptanceRepair().NeedsRepair() {
		return false, errAcceptanceUnmet
	}
	var limit *validation.LimitError
	if errors.As(err, &limit) {
		command := fmt.Sprintf("swarm issue validate %d", r.opts.IssueNumber)
		if r.opts.PRNumber != 0 {
			command = fmt.Sprintf("swarm pr validate %d", r.opts.PRNumber)
		}
		err = fmt.Errorf("%w; rerun the complete required checks with %s", err, command)
	}
	return false, err
}

var errAcceptanceUnmet = errors.New("acceptance criteria unmet")

// skipChecksForUnmetAcceptance returns a failed, checks-skipped report when the
// tester recorded an unmet criterion in the last allowed round. Running the
// required checks then cannot change the outcome: the round fails either way,
// so the full suite would only spend time. The tester evidence is still
// recorded, so the next explicit developer round sees it.
func (r *workRun) skipChecksForUnmetAcceptance(round int, reuseChecks bool, testerBody string) (*validation.ValidationReport, error) {
	if reuseChecks || r.testerResume || round < r.repairLimit || r.forkMode || r.e.LeaseManager.HasWorkerGateway() || r.opts.PRNumber != 0 || r.st.PRNumber != 0 || r.st.PRRevision != nil || r.opts.Phase == "test" {
		return nil, nil
	}
	if !validation.AssessAcceptance(r.pre.WorkUnit.Acceptance, validation.CaptureAcceptanceEvidence(testerBody)).NeedsRepair() {
		return nil, nil
	}
	head, err := gitcredential.Output(r.ctx, r.wtPath, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, fmt.Errorf("resolve candidate head: %w", err)
	}
	return validation.SkippedChecksReport(r.opts.IssueNumber, r.st.BaseSHA, strings.TrimSpace(string(head)), "the tester recorded unmet acceptance criteria and no repair rounds remain"), nil
}

func (r *workRun) acceptanceRepair() validation.AcceptanceAssessment {
	if r.forkMode || r.e.LeaseManager.HasWorkerGateway() || r.opts.PRNumber != 0 || r.st.PRNumber != 0 || r.st.PRRevision != nil || r.opts.Phase == "test" {
		return validation.AcceptanceAssessment{}
	}
	return lease.RepairAcceptance(r.st, r.pre.WorkUnit.Acceptance)
}

func (r *workRun) validateAndRepair() (*WorkResult, error) {
	reused := false
	refreshEvidence := false
	if r.continuation() && r.opts.instructionsFor("developer") == "" && !hasFailedValidationArtifact(r.st) && hasTrustedImplementation(r.st) {
		if err := r.prepareTestRetry(); err == nil {
			r.testerResume = true
			report, reuseErr := validation.Reuse(r.ctx, r.wtPath, validation.ValidateOptions{ForkMode: r.forkMode || r.e.LeaseManager.HasWorkerGateway(), DirectIssueReuse: !r.forkMode && r.opts.PRNumber == 0 && !r.e.LeaseManager.HasWorkerGateway(), Version: r.opts.Version, ClaimID: r.st.Lease.ClaimID, BaseSHA: r.st.BaseSHA}, r.opts.Owner, r.st.Artifacts)
			if reuseErr != nil {
				return r.res, reuseErr
			}
			if report != nil {
				updated, err := r.e.LeaseManager.RecordTrustedValidation(r.ctx, r.opts.IssueNumber, r.opts.Owner, report.Artifact)
				if err != nil {
					return r.res, err
				}
				if _, err := recordGuidance(r.wtPath, "VALIDATION.md", canonicalReport(report.Artifact.Body), nil); err != nil {
					return r.res, err
				}
				r.st, r.res.State = updated, updated
				r.res.Validation, r.res.ValidationPassed = report, true
				assessment := r.acceptanceRepair()
				if assessment.State == "complete" {
					return r.finish()
				}
				reused = assessment.NeedsRepair()
				refreshEvidence = !reused
			}
		} else {
			return r.res, err
		}
	}
	initialRound := 0
	limitContinuation := r.continuation() && r.opts.instructionsFor("developer") == "" && validation.LimitFromArtifacts(r.st.Artifacts, r.res.HeadSHA, r.st.BaseSHA) != ""
	if !limitContinuation && !r.testerResume && (!implementationProven(r.st, r.res.HeadSHA) || r.opts.instructionsFor("developer") != "") {
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
	var paused bool
	var err error
	if reused {
		err = errAcceptanceUnmet
	} else {
		paused, err = r.validationRoundWithReuse(initialRound, refreshEvidence)
	}
	var previous []string
	for round := initialRound + 1; err != nil && !paused && (repairableValidation(err, r.res.Validation) || errors.Is(err, errAcceptanceUnmet)); round++ {
		// Unmet criteria are targets even when checks failed too, so progress
		// and the final message name everything the round had to fix.
		failing := repairTargets(r.res.Validation, r.acceptanceRepair())
		if round > r.repairLimit {
			if r.repairLimit > 0 || errors.Is(err, errAcceptanceUnmet) {
				detail := repairOutcome(failing, previous)
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

func repairTargets(report *validation.ValidationReport, acceptance validation.AcceptanceAssessment) []string {
	failing := failingIdentities(report)
	if acceptance.NeedsRepair() {
		for _, w := range acceptance.Blockers {
			if w.Reason == "unmet" {
				failing = append(failing, fmt.Sprintf("acceptance %d: %s", w.Index, w.Criterion))
			}
		}
	}
	return failing
}

func repairOutcome(failing, previous []string) string {
	if slices.Equal(failing, previous) {
		return "the same targets are still unresolved"
	}
	for _, f := range failing {
		if !slices.Contains(previous, f) {
			return "new failures appeared since the previous round"
		}
	}
	return "targets from the previous round are still unresolved"
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
	if errors.Is(err, lease.ErrValidationRepairLimit) {
		return fmt.Errorf("%w: %w; automatic allowance exhausted; explicit --phase implement or --developer-instructions may continue subject to approval", ErrValidationRepairExhausted, err)
	}
	if err != nil {
		// A lost claim, stale approval or failed state write is not an exhausted
		// allowance; report it as itself so the next step is accurate.
		return err
	}
	r.st, r.res.State = st, st
	r.progress.retry("validation-repair", st.ValidationRepairCount, r.repairLimit)
	return nil
}

func hasTrustedImplementation(st *lease.IssueState) bool {
	bindings := lease.ReviewBindings(st)
	if bindings["plan_digest"] == "" || bindings["test_plan_digest"] == "" {
		return false
	}
	// Scheduling checks input freshness; CheckImplementationRetry remains the
	// authoritative admission check for the claim and candidate checkpoint.
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactImplementation && a.Verdict == "PASS" && a.Metadata["implementation_trusted_origin"] == "coordinator" {
			current := true
			for k, v := range bindings {
				if k != "review_claim_id" && (v == "" || a.Metadata["implementation_trusted_"+k] != v) {
					current = false
					break
				}
			}
			if current {
				return true
			}
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
