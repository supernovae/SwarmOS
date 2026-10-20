package agent

import (
	"context"

	"github.com/supernovae/swarmos/internal/contract"
)

type RunRequest struct {
	Agent        string // configured allowlisted provider/harness name
	Role         string // "planner", "reviewer", "developer", "tester"
	Mode         string // "interactive", "headless"
	WorktreePath string
	SystemPrompt string // relative or absolute path to .swarm/active/SYSTEM_PROMPT.md
	TaskPrompt   string
	Model        string   // optional configured model selector
	ReadOnly     bool     // requests an enforced read-only execution boundary
	Local        bool     // permits deterministic local-only mock execution
	DenyPaths    []string // internal isolation denylist supplied by RunReview
}

type RunResult struct {
	Agent       string                   `json:"agent"`
	Role        string                   `json:"role"`
	Command     []string                 `json:"command"`
	ExitCode    int                      `json:"exit_code"`
	Output      string                   `json:"output"`
	Attestation contract.RoleAttestation `json:"attestation"`
}

type Runner interface {
	Name() string
	Available() (string, bool)
	BuildCommand(req RunRequest) (string, []string)
	Provenance(role string, readOnly bool) contract.RoleAttestation
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
}
