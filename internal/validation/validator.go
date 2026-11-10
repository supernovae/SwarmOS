package validation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/workspace"
)

var (
	ErrProtectedPathModified = errors.New("protected path modified without approved harness permission")
	ErrMissingArtifact       = errors.New("required plan/test-plan artifact missing")
	ErrRequiredCheckFailed   = errors.New("required validation check failed")
	ErrMissingBaseSHA        = errors.New("trusted validation base SHA is required")
	ErrInvalidPolicy         = errors.New("invalid trusted validation policy")
	ErrUnsafePath            = errors.New("unsafe repository path")
)

// LimitError identifies a failed run that should rerun checks before code repair.
type LimitError struct{ Outcome string }

func (e *LimitError) Error() string {
	return "required checks ended: " + e.Outcome + "; rerun required validation"
}
func (e *LimitError) Unwrap() error { return ErrRequiredCheckFailed }

const LimitMetadataKey = "validation_limit"
const OutcomeTimeLimit = "time-limit"
const OutcomeStalled = "stalled"

func validLimit(s string) bool { return s == OutcomeTimeLimit || s == OutcomeStalled }

// LimitFromArtifacts only accepts the latest current coordinator FAIL.
func LimitFromArtifacts(artifacts []contract.Artifact, head, base string) string {
	for i := len(artifacts) - 1; i >= 0; i-- {
		a := artifacts[i]
		if a.Type != contract.ArtifactValidation {
			continue
		}
		if head != "" && base != "" && a.Verdict == "FAIL" && a.Metadata["validation_trusted_origin"] == "coordinator" && a.Metadata["head_sha"] == head && a.Metadata["base_sha"] == base && validLimit(a.Metadata[LimitMetadataKey]) {
			return a.Metadata[LimitMetadataKey]
		}
		return ""
	}
	return ""
}

type CheckRunOutcome struct {
	Outcome             string        `json:"outcome,omitempty"`
	SkippedTests        []SkippedTest `json:"skipped_tests,omitempty"`
	SkippedTotal        int           `json:"skipped_total,omitempty"`
	OutputTail          string        `json:"output_tail,omitempty"`
	FailingTests        []string      `json:"failing_tests,omitempty"`
	InfrastructureError bool          `json:"infrastructure_error,omitempty"`
	Name                string        `json:"name"`
	Command             string        `json:"command"`
	ExitCode            int           `json:"exit_code"`
	Duration            time.Duration `json:"duration"`
	Output              string        `json:"output"`
	Passed              bool          `json:"passed"`
	// Preliminary marks the changed-package pass that runs before a full
	// go test check. It can fail validation early but never passes a check.
	Preliminary bool     `json:"preliminary,omitempty"`
	Packages    []string `json:"packages,omitempty"`
	// FallbackReason explains a preliminary attempt that did not finish, such
	// as a timeout or sandbox error. The full check decided the result instead.
	FallbackReason string `json:"fallback_reason,omitempty"`
	// goFailures is the raw, package-attributed Go test evidence of a failed
	// check, kept in memory only for repair diagnosis.
	goFailures *goEvidence
}

// goEvidence is a failed check's attributed Go test failures. source is the
// trusted required-check command whose flags a targeted rerun reuses.
type goEvidence struct {
	source       string
	tests        []goFailure
	unattributed []string
	truncated    bool
}

type ValidationReport struct {
	Limit         string        `json:"limit,omitempty"`
	SkippedTests  []SkippedTest `json:"skipped_tests,omitempty"`
	SkippedTotal  int           `json:"skipped_total,omitempty"`
	Reused        bool          `json:"reused,omitempty"`
	Passed        bool          `json:"passed"`
	BaseSHA       string        `json:"base_sha"`
	HeadSHA       string        `json:"head_sha"`
	ModifiedFiles []string      `json:"modified_files"`
	// ProtectedChanges lists changed files that match the trusted protected
	// paths when they were reported rather than rejected.
	ProtectedChanges []string          `json:"protected_changes,omitempty"`
	CheckOutcomes    []CheckRunOutcome `json:"check_outcomes"`
	// Repair is the trusted repair summary of a failed interactive
	// validation, including base-rerun classification of failing Go tests.
	Repair        *RepairSummary     `json:"repair,omitempty"`
	Timings       *ValidationTimings `json:"timings,omitempty"`
	SourceHead    string             `json:"source_head,omitempty"`
	SourceTime    string             `json:"source_time,omitempty"`
	ReuseDuration *time.Duration     `json:"reuse_duration,omitempty"`
	Artifact      contract.Artifact  `json:"artifact"`
}

// PhaseTiming is additive execution evidence, never a reuse eligibility key.
type PhaseTiming struct {
	Duration time.Duration `json:"duration"`
	Status   string        `json:"status"`
}
type ValidationTimings struct {
	Preflight PhaseTiming   `json:"preflight"`
	Warmup    PhaseTiming   `json:"warmup"`
	Checks    PhaseTiming   `json:"checks"`
	Repair    PhaseTiming   `json:"repair"`
	Total     time.Duration `json:"total"`
}

func phaseEvent(observe func(progress.Event), name, status string, elapsed time.Duration) {
	if observe != nil {
		observe(progress.Event{Check: "preparation: " + name, Status: status, StageElapsed: elapsed})
	}
}

// TimingSummary keeps source execution and the current reuse operation distinct.
func (r *ValidationReport) TimingSummary() string {
	var b strings.Builder
	if r.Timings == nil {
		b.WriteString("\nPhase/total timings: unavailable\n")
	} else {
		b.WriteString("\n| Phase | Status | Duration |\n| :--- | :--- | :--- |\n")
		for _, phase := range []struct {
			name   string
			timing PhaseTiming
		}{
			{"preflight", r.Timings.Preflight}, {"warm-up", r.Timings.Warmup}, {"checks", r.Timings.Checks}, {"repair", r.Timings.Repair},
		} {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", phase.name, EscapeDiagnostic(phase.timing.Status), phase.timing.Duration.Round(time.Millisecond))
		}
		fmt.Fprintf(&b, "Total wall time: %s\n", r.Timings.Total.Round(time.Millisecond))
	}
	if r.Reused {
		fmt.Fprintf(&b, "Source claim: %s; candidate tree: %s; policy digest: %s\n", EscapeDiagnostic(r.Artifact.Metadata["validation_source_claim"]), EscapeDiagnostic(r.Artifact.Metadata["validation_tree"]), EscapeDiagnostic(r.Artifact.Metadata["validation_policy"]))
		fmt.Fprintf(&b, "Reused source head: %s; source time: %s; current reuse: %s\n", EscapeDiagnostic(r.SourceHead), EscapeDiagnostic(r.SourceTime), DurationText(r.ReuseDuration))
	}
	return b.String()
}

func reportCell(s string) string {
	return strings.NewReplacer("|", "/", "`", "'", "<", "&lt;", ">", "&gt;").Replace(EscapeDiagnostic(s))
}

func DurationText(d *time.Duration) string {
	if d == nil || *d < 0 {
		return "unavailable"
	}
	return d.Round(time.Millisecond).String()
}

type ValidateOptions struct {
	requiredCheckWorkers int // zero selects the CPU-derived production bound
	runCommand           func(context.Context, string, time.Duration, io.Writer, io.Writer) error
	// DirectIssueReuse permits bounded same-owner cross-claim reuse for direct issues.
	DirectIssueReuse bool
	// Version and ClaimID identify the coordinator validation context. Empty
	// context preserves ordinary validation without reusable evidence.
	Version string
	ClaimID string
	// OnProgress observes trusted check metadata only; callbacks must return promptly.
	OnProgress func(progress.Event)
	CIMode     bool
	// RunnerIsolated runs required checks directly on an ephemeral CI runner,
	// which is the isolation boundary there; agent sandboxes such as Codex's are
	// not installed on stock runners. Policy still comes from BaseSHA.
	RunnerIsolated bool
	// BaseSHA is the immutable approved commit from which policy is loaded and
	// against which committed candidate changes are compared.
	BaseSHA  string
	ForkMode bool
	// ReportProtectedChanges lists protected-path changes in the report instead
	// of rejecting them, then runs the required checks as usual. It is honored
	// only in CIMode, where `swarm repo validate` checks a maintainer-authored
	// pull request at merge time; agent work and fork mode always reject.
	ReportProtectedChanges bool
	// LookupEnv resolves SWARM_EXECUTION_MODE for this validation; nil reads
	// the process environment. Check subprocesses still receive cleanCheckEnv.
	LookupEnv func(string) (string, bool)
	// DependencyPolicy is the node profile's dependency cache policy for
	// this repository. The zero value keeps per-repository caches and allows
	// no private Go modules; CI never shares host caches.
	DependencyPolicy sandbox.DependencyPolicy
}

// CheckCheckpointBoundary uses the validator's approved execution selection.
// Preview resolves availability only; actual admission also probes isolation.
func CheckCheckpointBoundary(ctx context.Context, root, base string, probe bool) (bool, error) {
	if _, err := loadTrustedValidationSpecContext(ctx, root, base); err != nil {
		return false, err
	}
	cfg, err := harness.LoadConfigAtContext(ctx, root, base)
	if err != nil {
		return false, err
	}
	execution, err := contract.ResolveExecution(cfg, os.Getenv("SWARM_EXECUTION_MODE"))
	if err != nil {
		return false, err
	}
	if execution.Mode != contract.ExecutionIsolated {
		return false, nil
	}
	if execution.Backend == contract.BackendNative {
		if err = sandbox.CheckNativeValidationAvailable(cfg); err != nil {
			return true, err
		}
		if probe {
			err = sandbox.CheckNativeValidation(ctx, cfg)
		}
	} else {
		err = sandbox.CheckReadOnlyRuntime("container", execution.ContainerRuntime, execution.ContainerImage)
		if err == nil && probe {
			err = sandbox.CheckWorkerRuntime(ctx, execution.ContainerRuntime, execution.ContainerImage)
		}
	}
	return !probe, err
}

// Validate loads checks and protected paths exclusively from the immutable base.
func Validate(ctx context.Context, worktreePath string, wu *contract.WorkUnit, opts ValidateOptions) (result *ValidationReport, resultErr error) {
	ctx, useScope, scopeErr := storageuse.Ensure(ctx, worktreePath)
	if scopeErr != nil {
		return nil, scopeErr
	}
	defer func() {
		if resultErr == nil && result != nil && result.Passed && !result.Reused && ctx.Err() == nil {
			recordRequiredDurations(ctx, worktreePath, result.CheckOutcomes)
		}
	}()
	defer func() {
		if resultErr != nil && result != nil {
			result.Passed = false
		}
	}()
	if useScope != nil {
		defer useScope.Complete(&resultErr)
	}
	started := time.Now()
	preflightComplete := false
	defer func() {
		if !preflightComplete {
			status := "FAIL"
			if ctx.Err() != nil {
				status = "cancelled"
			}
			phaseEvent(opts.OnProgress, "preflight", status, time.Since(started))
		}
	}()
	phaseEvent(opts.OnProgress, "preflight", "started", 0)
	if opts.BaseSHA == "" {
		return nil, ErrMissingBaseSHA
	}
	// Include protected-path and artifact inspection in the stability window,
	// so an edit during policy checking cannot earn reusable evidence.
	var before map[string]string
	if !opts.CIMode && opts.Version != "" && opts.ClaimID != "" {
		before, _ = fingerprint(ctx, worktreePath, opts)
	}
	spec, err := loadTrustedValidationSpecContext(ctx, worktreePath, opts.BaseSHA)
	if err != nil {
		return nil, err
	}
	baseSHA, err := resolveCommit(worktreePath, opts.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("resolve trusted base: %w", err)
	}
	if wu == nil {
		wu = &contract.WorkUnit{ID: "CI", IssueNumber: 1}
	}
	if !opts.CIMode {
		for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
			data, err := safefs.ReadFile(worktreePath, filepath.Join(".swarm", "active", name), 1<<20)
			if err != nil || len(data) == 0 {
				return nil, fmt.Errorf("%w: %s not found or empty in .swarm/active/", ErrMissingArtifact, name)
			}
		}
	}
	modified, err := collectModifiedFiles(worktreePath, baseSHA)
	if err != nil {
		return nil, err
	}
	if opts.ReportProtectedChanges && (!opts.CIMode || opts.ForkMode) {
		return nil, errors.New("protected-path reporting is only available for repository validation outside fork mode")
	}
	if opts.ForkMode {
		for _, file := range modified {
			if file == ".swarm" || strings.HasPrefix(file, ".swarm/") || file == ".github/workflows" || strings.HasPrefix(file, ".github/workflows/") {
				return nil, fmt.Errorf("%w: file %q cannot be modified in fork mode", ErrProtectedPathModified, file)
			}
		}
	}
	// Authorization for explicit protected-path exceptions is checked by the
	// approval state machine before this call. This boolean is only its input.
	var protected []string
	if !wu.AllowHarnessChanges {
		for _, file := range modified {
			for _, pattern := range spec.ProtectedPaths {
				if matchGlobPattern(pattern, file) {
					if !opts.ReportProtectedChanges {
						return nil, fmt.Errorf("%w: file %q matches protected pattern %q", ErrProtectedPathModified, file, pattern)
					}
					protected = append(protected, file)
					break
				}
			}
		}
	}
	var checkpointTree string
	var restoreInstructions func() error
	if !opts.CIMode {
		restore, err := workspace.PrepareValidationInstructions(worktreePath)
		if err != nil {
			return nil, fmt.Errorf("prepare validation instructions: %v", err)
		}
		restoreInstructions = restore
		defer func() {
			if restoreInstructions == nil {
				return
			}
			if err := restoreInstructions(); err != nil {
				// A restoration error is infrastructure failure, never automatic repair.
				result = nil
				resultErr = fmt.Errorf("restore validation instructions: %v", err)
			}
		}()
		checkpointTree, _ = workspace.CheckpointTree(ctx, worktreePath)
	}
	headSHA, err := resolveCommit(worktreePath, "HEAD")
	if err != nil {
		return nil, err
	}
	if err := harness.ValidateTrackerConfig(worktreePath); err != nil {
		return nil, fmt.Errorf("candidate tracker configuration: %w", err)
	}
	cfg, err := harness.LoadConfigAtContext(ctx, worktreePath, baseSHA)
	if err != nil {
		return nil, fmt.Errorf("load approved execution policy: %w", err)
	}
	mode := os.Getenv("SWARM_EXECUTION_MODE")
	if opts.LookupEnv != nil {
		mode, _ = opts.LookupEnv("SWARM_EXECUTION_MODE")
	}
	execution, err := contract.ResolveExecution(cfg, mode)
	if err != nil {
		return nil, err
	}
	report := &ValidationReport{Timings: &ValidationTimings{}, Passed: true, BaseSHA: baseSHA, HeadSHA: headSHA, ModifiedFiles: modified, ProtectedChanges: protected}
	var markdown strings.Builder
	markdown.WriteString("# Validation Report\n\n")
	markdown.WriteString(fmt.Sprintf("Trusted base: `%s`  \nCandidate head: `%s`\n\n", baseSHA, headSHA))
	markdown.WriteString("| Check | Command | Status | Duration |\n| :--- | :--- | :--- | :--- |\n")
	var details strings.Builder
	var firstErr error
	checkHome, err := storageuse.MkdirTemp(ctx, "", "swarm-check-home-*", "validation-scratch")
	if err != nil {
		return nil, err
	}
	defer storageuse.FromContext(ctx).CleanupTemp(checkHome, &resultErr)
	// Resolve the validator's build and dependency caches once. OCI checks
	// get none. Dependency caches never fall back, so a failed prefetch
	// stops validation before any check runs.
	var caches []sandbox.BuildCache
	var deps sandbox.Dependencies
	if opts.RunnerIsolated || execution.Mode != contract.ExecutionIsolated || execution.Backend == contract.BackendNative {
		if caches, err = validatorBuildCaches(ctx, worktreePath, checkHome); err != nil {
			return nil, err
		}
		if deps, err = validatorDependencyCaches(ctx, worktreePath, baseSHA, opts.DependencyPolicy); err != nil {
			return nil, fmt.Errorf("validation preflight: %w", err)
		}
	}
	runner := &checkRunner{worktree: worktreePath, home: checkHome, buildCaches: caches, deps: deps, cfg: cfg, execution: execution, runnerIsolated: opts.RunnerIsolated, runCommand: opts.runCommand}
	// Only the trusted base writes the persistent caches, and only where a
	// candidate can read them back (sandbox.ValidatorCacheReusable).
	report.Timings.Preflight = PhaseTiming{Duration: time.Since(started), Status: "ran"}
	preflightComplete = true
	phaseEvent(opts.OnProgress, "preflight", "PASS", report.Timings.Preflight.Duration)
	warmNote := ""
	report.Timings.Warmup.Status = "not-applicable"
	if !opts.RunnerIsolated && execution.Mode == contract.ExecutionIsolated && execution.Backend == contract.BackendNative && sandbox.NativeValidationReusesCache(cfg) {
		warmNote = warmBuildCaches(ctx, runner, spec.RequiredChecks, baseSHA, &report.Timings.Warmup, opts.OnProgress)
	}
	record := func(outcome CheckRunOutcome) {
		if outcome.Passed && !outcome.Preliminary {
			report.SkippedTotal += outcome.SkippedTotal
			for _, skip := range outcome.SkippedTests {
				if len(report.SkippedTests) < 60 {
					skip.Status = skipStatus(ctx, worktreePath, baseSHA, modified, skip)
					report.SkippedTests = append(report.SkippedTests, skip)
				}
			}
		}
		report.CheckOutcomes = append(report.CheckOutcomes, outcome)
		status := "PASS"
		if outcome.FallbackReason != "" {
			status = "SKIPPED (" + outcome.FallbackReason + ")"
		} else if !outcome.Passed {
			status = fmt.Sprintf("FAIL (exit %d)", outcome.ExitCode)
			if validLimit(outcome.Outcome) {
				status = outcome.Outcome
			}
		}
		markdown.WriteString(fmt.Sprintf("| `%s` | `%s` | **%s** | `%s` |\n", reportCell(outcome.Name), reportCell(outcome.Command), status, outcome.Duration.Round(time.Millisecond)))
		if strings.TrimSpace(outcome.Output) != "" {
			details.WriteString(fmt.Sprintf("\n### Output: `%s`\n```text\n%s\n```\n", outcome.Name, strings.TrimSpace(outcome.Output)))
		}
	}
	fail := func(outcome *CheckRunOutcome, runErr error) {
		report.Passed = false
		if validLimit(outcome.Outcome) {
			report.Limit = outcome.Outcome
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%w: check %q (%s) exited with %d: %v", ErrRequiredCheckFailed, outcome.Name, outcome.Command, outcome.ExitCode, runErr)
		}
	}
	required := len(spec.RequiredChecks)
	checksStart := time.Now()
	jobs := make([]requiredCheckJob, required)
	for i, check := range spec.RequiredChecks {
		jobs[i] = requiredCheckJob{check.Name, check.Command, requiredCheckBudget(ctx, worktreePath, check)}
	}
	workers := opts.requiredCheckWorkers
	if workers == 0 {
		workers = requiredCheckParallelism(required, runtime.NumCPU())
	}
	runner.runRequiredChecks(ctx, jobs, workers, opts.OnProgress, func(outcome CheckRunOutcome, runErr error) {
		if runErr != nil {
			fail(&outcome, runErr)
		}
		record(outcome)
	})
	if ctx.Err() != nil {
		report.Passed = false
	}
	report.Timings.Checks = PhaseTiming{Duration: time.Since(checksStart), Status: "PASS"}
	if !report.Passed {
		report.Timings.Checks.Status = "FAIL"
	}
	report.Timings.Repair.Status = "not-applicable"
	details.WriteString(warmNote)
	// Diagnosis is coordinator work after the verdict; it never changes it.
	if !report.Passed && report.Limit == "" && !opts.CIMode && ctx.Err() == nil {
		repairStart := time.Now()
		phaseEvent(opts.OnProgress, "repair", "started", 0)
		report.Repair = diagnoseRepairTree(ctx, runner, report, checkpointTree)
		report.Timings.Repair = PhaseTiming{Duration: time.Since(repairStart), Status: "ran"}
		phaseEvent(opts.OnProgress, "repair", "PASS", report.Timings.Repair.Duration)
		for _, check := range report.Repair.FailedChecks {
			if check.OutsideCandidateTree != "" {
				fmt.Fprintf(&details, "\nCheck %s passed in checkpoint tree %s; its original validation result remains FAIL.\n", check.Name, check.OutsideCandidateTree)
			}
		}
		if len(report.Repair.Tests) > 0 {
			details.WriteString("\n### Failing tests compared with the approved base\n")
			for _, test := range report.Repair.Tests {
				details.WriteString("- `" + strings.ReplaceAll(testLine(test), "`", "'") + "`\n")
			}
			details.WriteString("\nSee `.swarm/active/REPAIR.md` for the repair brief.\n")
		}
	}
	if restoreInstructions != nil {
		restore := restoreInstructions
		restoreInstructions = nil
		if err := restore(); err != nil {
			return nil, fmt.Errorf("restore validation instructions: %v", err)
		}
	}
	stable := false
	if report.Passed && before != nil {
		after, e := fingerprint(ctx, worktreePath, opts)
		stable = e == nil
		for k, v := range before {
			if after[k] != v {
				stable = false
			}
		}
	}
	report.Timings.Total = time.Since(started)
	markdown.WriteString(report.TimingSummary())
	renderSkips(&details, report)
	markdown.WriteString(details.String())
	verdict, summary := "PASS", fmt.Sprintf("All %d required checks passed", required)
	if !report.Passed {
		verdict, summary = "FAIL", "One or more required checks failed"
	}
	issue := wu.IssueNumber
	if issue <= 0 {
		issue = 1
	}
	report.Artifact = contract.Artifact{Version: 1, ID: fmt.Sprintf("art-%d-validation", issue), IssueNumber: issue, Type: contract.ArtifactValidation, Producer: "swarm:validator", Verdict: verdict, Summary: summary, Body: markdown.String(), CreatedAt: time.Now().UTC(), Metadata: map[string]string{"base_sha": baseSHA, "head_sha": headSHA}}
	if report.Limit != "" {
		report.Artifact.Metadata[LimitMetadataKey] = report.Limit
	}
	if report.Repair != nil {
		report.Artifact.Metadata["validation_checkpoint_tree"] = checkpointTree
		evidence, _ := json.Marshal(report.Repair)
		report.Artifact.Metadata[RepairMetadataKey] = string(evidence)
	}
	timingReport := *report
	timingReport.Artifact = contract.Artifact{CreatedAt: report.Artifact.CreatedAt}
	timingReport.Repair = nil
	timingReport.SkippedTests = nil
	timingReport.CheckOutcomes = make([]CheckRunOutcome, len(report.CheckOutcomes))
	for i, c := range report.CheckOutcomes {
		timingReport.CheckOutcomes[i] = CheckRunOutcome{Name: c.Name, Command: c.Command, Outcome: c.Outcome, Passed: c.Passed, ExitCode: c.ExitCode, Duration: c.Duration, Preliminary: c.Preliminary}
	}
	timingEvidence, _ := json.Marshal(timingReport)
	report.Artifact.Metadata["validation_timing_report"] = string(timingEvidence)
	if stable {
		for k, v := range before {
			report.Artifact.Metadata[k] = v
		}
		evidence, _ := json.Marshal(report)
		report.Artifact.Metadata["validation_report"] = string(evidence)
	}
	if !opts.CIMode {
		if err := safefs.WriteFile(worktreePath, filepath.Join(".swarm", "active", "VALIDATION.md"), []byte(markdown.String()), 0o644); err != nil {
			return nil, err
		}
		// A passing validation clears any earlier brief.
		if err := WriteRepairBrief(worktreePath, report.Repair); err != nil {
			return nil, err
		}
	}
	if ctx.Err() != nil {
		return report, errors.Join(ctx.Err(), firstErr)
	}
	if report.Limit != "" {
		return report, &LimitError{Outcome: report.Limit}
	}
	if firstErr != nil {
		return report, firstErr
	}
	return report, nil
}

func loadTrustedValidationSpec(repoRoot, baseSHA string) (*contract.ValidationSpec, error) {
	return loadTrustedValidationSpecContext(context.Background(), repoRoot, baseSHA)
}
func loadTrustedValidationSpecContext(ctx context.Context, repoRoot, baseSHA string) (*contract.ValidationSpec, error) {
	spec, err := harness.LoadValidationSpecAtContext(ctx, repoRoot, baseSHA)
	if err != nil {
		return nil, fmt.Errorf("%w: load validation policy at %s: %v", ErrInvalidPolicy, baseSHA, err)
	}
	for _, pattern := range spec.ProtectedPaths {
		if err := checkProtectedPattern(pattern); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidPolicy, err)
		}
	}
	return spec, nil
}

func collectModifiedFiles(worktreePath, baseSHA string) ([]string, error) {
	// Inspect tracked control paths before excluding generated artifacts. A
	// committed link or file at the active directory is never ephemeral output.
	entries, err := gitOutput(worktreePath, "ls-files", "--stage", "-z", "--", ".swarm", ".swarm/active")
	if err != nil {
		return nil, err
	}
	for _, entry := range bytes.Split(entries, []byte{0}) {
		parts := bytes.SplitN(entry, []byte{'\t'}, 2)
		if len(parts) != 2 {
			continue
		}
		name := string(parts[1])
		if name == ".swarm" || name == ".swarm/active" || (isEphemeral(name) && bytes.HasPrefix(parts[0], []byte("120000"))) {
			return nil, fmt.Errorf("%w: tracked control path %q", ErrUnsafePath, name)
		}
	}
	commands := [][]string{{"diff", "--name-only", "-z", baseSHA + "...HEAD"}, {"diff", "--cached", "--name-only", "-z"}, {"diff", "--name-only", "-z"}, {"ls-files", "--others", "--exclude-standard", "-z"}}
	seen := make(map[string]struct{})
	recorded := make(map[string]bool) // committed or staged, never ephemeral
	var files []string
	for i, args := range commands {
		out, err := gitOutput(worktreePath, args...)
		if err != nil {
			return nil, err
		}
		for _, raw := range bytes.Split(out, []byte{0}) {
			if len(raw) == 0 {
				continue
			}
			path := string(raw)
			if err := safeRepoPath(worktreePath, path); err != nil {
				return nil, err
			}
			if isEphemeral(path) {
				continue
			}
			if i < 2 {
				recorded[path] = true
			} else if !recorded[path] {
				pointerOnly, err := workspace.OnlyActivePointer(worktreePath, path)
				if err != nil {
					return nil, err
				}
				if pointerOnly {
					continue
				}
			}
			if _, ok := seen[path]; !ok {
				seen[path] = struct{}{}
				files = append(files, path)
			}
		}
	}
	return files, nil
}

// defaultCheckTimeout applies when a check names no timeout; maxCheckTimeout
// bounds any configured value so a stuck check still ends.
const (
	defaultCheckTimeout = 15 * time.Minute
	maxCheckTimeout     = time.Hour
)

// checkTimeout returns the check's configured timeout, up to maxCheckTimeout.
func checkTimeout(configured string) time.Duration {
	parsed, err := time.ParseDuration(configured)
	if configured == "" || err != nil || parsed <= 0 {
		return defaultCheckTimeout
	}
	return min(parsed, maxCheckTimeout)
}

// checkRunner runs commands inside the execution boundary selected by the
// approved policy. Required checks, Go package discovery, and the
// changed-package pass all share it.
type checkRunner struct {
	worktree, home string
	// buildCaches are the validator's caches. warmup marks the trusted base
	// run, the only one sandbox.ValidatorCacheMounts lets write them.
	buildCaches    []sandbox.BuildCache
	warmup         bool
	deps           sandbox.Dependencies // read-only in every run
	cfg            *contract.SwarmConfig
	execution      contract.ExecutionConfig
	runnerIsolated bool
	runCommand     func(context.Context, string, time.Duration, io.Writer, io.Writer) error
	runOCI         func(context.Context, string, time.Duration, io.Writer, io.Writer) error
	// Per-run observer seams; defaults are Linux procfs, one second, five minutes.
	stallWindow     time.Duration
	sampleInterval  time.Duration
	sampleProcesses func() (map[int]checkProcess, error)
	now             func() time.Time
}

// run executes command under ctx, which the caller bounds by duration.
func (r *checkRunner) run(ctx context.Context, command string, duration time.Duration, stdout, stderr io.Writer) error {
	if r.runCommand != nil {
		return r.runCommand(ctx, command, duration, stdout, stderr)
	}
	if r.runnerIsolated {
		return r.runOnHost(ctx, command, stdout, stderr)
	} else if r.execution.Mode == contract.ExecutionIsolated && r.execution.Backend == contract.BackendNative {
		if runtime.GOOS != "linux" {
			return sandbox.RunConfiguredNativeValidation(ctx, r.worktree, r.cfg, command, r.buildCaches, r.deps, r.warmup, stdout, stderr)
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		marker := "swarm-validation-" + hex.EncodeToString(nonce[:])
		wrapped := nativeCheckSupervisor(command, marker)
		observedCtx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		stop := r.observeCPU(observedCtx, cancel, 0, marker)
		defer stop()
		err := sandbox.RunConfiguredNativeValidation(observedCtx, r.worktree, r.cfg, wrapped, r.buildCaches, r.deps, r.warmup, stdout, stderr)
		if errors.Is(context.Cause(observedCtx), errCheckStalled) && ctx.Err() == nil && err != nil {
			return errors.Join(err, errCheckStalled)
		}
		return err
	} else if r.execution.Mode == contract.ExecutionIsolated {
		if r.runOCI != nil {
			return r.runOCI(ctx, command, duration, stdout, stderr)
		}
		return sandbox.RunWorker(ctx, r.worktree, "/bin/sh", []string{"-c", command}, sandbox.WorkerOptions{
			Runtime: r.execution.ContainerRuntime, Image: r.execution.ContainerImage, Role: "validation", Timeout: duration, DiscardChanges: true,
		}, nil, stdout, stderr)
	}
	return r.runOnHost(ctx, command, stdout, stderr)
}

type requiredCheckJob struct {
	name, command string
	budget        time.Duration
}

func requiredCheckParallelism(checks, cpus int) int {
	return min(checks, max(1, cpus))
}

// runRequiredChecks joins all workers before returning. Only this coordinator
// presents results; each worker owns one indexed slot at a time.
func (r *checkRunner) runRequiredChecks(ctx context.Context, jobs []requiredCheckJob, workers int, observe func(progress.Event), record func(CheckRunOutcome, error)) {
	if len(jobs) == 0 {
		return
	}
	workers = requiredCheckParallelism(len(jobs), workers)
	type result struct {
		outcome CheckRunOutcome
		err     error
		done    chan struct{}
	}
	results := make([]result, len(jobs))
	for i := range results {
		results[i].done = make(chan struct{})
	}
	started := func(i int) {
		if observe != nil {
			observe(progress.Event{Check: jobs[i].name, Status: "started"})
		}
	}
	started(0)
	queue := make(chan int, len(jobs))
	for i := range jobs {
		queue <- i
	}
	close(queue)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				job := jobs[i]
				outcome, err := r.runCheck(ctx, job.name, job.command, job.budget, nil)
				if err == nil && ctx.Err() != nil {
					err = ctx.Err()
					outcome.Passed, outcome.ExitCode, outcome.Outcome = false, 1, "failure"
				}
				results[i].outcome, results[i].err = outcome, err
				close(results[i].done)
			}
		}()
	}
	for i := range jobs {
		if i > 0 {
			started(i)
		}
		<-results[i].done
		outcome := results[i].outcome
		if observe != nil {
			observe(checkTerminalEvent(outcome))
		}
		record(outcome, results[i].err)
	}
	wg.Wait()
}

func checkTerminalEvent(outcome CheckRunOutcome) progress.Event {
	status := "PASS"
	if !outcome.Passed {
		status = "FAIL"
		if validLimit(outcome.Outcome) {
			status = outcome.Outcome
		}
	}
	return progress.Event{Check: outcome.Name, Status: status, StageElapsed: outcome.Duration}
}

// runCheck runs one named command with a fresh timeout derived from ctx and
// reports its lifecycle to onProgress.
func (r *checkRunner) runCheck(ctx context.Context, name, command string, duration time.Duration, onProgress func(progress.Event)) (result CheckRunOutcome, resultErr error) {
	ctx, owner, err := r.storageScope(ctx)
	if owner != nil {
		defer func() {
			if resultErr != nil {
				result.Passed = false
			}
		}()
		defer owner.Complete(&resultErr)
	}

	if err != nil {
		return CheckRunOutcome{Name: name, Command: command, InfrastructureError: true}, err
	}

	checkCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	if onProgress != nil {
		onProgress(progress.Event{Check: name, Status: "started"})
	}
	start := time.Now()
	actual, output := r.goCapture(command)
	runErr := r.run(checkCtx, actual, duration, output, output)
	output.finish()
	outcome := CheckRunOutcome{Name: name, Command: command, Duration: time.Since(start), Output: output.raw.String(), Passed: runErr == nil}
	if runErr != nil {
		outcome.Outcome = "failure"
		if ctx.Err() == nil && errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			outcome.Outcome = OutcomeTimeLimit
		} else if ctx.Err() == nil && errors.Is(runErr, errCheckStalled) {
			outcome.Outcome = OutcomeStalled
		}
	}
	if runErr == nil {
		outcome.SkippedTests = output.skips
		outcome.SkippedTotal = output.skipTotal
	}
	if runErr != nil {
		outcome.OutputTail, outcome.FailingTests = output.diagnostics()
		outcome.goFailures = output.evidence(command)
		var exitErr *exec.ExitError
		outcome.InfrastructureError = !errors.As(runErr, &exitErr)
		outcome.ExitCode = 1
		if exitErr != nil {
			outcome.ExitCode = exitErr.ExitCode()
		}
	}
	if onProgress != nil {
		onProgress(checkTerminalEvent(outcome))
	}
	return outcome, runErr
}

// runOnHost runs a required check directly, with a scrubbed environment.
func (r *checkRunner) runOnHost(ctx context.Context, command string, stdout, stderr io.Writer) error {
	if scope := storageuse.FromContext(ctx); scope != nil {
		if err := scope.OutsideConsumer(ctx); err != nil {
			return err
		}
	}
	caches, err := sandbox.ValidatorCacheMounts(r.buildCaches, sandbox.ValidationOnHost, r.warmup, false, r.home)
	if err != nil {
		return err
	}
	observedCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	cmd := exec.CommandContext(observedCtx, "sh", "-c", command)
	cmd.Dir = r.worktree
	cmd.Env = cleanCheckEnv(r.home, caches, r.deps)
	process.Configure(cmd)
	cmd.Cancel = func() error { process.Stop(cmd); return nil }
	cmd.WaitDelay = time.Second
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Start()
	if err == nil {
		stop := r.observeCPU(observedCtx, cancel, cmd.Process.Pid, "")
		err = cmd.Wait()
		stop()
		if ctx.Err() == nil && err != nil && errors.Is(context.Cause(observedCtx), errCheckStalled) {
			err = errors.Join(err, errCheckStalled)
		}
	}
	stopErr := stopValidationProcess(cmd)
	if stopErr != nil {
		if scope := storageuse.FromContext(ctx); scope != nil {
			scope.Retain()
		}
	}
	if stopErr != nil {
		return errors.Join(err, stopErr)
	}
	return err
}

func isEphemeral(path string) bool {
	return path == ".swarm/active" || strings.HasPrefix(path, ".swarm/active/")
}

func safeRepoPath(root, path string) error {
	if path == "" || filepath.IsAbs(path) {
		return fmt.Errorf("%w: %q", ErrUnsafePath, path)
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q", ErrUnsafePath, path)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	candidate := filepath.Join(rootAbs, clean)
	if rel, err := filepath.Rel(rootAbs, candidate); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q", ErrUnsafePath, path)
	}
	for part := candidate; ; part = filepath.Dir(part) {
		if info, err := os.Lstat(part); err == nil && info.Mode()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(part)
			if err != nil {
				return fmt.Errorf("%w: resolve symlink %q: %v", ErrUnsafePath, path, err)
			}
			rel, err := filepath.Rel(rootAbs, target)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%w: symlink escape %q", ErrUnsafePath, path)
			}
		}
		if part == rootAbs {
			break
		}
	}
	return nil
}

func cleanCheckEnv(home string, caches []sandbox.BuildCache, deps sandbox.Dependencies) []string {
	// Do not inherit a user home, credential helper, publishing token, or SSH
	// agent. Checks get only deterministic process basics.
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"APPDATA=" + filepath.Join(home, "config"), "LOCALAPPDATA=" + filepath.Join(home, "cache"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	// Preserve public language toolchain and package cache directories outside
	// the scratch HOME, without exposing credential files such as ~/.cargo/credentials.toml,
	// ~/.npmrc, or ~/.pypirc. Adapter-backed ecosystems use the validator's
	// per-repository dependency caches instead of the host-wide ones.
	for _, b := range sandbox.HostToolchainsFor(deps) {
		if b.EnvKey != "" && !sandbox.IsBuildCacheKey(b.EnvKey) && b.EnvVal != "" {
			env = append(env, b.EnvKey+"="+b.EnvVal)
		}
	}
	// Build caches are SwarmOS-owned validator caches or scratch, never the
	// user's own GOCACHE or CCACHE_DIR.
	env = append(env, sandbox.BuildCacheEnv(caches)...)
	env = append(env, sandbox.DependencyCacheEnv(deps.Caches)...)
	for _, key := range []string{"TMPDIR", "TMP", "TEMP", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT", "LANG", "LC_ALL", "TZ"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func gitOutput(repoRoot string, args ...string) ([]byte, error) {
	return gitOutputContext(context.Background(), repoRoot, args...)
}

func gitOutputContext(ctx context.Context, repoRoot string, args ...string) ([]byte, error) {
	cmd, redact, err := gitcredential.Command(ctx, repoRoot, args...)
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, redact(strings.TrimSpace(stderr.String())))
	}
	return out, nil
}

func resolveCommit(repoRoot, revision string) (string, error) {
	out, err := gitOutput(repoRoot, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// MatchProtectedPath reports whether target matches a protected_paths pattern.
func MatchProtectedPath(pattern, target string) bool { return matchGlobPattern(pattern, target) }

// matchGlobPattern matches like filepath.Match, except that a whole "**"
// path component matches zero or more directories and a trailing "/**" matches
// a directory and everything below it.
func matchGlobPattern(pattern, target string) bool {
	pattern, target = filepath.ToSlash(strings.TrimSpace(pattern)), filepath.ToSlash(target)
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		if target == prefix || strings.HasPrefix(target, prefix+"/") {
			return true
		}
	}
	if ok, err := filepath.Match(pattern, target); (err == nil && ok) || pattern == target {
		return true
	}
	parts := strings.Split(pattern, "/")
	if !slices.Contains(parts, "**") {
		return false
	}
	return matchComponents(parts, strings.Split(target, "/"))
}

// matchComponents memoizes (pattern, target) positions so repeated "**"
// components stay polynomial.
func matchComponents(pattern, target []string) bool {
	failed := make(map[[2]int]bool)
	var match func(p, t int) bool
	match = func(p, t int) bool {
		if p == len(pattern) {
			return t == len(target)
		}
		if failed[[2]int{p, t}] {
			return false
		}
		if pattern[p] == "**" {
			for next := t; next <= len(target); next++ {
				if match(p+1, next) {
					return true
				}
			}
		} else if t < len(target) {
			if ok, err := filepath.Match(pattern[p], target[t]); err == nil && ok && match(p+1, t+1) {
				return true
			}
		}
		failed[[2]int{p, t}] = true
		return false
	}
	return match(0, 0)
}

// checkProtectedPattern rejects filepath.Match syntax errors anywhere in a
// pattern. filepath.Match itself stops parsing at the first mismatch, so it
// accepts patterns such as "a*b[" when matched against a short target.
func checkProtectedPattern(pattern string) error {
	p := filepath.ToSlash(strings.TrimSpace(pattern))
	for len(p) > 0 {
		switch p[0] {
		case '\\':
			if runtime.GOOS != "windows" {
				if len(p) < 2 {
					return fmt.Errorf("protected path pattern %q: %w", pattern, filepath.ErrBadPattern)
				}
				p = p[2:]
				continue
			}
		case '[':
			rest, err := skipClass(p[1:])
			if err != nil {
				return fmt.Errorf("protected path pattern %q: %w", pattern, err)
			}
			p = rest
			continue
		}
		p = p[1:]
	}
	return nil
}

// skipClass follows filepath.Match's character class grammar and returns the
// pattern after the closing bracket.
func skipClass(p string) (string, error) {
	p = strings.TrimPrefix(p, "^")
	for n := 0; ; n++ {
		if n > 0 && strings.HasPrefix(p, "]") {
			return p[1:], nil
		}
		var err error
		if p, err = skipClassChar(p); err != nil {
			return "", err
		}
		if strings.HasPrefix(p, "-") {
			if p, err = skipClassChar(p[1:]); err != nil {
				return "", err
			}
		}
	}
}

func skipClassChar(p string) (string, error) {
	if p == "" || p[0] == '-' || p[0] == ']' {
		return "", filepath.ErrBadPattern
	}
	if p[0] == '\\' && runtime.GOOS != "windows" {
		p = p[1:]
	}
	r, n := utf8.DecodeRuneInString(p)
	if (r == utf8.RuneError && n <= 1) || len(p) == n {
		return "", filepath.ErrBadPattern
	}
	return p[n:], nil
}

func renderSkips(b *strings.Builder, report *ValidationReport) {
	if report.SkippedTotal == 0 {
		return
	}
	fmt.Fprintf(b, "\n### Skipped Go tests\n\n%d test skip events across passing required checks (excluding package skips, preliminary runs and cache warm-up). CI is the next place sandbox-limited tests can run.\n\n", report.SkippedTotal)
	for _, s := range report.SkippedTests {
		// Indented code keeps Markdown from interpreting test-controlled text.
		line := EscapeDiagnostic(s.Package + "." + s.Test + " [" + s.Status + "]: " + s.Message)
		fmt.Fprintf(b, "    %s\n", line)
	}
	if n := report.SkippedTotal - len(report.SkippedTests); n > 0 {
		fmt.Fprintf(b, "\n%d additional skip events omitted.\n", n)
	}
}

// stopValidationProcess is separate from command outcome so verified failure or
// cancellation can finish use while an uncertain stop retains durable evidence.
var stopValidationProcess = process.StopVerified

func (r *checkRunner) storageScope(ctx context.Context) (context.Context, *storageuse.Scope, error) {
	ctx, owner, err := storageuse.Ensure(ctx, r.worktree)
	if err != nil {
		return ctx, owner, err
	}
	if owner == nil {
		return ctx, nil, nil
	}
	scope := storageuse.FromContext(ctx)
	for _, c := range r.buildCaches {
		if c.Persistent {
			if err := scope.Acquire(ctx, storageuse.ResourceFor(scope.Repository(), "validate-build", c.Dir)); err != nil {
				return ctx, owner, err
			}
		}
	}
	if r.deps.Mode != sandbox.SharedHostCaches {
		for _, c := range r.deps.Caches {
			if err := scope.Acquire(ctx, storageuse.ResourceFor(scope.Repository(), "validate-deps", c.Dir)); err != nil {
				return ctx, owner, err
			}
		}
	}
	return ctx, owner, nil
}

var errCheckStalled = errors.New("required check CPU stalled")

type checkProcess struct {
	pid, parent int
	start, cpu  uint64
	args        []string
}

// A waiting supervisor retains its marker while the original shell executes.
// The original command is an argument, not interpolated into supervisor code.
func nativeCheckSupervisor(command, marker string) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	return "/bin/sh -c " + quote(`/bin/sh -c "$1" & child=$!; wait "$child"`) + " " + quote(marker) + " " + quote(command)
}
func checkProcesses() (map[int]checkProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	result := make(map[int]checkProcess)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			return nil, errors.New("invalid proc stat")
		}
		fields := strings.Fields(string(raw[end+1:]))
		if len(fields) < 20 {
			return nil, errors.New("short proc stat")
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		start, err := strconv.ParseUint(fields[19], 10, 64)
		if err != nil {
			return nil, err
		}
		cpu := uint64(0)
		for _, index := range []int{11, 12, 13, 14} {
			value, err := strconv.ParseUint(fields[index], 10, 64)
			if err != nil || cpu+value < cpu {
				return nil, errors.New("invalid proc CPU")
			}
			cpu += value
		}
		args, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result[pid] = checkProcess{pid: pid, parent: parent, start: start, cpu: cpu, args: strings.Split(string(args), "\x00")}
	}
	return result, nil
}
func checkDescendant(tree map[int]checkProcess, pid, root int) bool {
	seen := make(map[int]bool)
	for pid != 0 && !seen[pid] {
		if pid == root {
			return true
		}
		seen[pid] = true
		p, ok := tree[pid]
		if !ok {
			return false
		}
		pid = p.parent
	}
	return false
}

// Observation is advisory. Any sampling error disables termination for the
// invocation; deadlines and verified process cleanup remain authoritative.
func cpuObservationEnabled(platform string, execution contract.ExecutionConfig, runnerIsolated bool) bool {
	return platform == "linux" && (runnerIsolated || execution.Mode != contract.ExecutionIsolated || execution.Backend == contract.BackendNative)
}
func (r *checkRunner) observeCPU(ctx context.Context, cancel context.CancelCauseFunc, root int, marker string) func() {
	if !cpuObservationEnabled(runtime.GOOS, r.execution, r.runnerIsolated) {
		return func() {}
	}
	sample := r.sampleProcesses
	if sample == nil {
		sample = checkProcesses
	}
	now := r.now
	if now == nil {
		now = time.Now
	}
	interval, window := r.sampleInterval, r.stallWindow
	if interval <= 0 {
		interval = time.Second
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	// Capture direct identity before cmd.Wait can reap the shell and allow
	// its PID to be reused while inherited output pipes are still open.
	var pinnedStart uint64
	if root != 0 {
		tree, err := sample()
		if err != nil {
			return func() {}
		}
		p, ok := tree[root]
		if !ok || p.start == 0 {
			return func() {}
		}
		pinnedStart = p.start
	}
	done, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		previous := make(map[[2]uint64]uint64)
		rootStart := pinnedStart
		var last time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
			}
			tree, err := sample()
			if err != nil {
				return
			}
			// A partial ancestry snapshot cannot establish which CPU belongs
			// to this check. Disable observation instead of counting zero.
			for pid, p := range tree {
				if p.pid != pid || p.start == 0 {
					return
				}
				if p.parent != 0 {
					if _, ok := tree[p.parent]; !ok {
						return
					}
				}
			}
			if marker != "" {
				found := 0
				for pid, p := range tree {
					if !checkDescendant(tree, pid, os.Getpid()) {
						continue
					}
					// Require the shell's positional marker, never substring matches in
					// launcher arguments containing a copy of the whole command.
					if len(p.args) >= 4 && p.args[1] == "-c" && p.args[3] == marker {
						if found != 0 {
							return
						}
						found = pid
					}
				}
				if found == 0 {
					if root != 0 {
						return
					}
					continue
				}
				if root != 0 && root != found {
					return
				}
				root = found
			}
			parent, ok := tree[root]
			if !ok {
				return
			}
			if rootStart != 0 && parent.start != rootStart {
				return
			}
			rootStart = parent.start
			progress := false
			for pid, p := range tree {
				if !checkDescendant(tree, pid, root) {
					continue
				}
				id := [2]uint64{uint64(pid), p.start}
				old, known := previous[id]
				if known && p.cpu < old {
					return
				}
				if p.cpu > old {
					progress = true
				}
				previous[id] = p.cpu
				if len(previous) > 100000 {
					return
				}
			}
			current := now()
			if last.IsZero() || progress {
				last = current
			}
			if current.Sub(last) >= window {
				cancel(errCheckStalled)
				return
			}
		}
	}()
	return func() { close(done); <-joined }
}
