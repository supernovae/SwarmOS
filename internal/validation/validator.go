package validation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

var (
	ErrProtectedPathModified = errors.New("protected path modified without allow_harness_changes")
	ErrMissingArtifact       = errors.New("required plan/test-plan artifact missing")
	ErrRequiredCheckFailed   = errors.New("required validation check failed")
)

type CheckRunOutcome struct {
	Name     string        `json:"name"`
	Command  string        `json:"command"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration"`
	Output   string        `json:"output"`
	Passed   bool          `json:"passed"`
}

type ValidationReport struct {
	Passed        bool              `json:"passed"`
	ModifiedFiles []string          `json:"modified_files"`
	CheckOutcomes []CheckRunOutcome `json:"check_outcomes"`
	Artifact      contract.Artifact `json:"artifact"`
}

type ValidateOptions struct {
	CIMode bool
}

func Validate(ctx context.Context, worktreePath string, wu *contract.WorkUnit, spec *contract.ValidationSpec) (*ValidationReport, error) {
	return ValidateWithOptions(ctx, worktreePath, wu, spec, ValidateOptions{})
}

func ValidateWithOptions(ctx context.Context, worktreePath string, wu *contract.WorkUnit, spec *contract.ValidationSpec, opts ValidateOptions) (*ValidationReport, error) {
	if wu == nil {
		wu = &contract.WorkUnit{ID: "CI", IssueNumber: 1}
	}
	if spec == nil {
		spec = &contract.ValidationSpec{
			ProtectedPaths: []string{".github/workflows/**", ".swarm/validation/**", ".swarm/contracts/**"},
		}
	}

	// 1. Artifact Completeness Check (when running inside an active worktree, not --ci)
	if !opts.CIMode {
		activeDir := filepath.Join(worktreePath, ".swarm", "active")
		for _, reqFile := range []string{"PLAN.md", "TEST_PLAN.md"} {
			full := filepath.Join(activeDir, reqFile)
			info, err := os.Stat(full)
			if err != nil || info.Size() == 0 {
				return nil, fmt.Errorf("%w: %s not found or empty in .swarm/active/", ErrMissingArtifact, reqFile)
			}
		}
	}

	// 2. Protected Path Tamper Check
	modified, err := collectModifiedFiles(worktreePath)
	if err != nil {
		return nil, err
	}
	if !wu.AllowHarnessChanges {
		for _, modFile := range modified {
			for _, pattern := range spec.ProtectedPaths {
				if matchGlobPattern(pattern, modFile) {
					return nil, fmt.Errorf("%w: file %q matches protected pattern %q", ErrProtectedPathModified, modFile, pattern)
				}
			}
		}
	}

	// 3. Required Checks Execution
	report := &ValidationReport{
		Passed:        true,
		ModifiedFiles: modified,
	}

	var md strings.Builder
	md.WriteString("# Validation Report\n\n")
	md.WriteString("| Check | Command | Status | Duration |\n| :--- | :--- | :--- | :--- |\n")

	var details strings.Builder
	var firstCheckErr error

	for _, chk := range spec.RequiredChecks {
		checkCtx := ctx
		var cancel context.CancelFunc
		if chk.Timeout != "" {
			if dur, err := time.ParseDuration(chk.Timeout); err == nil && dur > 0 {
				checkCtx, cancel = context.WithTimeout(ctx, dur)
			}
		}

		start := time.Now()
		cmd := exec.CommandContext(checkCtx, "sh", "-c", chk.Command)
		cmd.Dir = worktreePath
		var outBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &outBuf

		runErr := cmd.Run()
		if cancel != nil {
			cancel()
		}
		dur := time.Since(start)
		exitCode := 0
		passed := true
		if runErr != nil {
			passed = false
			report.Passed = false
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
			if firstCheckErr == nil {
				firstCheckErr = fmt.Errorf("%w: check %q (%s) exited with %d: %s",
					ErrRequiredCheckFailed, chk.Name, chk.Command, exitCode, strings.TrimSpace(outBuf.String()))
			}
		}

		outcome := CheckRunOutcome{
			Name:     chk.Name,
			Command:  chk.Command,
			ExitCode: exitCode,
			Duration: dur,
			Output:   outBuf.String(),
			Passed:   passed,
		}
		report.CheckOutcomes = append(report.CheckOutcomes, outcome)

		statusStr := "PASS"
		if !passed {
			statusStr = fmt.Sprintf("FAIL (exit %d)", exitCode)
		}
		md.WriteString(fmt.Sprintf("| `%s` | `%s` | **%s** | `%s` |\n", chk.Name, chk.Command, statusStr, dur.Round(time.Millisecond)))
		if strings.TrimSpace(outcome.Output) != "" {
			details.WriteString(fmt.Sprintf("\n### Output: `%s`\n```text\n%s\n```\n", chk.Name, strings.TrimSpace(outcome.Output)))
		}
	}

	md.WriteString(details.String())

	verdict := "PASS"
	summary := fmt.Sprintf("All %d required checks passed", len(report.CheckOutcomes))
	if !report.Passed {
		verdict = "FAIL"
		summary = "One or more required checks failed"
	}

	issueNum := wu.IssueNumber
	if issueNum <= 0 {
		issueNum = 1
	}
	report.Artifact = contract.Artifact{
		Version:     1,
		ID:          fmt.Sprintf("art-%d-validation", issueNum),
		IssueNumber: issueNum,
		Type:        contract.ArtifactValidation,
		Producer:    "swarm:validator",
		Verdict:     verdict,
		Summary:     summary,
		Body:        md.String(),
		CreatedAt:   time.Now().UTC(),
	}

	if !opts.CIMode {
		activeDir := filepath.Join(worktreePath, ".swarm", "active")
		_ = os.MkdirAll(activeDir, 0o755)
		_ = os.WriteFile(filepath.Join(activeDir, "VALIDATION.md"), []byte(md.String()), 0o644)
	}

	if firstCheckErr != nil {
		return report, firstCheckErr
	}
	return report, nil
}

func collectModifiedFiles(worktreePath string) ([]string, error) {
	syncedUntracked := make(map[string]string)
	if raw, err := os.ReadFile(filepath.Join(worktreePath, ".swarm", "active", "synced-untracked.json")); err == nil {
		_ = json.Unmarshal(raw, &syncedUntracked)
	}

	seen := make(map[string]bool)
	var files []string
	addLines := func(out []byte) {
		for _, line := range strings.Split(string(out), "\n") {
			f := filepath.ToSlash(strings.TrimSpace(line))
			if f == "" || strings.HasPrefix(f, ".swarm/active") || strings.HasPrefix(f, ".swarm/worktrees") || strings.HasPrefix(f, ".swarm/local-tracker") {
				continue
			}
			// Ignore untracked AGENTS.md / CLAUDE.md or slash command scaffolding injected by CompileHarnessBundle
			if f == "AGENTS.md" || f == "CLAUDE.md" || strings.HasPrefix(f, ".claude/commands/swarm-") || strings.HasPrefix(f, ".opencode/command/swarm-") {
				continue
			}
			if origHash, ok := syncedUntracked[f]; ok {
				if curData, err := os.ReadFile(filepath.Join(worktreePath, f)); err == nil {
					sum := sha256.Sum256(curData)
					if hex.EncodeToString(sum[:]) == origHash {
						continue
					}
				}
			}
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}

	if out, err := exec.Command("git", "-C", worktreePath, "diff", "--name-only", "origin/HEAD...HEAD").Output(); err == nil {
		addLines(out)
	} else if out, err := exec.Command("git", "-C", worktreePath, "diff", "--name-only", "main...HEAD").Output(); err == nil {
		addLines(out)
	}
	if out, err := exec.Command("git", "-C", worktreePath, "diff", "--name-only", "HEAD").Output(); err == nil {
		addLines(out)
	}
	if out, err := exec.Command("git", "-C", worktreePath, "diff", "--cached", "--name-only").Output(); err == nil {
		addLines(out)
	}
	if out, err := exec.Command("git", "-C", worktreePath, "ls-files", "--others", "--exclude-standard").Output(); err == nil {
		addLines(out)
	}

	return files, nil
}

func matchGlobPattern(pattern, target string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	target = filepath.ToSlash(strings.TrimSpace(target))

	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return target == prefix || strings.HasPrefix(target, prefix+"/")
	}
	if ok, err := filepath.Match(pattern, target); err == nil && ok {
		return true
	}
	return pattern == target
}
