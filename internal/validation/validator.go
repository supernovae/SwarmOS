package validation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
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

type CheckRunOutcome struct {
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
	Reused        bool     `json:"reused,omitempty"`
	Passed        bool     `json:"passed"`
	BaseSHA       string   `json:"base_sha"`
	HeadSHA       string   `json:"head_sha"`
	ModifiedFiles []string `json:"modified_files"`
	// ProtectedChanges lists changed files that match the trusted protected
	// paths when they were reported rather than rejected.
	ProtectedChanges []string          `json:"protected_changes,omitempty"`
	CheckOutcomes    []CheckRunOutcome `json:"check_outcomes"`
	// Repair is the trusted repair summary of a failed interactive
	// validation, including base-rerun classification of failing Go tests.
	Repair   *RepairSummary    `json:"repair,omitempty"`
	Artifact contract.Artifact `json:"artifact"`
}

type ValidateOptions struct {
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
	// GoFastPass first runs go test on the Go packages changed since BaseSHA,
	// and their in-module reverse dependencies, before each required
	// `go test ./...` check. A failure there stops validation early; a pass
	// always continues to the unchanged full check. Ignored in CIMode.
	GoFastPass bool
	// LookupEnv resolves SWARM_EXECUTION_MODE for this validation; nil reads
	// the process environment. Check subprocesses still receive cleanCheckEnv.
	LookupEnv func(string) (string, bool)
	// DependencyPolicy is the node profile's dependency cache policy for
	// this repository. The zero value keeps per-repository caches and allows
	// no private Go modules; CI never shares host caches.
	DependencyPolicy sandbox.DependencyPolicy
}

// Validate loads checks and protected paths exclusively from the immutable base.
func Validate(ctx context.Context, worktreePath string, wu *contract.WorkUnit, opts ValidateOptions) (*ValidationReport, error) {
	if opts.BaseSHA == "" {
		return nil, ErrMissingBaseSHA
	}
	// Include protected-path and artifact inspection in the stability window,
	// so an edit during policy checking cannot earn reusable evidence.
	var before map[string]string
	if !opts.CIMode && opts.Version != "" && opts.ClaimID != "" {
		before, _ = fingerprint(ctx, worktreePath, opts)
	}
	spec, err := loadTrustedValidationSpec(worktreePath, opts.BaseSHA)
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
	headSHA, err := resolveCommit(worktreePath, "HEAD")
	if err != nil {
		return nil, err
	}
	cfg, err := harness.LoadConfigAt(worktreePath, baseSHA)
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
	report := &ValidationReport{Passed: true, BaseSHA: baseSHA, HeadSHA: headSHA, ModifiedFiles: modified, ProtectedChanges: protected}
	var markdown strings.Builder
	markdown.WriteString("# Validation Report\n\n")
	markdown.WriteString(fmt.Sprintf("Trusted base: `%s`  \nCandidate head: `%s`\n\n", baseSHA, headSHA))
	markdown.WriteString("| Check | Command | Status | Duration |\n| :--- | :--- | :--- | :--- |\n")
	var details strings.Builder
	var firstErr error
	stoppedEarly := false
	checkHome, err := os.MkdirTemp("", "swarm-check-home-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(checkHome)
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
	runner := &checkRunner{worktree: worktreePath, home: checkHome, buildCaches: caches, deps: deps, cfg: cfg, execution: execution, runnerIsolated: opts.RunnerIsolated}
	// Only the trusted base writes the persistent caches, and only where a
	// candidate can read them back (sandbox.ValidatorCacheReusable).
	warmNote := ""
	if !opts.RunnerIsolated && execution.Mode == contract.ExecutionIsolated && execution.Backend == contract.BackendNative && sandbox.NativeValidationReusesCache(cfg) {
		warmNote = warmBuildCaches(ctx, runner, spec.RequiredChecks, baseSHA)
	}
	record := func(outcome CheckRunOutcome) {
		report.CheckOutcomes = append(report.CheckOutcomes, outcome)
		status := "PASS"
		if outcome.FallbackReason != "" {
			status = "SKIPPED (" + outcome.FallbackReason + ")"
		} else if !outcome.Passed {
			status = fmt.Sprintf("FAIL (exit %d)", outcome.ExitCode)
		}
		markdown.WriteString(fmt.Sprintf("| `%s` | `%s` | **%s** | `%s` |\n", outcome.Name, outcome.Command, status, outcome.Duration.Round(time.Millisecond)))
		if strings.TrimSpace(outcome.Output) != "" {
			details.WriteString(fmt.Sprintf("\n### Output: `%s`\n```text\n%s\n```\n", outcome.Name, strings.TrimSpace(outcome.Output)))
		}
	}
	fail := func(outcome *CheckRunOutcome, runErr error) {
		report.Passed = false
		if firstErr == nil {
			firstErr = fmt.Errorf("%w: check %q (%s) exited with %d: %v", ErrRequiredCheckFailed, outcome.Name, outcome.Command, outcome.ExitCode, runErr)
		}
	}
	// The changed-package pass only runs for the explicitly requested
	// interactive operations, never for CI or repository validation.
	var fastPass *goFastPass
	if opts.GoFastPass && !opts.CIMode {
		fastPass = &goFastPass{runner: runner, worktree: worktreePath, baseSHA: baseSHA}
	}
	required := 0
	for i, check := range spec.RequiredChecks {
		duration := checkTimeout(check.Timeout)
		if fastPass != nil {
			outcome, stop, runErr := fastPass.run(ctx, check, duration, opts.OnProgress)
			if outcome != nil {
				record(*outcome)
			}
			if fastPass.note != "" {
				details.WriteString(fmt.Sprintf("\nChanged-package pass for `%s` skipped: %s\n", check.Name, fastPass.note))
				fastPass.note = ""
			}
			if stop {
				fail(outcome, runErr)
				if remaining := len(spec.RequiredChecks) - i; remaining > 0 {
					details.WriteString(fmt.Sprintf("\nValidation stopped after the changed-package pass failed; %d required check(s) did not run, including the full `%s`.\n", remaining, check.Name))
				}
				stoppedEarly = true
				break
			}
		}
		required++
		outcome, runErr := runner.runCheck(ctx, check.Name, check.Command, duration, opts.OnProgress)
		if runErr != nil {
			fail(&outcome, runErr)
		}
		record(outcome)
	}
	details.WriteString(warmNote)
	// Diagnosis is coordinator work after the verdict; it never changes it.
	if !report.Passed && !opts.CIMode && ctx.Err() == nil {
		report.Repair = diagnoseRepair(ctx, runner, report)
		if len(report.Repair.Tests) > 0 {
			details.WriteString("\n### Failing tests compared with the approved base\n")
			for _, test := range report.Repair.Tests {
				details.WriteString("- `" + strings.ReplaceAll(testLine(test), "`", "'") + "`\n")
			}
			details.WriteString("\nSee `.swarm/active/REPAIR.md` for the repair brief.\n")
		}
	}
	markdown.WriteString(details.String())
	verdict, summary := "PASS", fmt.Sprintf("All %d required checks passed", required)
	if !report.Passed {
		verdict, summary = "FAIL", "One or more required checks failed"
		if stoppedEarly {
			summary = "Tests for changed packages failed; the full required checks did not run"
		}
	}
	issue := wu.IssueNumber
	if issue <= 0 {
		issue = 1
	}
	report.Artifact = contract.Artifact{Version: 1, ID: fmt.Sprintf("art-%d-validation", issue), IssueNumber: issue, Type: contract.ArtifactValidation, Producer: "swarm:validator", Verdict: verdict, Summary: summary, Body: markdown.String(), CreatedAt: time.Now().UTC(), Metadata: map[string]string{"base_sha": baseSHA, "head_sha": headSHA}}
	if report.Repair != nil {
		evidence, _ := json.Marshal(report.Repair)
		report.Artifact.Metadata[RepairMetadataKey] = string(evidence)
	}
	if report.Passed && before != nil {
		after, e := fingerprint(ctx, worktreePath, opts)
		stable := e == nil
		for k, v := range before {
			if after[k] != v {
				stable = false
			}
		}
		if stable {
			for k, v := range before {
				report.Artifact.Metadata[k] = v
			}
			// Serialize before embedding so evidence does not recursively contain itself.
			evidence, _ := json.Marshal(report)
			report.Artifact.Metadata["validation_report"] = string(evidence)
		}
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
	if firstErr != nil {
		return report, firstErr
	}
	return report, nil
}

func loadTrustedValidationSpec(repoRoot, baseSHA string) (*contract.ValidationSpec, error) {
	spec, err := harness.LoadValidationSpecAt(repoRoot, baseSHA)
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

// checkTimeout caps a configured check timeout at 15 minutes.
func checkTimeout(configured string) time.Duration {
	duration := 15 * time.Minute
	if configured != "" {
		parsed, _ := time.ParseDuration(configured)
		if parsed < duration {
			duration = parsed
		}
	}
	return duration
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
}

// run executes command under ctx, which the caller bounds by duration.
func (r *checkRunner) run(ctx context.Context, command string, duration time.Duration, stdout, stderr io.Writer) error {
	if r.runnerIsolated {
		return r.runOnHost(ctx, command, stdout, stderr)
	} else if r.execution.Mode == contract.ExecutionIsolated && r.execution.Backend == contract.BackendNative {
		return sandbox.RunConfiguredNativeValidation(ctx, r.worktree, r.cfg, command, r.buildCaches, r.deps, r.warmup, stdout, stderr)
	} else if r.execution.Mode == contract.ExecutionIsolated {
		return sandbox.RunWorker(ctx, r.worktree, "/bin/sh", []string{"-c", command}, sandbox.WorkerOptions{
			Runtime: r.execution.ContainerRuntime, Image: r.execution.ContainerImage, Role: "validation", Timeout: duration, DiscardChanges: true,
		}, nil, stdout, stderr)
	}
	return r.runOnHost(ctx, command, stdout, stderr)
}

// runCheck runs one named command with a fresh timeout derived from ctx and
// reports its lifecycle to onProgress.
func (r *checkRunner) runCheck(ctx context.Context, name, command string, duration time.Duration, onProgress func(progress.Event)) (CheckRunOutcome, error) {
	checkCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	if onProgress != nil {
		onProgress(progress.Event{Check: name, Status: "started"})
	}
	start := time.Now()
	var output checkCapture
	runErr := r.run(checkCtx, command, duration, &output, &output)
	outcome := CheckRunOutcome{Name: name, Command: command, Duration: time.Since(start), Output: output.raw.String(), Passed: runErr == nil}
	if runErr != nil {
		outcome.OutputTail, outcome.FailingTests = output.diagnostics()
		outcome.goFailures = output.evidence(command)
		var exitErr *exec.ExitError
		outcome.InfrastructureError = !errors.As(runErr, &exitErr)
		outcome.ExitCode = 1
		if exit, ok := runErr.(*exec.ExitError); ok {
			outcome.ExitCode = exit.ExitCode()
		}
	}
	if onProgress != nil {
		status := "PASS"
		if !outcome.Passed {
			status = "FAIL"
		}
		onProgress(progress.Event{Check: name, Status: status, StageElapsed: outcome.Duration})
	}
	return outcome, runErr
}

// runOnHost runs a required check directly, with a scrubbed environment.
func (r *checkRunner) runOnHost(ctx context.Context, command string, stdout, stderr io.Writer) error {
	caches, err := sandbox.ValidatorCacheMounts(r.buildCaches, sandbox.ValidationOnHost, r.warmup, false, r.home)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = r.worktree
	cmd.Env = cleanCheckEnv(r.home, caches, r.deps)
	process.Configure(cmd)
	cmd.Cancel = func() error { process.Stop(cmd); return nil }
	cmd.WaitDelay = time.Second
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	process.Stop(cmd)
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
	for _, b := range sandbox.HostToolchainsFor(deps.Mode) {
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
