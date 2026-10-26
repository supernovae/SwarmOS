package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/claimpause"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/sandbox"
)

func (a *CLIAdapter) runIsolated(ctx context.Context, req RunRequest) (*RunResult, error) {
	if a.sandboxRuntime != "codex-native" && a.sandboxRuntime != "harness-tools-bwrap" && (a.authentication != "none" || len(a.envAllow) != 0 || a.reviewNetwork) {
		return nil, fmt.Errorf("isolated agent requires a verified authentication adapter and restricted networking")
	}
	// Agent prompt paths refer to the detached worker workspace, never a host path.
	if req.SystemPrompt == "" {
		req.SystemPrompt = ".swarm/active/SYSTEM_PROMPT.md"
	}
	if filepath.IsAbs(req.SystemPrompt) {
		rel, err := filepath.Rel(req.WorktreePath, req.SystemPrompt)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("system prompt must be inside task workspace")
		}
		req.SystemPrompt = filepath.ToSlash(rel)
	}
	bin, args := a.BuildCommand(req)
	var out process.Output
	var in io.Reader
	var stdout, stderr io.Writer = &out, &out
	if strings.EqualFold(req.Mode, "interactive") {
		in = os.Stdin
		stdout, stderr = io.MultiWriter(os.Stdout, &out), io.MultiWriter(os.Stderr, &out)
	}
	limits, limitErr := contract.ResolveAgentLimits(a.executionPolicy, req.Role, strings.EqualFold(req.Mode, "interactive"), 0)
	if limitErr != nil {
		return nil, limitErr
	}
	options := sandbox.WorkerOptions{Runtime: a.containerRuntime, Image: a.container, Role: req.Role,
		ReadOnly: req.ReadOnly || req.Role == "planner" || req.Role == "reviewer", Limits: limits, Warning: os.Stderr, RecoveryRoot: req.RecoveryRoot, CheckImport: req.CheckImport}
	collector := &GuidanceCollector{}
	options.GuidanceDelivered = collector.Delivered
	options.ProjectPlan = req.ProjectPlan
	if req.ReadOnly && req.Role == "reviewer" {
		options.OutputDir = filepath.Join(req.WorktreePath, ".swarm", "active-output")
	}
	var err error
	var nativeVersion string
	command := append([]string{bin}, args...)
	if a.sandboxRuntime == "codex-native" {
		prompt := fmt.Sprintf("Read %s and .swarm/active/WORKUNIT.md first.\n\n%s", req.SystemPrompt, req.TaskPrompt)
		command, nativeVersion, err = sandbox.RunNativeCodex(ctx, req.WorktreePath, a.binary, req.Model, prompt, options, strings.EqualFold(req.Mode, "interactive"), in, stdout, stderr)
	} else if a.sandboxRuntime == "harness-tools-bwrap" {
		prompt := fmt.Sprintf("Read %s and .swarm/active/WORKUNIT.md first.\n\n%s", req.SystemPrompt, req.TaskPrompt)
		command, nativeVersion, err = sandbox.RunDelegatedHarness(ctx, req.WorktreePath, a.driver, a.binary, req.Model, prompt, options, strings.EqualFold(req.Mode, "interactive"), in, stdout, stderr)
	} else {
		err = sandbox.RunWorker(ctx, req.WorktreePath, bin, args, options, in, stdout, stderr)
	}
	att := a.Provenance(req.Role, options.ReadOnly)
	att.ExecutionMode = contract.ExecutionIsolated
	if nativeVersion != "" {
		att.HarnessVersion = nativeVersion
	}
	result := &RunResult{Agent: a.agentName, Role: req.Role, Command: command, Output: out.String(), Attestation: att, Guidance: collector.Messages()}
	if err != nil {
		if (req.Role == "developer" || req.Role == "tester") && context.Cause(ctx) == claimpause.ErrBudget && !errors.Is(err, claimpause.ErrBudget) {
			err = &claimpause.PreservationError{Err: err}
		}
		result.ExitCode = 1
		return result, err
	}
	return result, nil
}
