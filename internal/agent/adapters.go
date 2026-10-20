package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/workspace"
)

type CLIAdapter struct {
	agentName        string
	driver           string
	binary           string
	prefixArgs       []string
	model            string
	envAllow         []string
	sandboxRuntime   string
	reviewNetwork    bool
	containerRuntime string
	container        string
}

func (a *CLIAdapter) Name() string {
	return a.agentName
}

func (a *CLIAdapter) Available() (string, bool) {
	path, err := exec.LookPath(a.binary)
	if err != nil {
		return "", false
	}
	return path, true
}

func (a *CLIAdapter) BuildCommand(req RunRequest) (string, []string) {
	sysFile := req.SystemPrompt
	if sysFile == "" {
		sysFile = ".swarm/active/SYSTEM_PROMPT.md"
	}
	headless := strings.EqualFold(req.Mode, "headless") || req.Mode == ""
	combinedPrompt := fmt.Sprintf("Read %s and .swarm/active/WORKUNIT.md first.\n\n%s", sysFile, req.TaskPrompt)

	var args []string
	switch a.driver {
	case "claude":
		if headless {
			args = []string{"-p", "--append-system-prompt-file", sysFile, req.TaskPrompt}
		} else {
			args = []string{"--append-system-prompt-file", sysFile, req.TaskPrompt}
		}
	case "omp":
		if headless {
			args = []string{"-p", "--no-pty", "--append-system-prompt", sysFile, req.TaskPrompt}
		} else {
			args = []string{"--append-system-prompt", sysFile, req.TaskPrompt}
		}
	case "codex", "dot":
		if headless {
			args = []string{"exec", combinedPrompt}
		} else {
			args = []string{req.TaskPrompt}
		}
	case "grok":
		if headless {
			args = []string{"-p", combinedPrompt}
		} else {
			args = []string{req.TaskPrompt}
		}
	case "opencode":
		if headless {
			args = []string{"run", combinedPrompt}
		} else {
			args = []string{"--prompt", req.TaskPrompt}
		}
	default:
		args = []string{combinedPrompt}
	}
	if req.Model != "" {
		switch a.driver {
		case "claude", "codex", "omp", "opencode":
			args = append([]string{"--model", req.Model}, args...)
		}
	}
	return a.binary, append(append([]string(nil), a.prefixArgs...), args...)
}

func (a *CLIAdapter) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	bin, args := a.BuildCommand(req)
	if a.sandboxRuntime != "container" {
		resolvedBin, err := exec.LookPath(bin)
		if err != nil {
			return nil, fmt.Errorf("resolve agent %q binary %q: %w", a.agentName, bin, err)
		}
		bin = resolvedBin
	}
	fullCmd := append([]string{bin}, args...)
	agentHome, err := os.MkdirTemp("", "swarm-agent-home-*")
	if err != nil {
		return nil, fmt.Errorf("create isolated agent home: %w", err)
	}
	defer os.RemoveAll(agentHome)
	agentHome, err = filepath.EvalSymlinks(agentHome)
	if err != nil {
		return nil, fmt.Errorf("resolve isolated agent home: %w", err)
	}
	for _, path := range []string{agentHome, filepath.Join(agentHome, ".config"), filepath.Join(agentHome, ".cache"), filepath.Join(agentHome, ".local", "share"), filepath.Join(agentHome, "tmp")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
	}
	var cmd *exec.Cmd
	var containerIDFile string
	if req.ReadOnly && a.sandboxRuntime == "container" {
		controlDir, err := os.MkdirTemp("", "swarm-container-control-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(controlDir)
		containerIDFile = filepath.Join(controlDir, "container.id")
	}
	if req.ReadOnly {
		cmd, err = sandbox.ReadOnlyCommand(req.WorktreePath, filepath.Join(req.WorktreePath, ".swarm", "active-output"), bin, args, sandbox.Options{
			Runtime: a.sandboxRuntime, ContainerRuntime: a.containerRuntime, ContainerImage: a.container,
			ContainerIDFile: containerIDFile, Home: agentHome, DenyPaths: req.DenyPaths,
			AllowNetwork: a.reviewNetwork, Env: a.envAllow,
		})
		if err != nil {
			return nil, err
		}
	} else {
		cmd = exec.Command(bin, args...)
		cmd.Dir = req.WorktreePath
	}
	cmd.Env = restrictedEnv(a.envAllow, agentHome)
	process.Configure(cmd)
	cmd.WaitDelay = time.Second

	var outBuf process.Output
	if strings.EqualFold(req.Mode, "interactive") && !req.ReadOnly {
		cmd.Stdin = os.Stdin
		cmd.Stdout = io.MultiWriter(os.Stdout, &outBuf)
		cmd.Stderr = io.MultiWriter(os.Stderr, &outBuf)
	} else {
		cmd.Stdout, cmd.Stderr = &outBuf, &outBuf
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent %q: %w", a.agentName, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		process.Stop(cmd)
		err = <-done
		if err == nil {
			err = ctx.Err()
		}
	}
	process.Stop(cmd)
	if err != nil && containerIDFile != "" {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = errors.Join(err, sandbox.RemoveContainer(cleanupCtx, cmd.Path, cmd.Env, containerIDFile))
		cancel()
	}
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
		return &RunResult{Agent: a.agentName, Role: req.Role, Command: fullCmd, ExitCode: exitCode, Output: outBuf.String()}, fmt.Errorf("agent %q (%s) failed (exit %d): %w\n%s", a.agentName, req.Role, exitCode, err, strings.TrimSpace(outBuf.String()))
	}
	return &RunResult{Agent: a.agentName, Role: req.Role, Command: fullCmd, ExitCode: 0, Output: outBuf.String()}, nil
}

// MockRunner is a deterministic adapter for unit/CI tests and offline dry-runs (--agent mock).
type MockRunner struct {
	ReviewVerdict string // defaults to "APPROVED" unless SWARM_MOCK_VERDICT is set
}

func (m *MockRunner) Name() string {
	return "mock"
}

func (m *MockRunner) Available() (string, bool) {
	return "builtin:mock", true
}

func (m *MockRunner) BuildCommand(req RunRequest) (string, []string) {
	return "mock", []string{"--role", req.Role, "--mode", req.Mode, req.TaskPrompt}
}

func (m *MockRunner) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	activeDir := filepath.Join(req.WorktreePath, ".swarm", "active")
	if err := os.MkdirAll(activeDir, 0o755); err != nil {
		return nil, err
	}

	wu, _ := workspace.LoadActiveWorkUnit(req.WorktreePath)
	title := "WorkUnit"
	objective := "Complete objective"
	var constraints, acceptance []string
	if wu != nil {
		title = wu.Title
		objective = wu.Objective
		constraints = wu.Constraints
		acceptance = wu.Acceptance
	}

	var output string
	switch req.Role {
	case "planner":
		var planSB strings.Builder
		planSB.WriteString(fmt.Sprintf("# Implementation Plan: %s\n\n", title))
		planSB.WriteString("## Context & Objective\n\n")
		planSB.WriteString(objective + "\n\n")
		planSB.WriteString("## Constraints Honored\n\n")
		for _, c := range constraints {
			planSB.WriteString("- " + c + "\n")
		}
		planSB.WriteString("\n## Planned File Changes\n\n")
		planSB.WriteString("1. Inspect existing CLI and domain packages.\n")
		planSB.WriteString("2. Implement the requested behavior in clean, self-contained Go files.\n")
		planSB.WriteString("3. Add unit tests verifying all acceptance criteria.\n")

		if err := os.WriteFile(filepath.Join(activeDir, "PLAN.md"), []byte(planSB.String()), 0o644); err != nil {
			return nil, err
		}

		var testSB strings.Builder
		testSB.WriteString(fmt.Sprintf("# Test Plan: %s\n\n", title))
		testSB.WriteString("| Acceptance Criterion | Verification Command |\n| :--- | :--- |\n")
		for _, a := range acceptance {
			testSB.WriteString(fmt.Sprintf("| %s | `go test ./...` |\n", a))
		}
		if err := os.WriteFile(filepath.Join(activeDir, "TEST_PLAN.md"), []byte(testSB.String()), 0o644); err != nil {
			return nil, err
		}
		output = "Mock planner generated .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md"

	case "reviewer":
		verdict := m.ReviewVerdict
		if envV := os.Getenv("SWARM_MOCK_VERDICT"); envV != "" {
			verdict = envV
		}
		if verdict == "" {
			verdict = "APPROVED"
		}
		reviewContent := fmt.Sprintf("VERDICT: %s\n\n## Plan Audit Summary\n\n- **Objective Alignment**: PASS\n- **Constraints Check**: PASS\n- **Test Plan Coverage**: PASS\n- **Protected Paths**: Unmodified\n", verdict)
		if err := os.WriteFile(filepath.Join(activeDir, "REVIEW.md"), []byte(reviewContent), 0o644); err != nil {
			return nil, err
		}
		output = fmt.Sprintf("Mock reviewer generated .swarm/active/REVIEW.md with VERDICT: %s", verdict)

	case "developer":
		if customCmd := os.Getenv("SWARM_MOCK_DEVELOPER_CMD"); customCmd != "" {
			c := exec.CommandContext(ctx, "sh", "-c", customCmd)
			c.Dir = req.WorktreePath
			out, err := c.CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("mock developer command failed: %w (%s)", err, string(out))
			}
			output = string(out)
		} else {
			// Produce a clean implementation note or requested change in the worktree so git commit has a real change
			implPath := filepath.Join(req.WorktreePath, "internal", "cli", "version.go")
			if _, err := os.Stat(filepath.Join(req.WorktreePath, "internal", "cli")); err == nil {
				if _, statErr := os.Stat(implPath); statErr != nil {
					versionCode := `package cli

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var Version = "0.1.0-dev"

func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print SwarmOS version and build information",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := map[string]string{
				"version": Version,
				"go":      runtime.Version(),
				"os":      runtime.GOOS,
				"arch":    runtime.GOARCH,
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "swarm version %s (%s %s/%s)\n", Version, info["go"], info["os"], info["arch"])
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output version info as JSON")
	return cmd
}
`
					_ = os.WriteFile(implPath, []byte(versionCode), 0o644)
				}
			} else {
				// Fallback file in generic test repositories
				_ = os.WriteFile(filepath.Join(req.WorktreePath, "SWARM_IMPLEMENTED.md"), []byte(fmt.Sprintf("# Implemented %s\n\n%s\n", title, objective)), 0o644)
			}
			output = "Mock developer implemented planned changes"
		}

	case "tester":
		output = "Mock tester completed verification"
	}

	_, args := m.BuildCommand(req)
	return &RunResult{
		Agent:    "mock",
		Role:     req.Role,
		Command:  append([]string{"mock"}, args...),
		ExitCode: 0,
		Output:   output,
	}, nil
}
