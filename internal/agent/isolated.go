package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/sandbox"
)

func (a *CLIAdapter) runIsolated(ctx context.Context, req RunRequest) (*RunResult, error) {
	if a.authentication != "none" || len(a.envAllow) != 0 || a.reviewNetwork {
		return nil, fmt.Errorf("isolated agent requires a verified authentication adapter and restricted networking")
	}
	// Agent prompt paths refer to the detached worker workspace, never a host path.
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
	options := sandbox.WorkerOptions{Runtime: a.containerRuntime, Image: a.container, Role: req.Role,
		ReadOnly: req.ReadOnly || req.Role == "planner" || req.Role == "reviewer", Timeout: 15 * time.Minute}
	options.ProjectPlan = req.ProjectPlan
	if req.ReadOnly && req.Role == "reviewer" {
		options.OutputDir = filepath.Join(req.WorktreePath, ".swarm", "active-output")
	}
	err := sandbox.RunWorker(ctx, req.WorktreePath, bin, args, options, in, stdout, stderr)
	att := a.Provenance(req.Role, options.ReadOnly)
	att.ExecutionMode = contract.ExecutionIsolated
	result := &RunResult{Agent: a.agentName, Role: req.Role, Command: append([]string{bin}, args...), Output: out.String(), Attestation: att}
	if err != nil {
		result.ExitCode = 1
		return result, fmt.Errorf("isolated %s agent: %w", req.Role, err)
	}
	return result, nil
}
