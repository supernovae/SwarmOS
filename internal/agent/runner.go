package agent

import "context"

type RunRequest struct {
	Agent        string // "claude", "codex", "opencode", "omp", "custom", "mock"
	Role         string // "planner", "reviewer", "developer", "tester"
	Mode         string // "interactive", "headless"
	WorktreePath string
	SystemPrompt string // relative or absolute path to .swarm/active/SYSTEM_PROMPT.md
	TaskPrompt   string
}

type RunResult struct {
	Agent    string   `json:"agent"`
	Role     string   `json:"role"`
	Command  []string `json:"command"`
	ExitCode int      `json:"exit_code"`
	Output   string   `json:"output"`
}

type Runner interface {
	Name() string
	Available() (string, bool)
	BuildCommand(req RunRequest) (string, []string)
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
}
