package lease

import (
	"context"

	"github.com/supernovae/swarmos/internal/contract"
)

// ForkRequest carries the typed worker operation parameters over the untrusted
// issue comment transport. It intentionally excludes lease owner, approved
// base SHA, policy digest, max renewals, and raw IssueState.
type ForkRequest struct {
	RebaseContext  *RebaseContext     `json:"rebase_context,omitempty"`
	RevisionIntent string             `json:"revision_intent,omitempty"`
	Version        int                `json:"version"`
	Operation      string             `json:"operation"`
	IssueNumber    int                `json:"issue_number"`
	ForkRepo       string             `json:"fork_repo,omitempty"`
	ClaimID        string             `json:"claim_id,omitempty"`
	Agent          string             `json:"agent,omitempty"`
	Phase          string             `json:"phase,omitempty"`
	Status         string             `json:"status,omitempty"`
	Duration       string             `json:"duration,omitempty"`
	Gate           contract.GateMode  `json:"gate,omitempty"`
	Artifact       *contract.Artifact `json:"artifact,omitempty"`
	Branch         string             `json:"branch,omitempty"`
	HeadSHA        string             `json:"head_sha,omitempty"`
	// ExpectedHeadSHA is the published PR head a revision starts from or replaces.
	ExpectedHeadSHA string `json:"expected_head_sha,omitempty"`
	PRNumber        int    `json:"pr_number,omitempty"`
	Reason          string `json:"reason,omitempty"`
	NextPhase       string `json:"next_phase,omitempty"`
	// RequestNonce lets a worker find its own request comment after an
	// ambiguous post. It is a correlation id only and grants no authority.
	RequestNonce string `json:"request_nonce,omitempty"`
}

// WorkerGateway forwards worker-side lease mutations in --fork mode to the
// upstream coordinator workflow.
type WorkerGateway interface {
	Apply(ctx context.Context, req ForkRequest) (*IssueState, error)
}
