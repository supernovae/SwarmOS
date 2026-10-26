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
	"strings"
	"time"

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
	Artifact         contract.Artifact `json:"artifact"`
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
	execution, err := contract.ResolveExecution(cfg, os.Getenv("SWARM_EXECUTION_MODE"))
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
	checkHome, err := os.MkdirTemp("", "swarm-check-home-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(checkHome)
	for _, check := range spec.RequiredChecks {
		duration := 15 * time.Minute
		if check.Timeout != "" {
			configured, _ := time.ParseDuration(check.Timeout)
			if configured < duration {
				duration = configured
			}
		}
		checkCtx, cancel := context.WithTimeout(ctx, duration)
		if opts.OnProgress != nil {
			opts.OnProgress(progress.Event{Check: check.Name, Status: "started"})
		}
		start := time.Now()
		var output checkCapture
		var runErr error
		if opts.RunnerIsolated {
			runErr = runOnHost(checkCtx, worktreePath, checkHome, check.Command, &output)
		} else if execution.Mode == contract.ExecutionIsolated && execution.Backend == contract.BackendNative {
			runErr = sandbox.RunConfiguredNativeValidation(checkCtx, worktreePath, cfg, check.Command, &output, &output)
		} else if execution.Mode == contract.ExecutionIsolated {
			runErr = sandbox.RunWorker(checkCtx, worktreePath, "/bin/sh", []string{"-c", check.Command}, sandbox.WorkerOptions{
				Runtime: execution.ContainerRuntime, Image: execution.ContainerImage, Role: "validation", Timeout: duration, DiscardChanges: true,
			}, nil, &output, &output)
		} else {
			runErr = runOnHost(checkCtx, worktreePath, checkHome, check.Command, &output)
		}
		cancel()
		outcome := CheckRunOutcome{Name: check.Name, Command: check.Command, Duration: time.Since(start), Output: output.raw.String(), Passed: runErr == nil}
		if runErr != nil {
			outcome.OutputTail, outcome.FailingTests = output.diagnostics()
			var exitErr *exec.ExitError
			outcome.InfrastructureError = !errors.As(runErr, &exitErr)
			report.Passed = false
			outcome.ExitCode = 1
			if exit, ok := runErr.(*exec.ExitError); ok {
				outcome.ExitCode = exit.ExitCode()
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("%w: check %q (%s) exited with %d: %v", ErrRequiredCheckFailed, check.Name, check.Command, outcome.ExitCode, runErr)
			}
		}
		if opts.OnProgress != nil {
			status := "PASS"
			if !outcome.Passed {
				status = "FAIL"
			}
			opts.OnProgress(progress.Event{Check: check.Name, Status: status, StageElapsed: outcome.Duration})
		}
		report.CheckOutcomes = append(report.CheckOutcomes, outcome)
		status := "PASS"
		if !outcome.Passed {
			status = fmt.Sprintf("FAIL (exit %d)", outcome.ExitCode)
		}
		markdown.WriteString(fmt.Sprintf("| `%s` | `%s` | **%s** | `%s` |\n", check.Name, check.Command, status, outcome.Duration.Round(time.Millisecond)))
		if strings.TrimSpace(outcome.Output) != "" {
			details.WriteString(fmt.Sprintf("\n### Output: `%s`\n```text\n%s\n```\n", check.Name, strings.TrimSpace(outcome.Output)))
		}
	}
	markdown.WriteString(details.String())
	verdict, summary := "PASS", fmt.Sprintf("All %d required checks passed", len(report.CheckOutcomes))
	if !report.Passed {
		verdict, summary = "FAIL", "One or more required checks failed"
	}
	issue := wu.IssueNumber
	if issue <= 0 {
		issue = 1
	}
	report.Artifact = contract.Artifact{Version: 1, ID: fmt.Sprintf("art-%d-validation", issue), IssueNumber: issue, Type: contract.ArtifactValidation, Producer: "swarm:validator", Verdict: verdict, Summary: summary, Body: markdown.String(), CreatedAt: time.Now().UTC(), Metadata: map[string]string{"base_sha": baseSHA, "head_sha": headSHA}}
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

// runOnHost runs a required check directly, with a scrubbed environment.
func runOnHost(ctx context.Context, dir, home, command string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = cleanCheckEnv(home)
	process.Configure(cmd)
	cmd.Cancel = func() error { process.Stop(cmd); return nil }
	cmd.WaitDelay = time.Second
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
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

func cleanCheckEnv(home string) []string {
	// Do not inherit a user home, credential helper, publishing token, or SSH
	// agent. Checks get only deterministic process basics.
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "USERPROFILE=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"APPDATA=" + filepath.Join(home, "config"), "LOCALAPPDATA=" + filepath.Join(home, "cache"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	// Preserve public language toolchain and package cache directories outside
	// the scratch HOME, without exposing credential files such as ~/.cargo/credentials.toml,
	// ~/.npmrc, or ~/.pypirc.
	for _, b := range sandbox.HostToolchains() {
		if b.EnvKey != "" && b.EnvVal != "" {
			env = append(env, b.EnvKey+"="+b.EnvVal)
		}
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT", "LANG", "LC_ALL", "TZ"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func gitOutput(repoRoot string, args ...string) ([]byte, error) {
	cmd, redact, err := gitcredential.Command(context.Background(), repoRoot, args...)
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

func matchGlobPattern(pattern, target string) bool {
	pattern, target = filepath.ToSlash(strings.TrimSpace(pattern)), filepath.ToSlash(target)
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return target == prefix || strings.HasPrefix(target, prefix+"/")
	}
	ok, err := filepath.Match(pattern, target)
	return (err == nil && ok) || pattern == target
}
