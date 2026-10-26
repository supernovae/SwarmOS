package agent

import (
	"context"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/process"
)

type RunRequest struct {
	CheckPauseImport     func(context.Context, []string) error // coordinator-only fencing and trusted protected-path check
	OperatorInstructions string                                // coordinator-supplied flag guidance, separate from attestations
	RecoveryRoot         string                                // coordinator-owned durable timeout recovery destination
	ProjectPlan          bool                                  // coordinator-selected project roadmap output for planner
	Agent                string                                // configured allowlisted provider/harness name
	Role                 string                                // "planner", "reviewer", "developer", "tester"
	Mode                 string                                // "interactive", "headless"
	WorktreePath         string
	SystemPrompt         string // relative or absolute path to .swarm/active/SYSTEM_PROMPT.md
	TaskPrompt           string
	Model                string   // optional configured model selector
	ReadOnly             bool     // requests an enforced read-only execution boundary
	Local                bool     // permits deterministic local-only mock execution
	DenyPaths            []string // internal isolation denylist supplied by RunReview
}

type RunResult struct {
	Guidance    []string                 `json:"-"`
	Agent       string                   `json:"agent"`
	Role        string                   `json:"role"`
	Command     []string                 `json:"command"`
	ExitCode    int                      `json:"exit_code"`
	Output      string                   `json:"output"`
	Attestation contract.RoleAttestation `json:"attestation"`
	// transcript holds a failed run's output tail until the coordinator saves it
	// to private diagnostics; it is never serialized or returned to callers.
	transcript *process.Tail
}

type Runner interface {
	Name() string
	Available() (string, bool)
	BuildCommand(req RunRequest) (string, []string)
	Provenance(role string, readOnly bool) contract.RoleAttestation
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
}
