package releasedv080

import "time"

type ForkRequest struct {
	RebaseContext  *RebaseContext `json:"rebase_context,omitempty"`
	RevisionIntent string         `json:"revision_intent,omitempty"`
	Version        int            `json:"version"`
	Operation      string         `json:"operation"`
	IssueNumber    int            `json:"issue_number"`
	ForkRepo       string         `json:"fork_repo,omitempty"`
	ClaimID        string         `json:"claim_id,omitempty"`
	Agent          string         `json:"agent,omitempty"`
	Phase          string         `json:"phase,omitempty"`
	Status         string         `json:"status,omitempty"`
	Duration       string         `json:"duration,omitempty"`
	Gate           GateMode       `json:"gate,omitempty"`
	Artifact       *Artifact      `json:"artifact,omitempty"`
	Branch         string         `json:"branch,omitempty"`
	HeadSHA        string         `json:"head_sha,omitempty"`
	// ExpectedHeadSHA is the published PR head a revision starts from or replaces.
	ExpectedHeadSHA string `json:"expected_head_sha,omitempty"`
	PRNumber        int    `json:"pr_number,omitempty"`
	Reason          string `json:"reason,omitempty"`
	NextPhase       string `json:"next_phase,omitempty"`
}

type RebaseContext struct {
	RepairPaths     *[]string               `json:"repair_paths,omitempty"`
	RepairAttempt   *int                    `json:"repair_attempt,omitempty"`
	ConflictedPaths []string                `json:"conflicted_paths,omitempty"`
	Status          string                  `json:"status"`
	Plan            *RebasePlanReport       `json:"plan,omitempty"`
	Resolution      *RebaseResolutionReport `json:"resolution,omitempty"`
}

type RebasePlanReport struct {
	Harness        string   `json:"harness,omitempty"`
	HarnessVersion string   `json:"harness_version,omitempty"`
	Model          string   `json:"model,omitempty"`
	Sandbox        string   `json:"sandbox,omitempty"`
	Commit         string   `json:"commit"`
	Paths          []string `json:"paths"`
	Body           string   `json:"body"`
	Digest         string   `json:"digest"`
	PlanDigest     string   `json:"plan_digest"`
	BaseSHA        string   `json:"base_sha"`
	Agent          string   `json:"agent"`
	// Guidance is the operator guidance delivered to the planning turn.
	Guidance []string `json:"guidance,omitempty"`
}

type RebaseResolutionReport struct {
	Commit         string   `json:"commit"`
	Paths          []string `json:"paths"`
	Agent          string   `json:"agent"`
	Harness        string   `json:"harness,omitempty"`
	HarnessVersion string   `json:"harness_version,omitempty"`
	Model          string   `json:"model,omitempty"`
	Sandbox        string   `json:"sandbox,omitempty"`
	// Guidance is the operator guidance delivered to the resolve turn.
	Guidance []string `json:"guidance,omitempty"`
}

type GateMode string

type ArtifactType string

type Artifact struct {
	Version     int               `yaml:"version" json:"version"`
	ID          string            `yaml:"id" json:"id"`
	IssueNumber int               `yaml:"issue_number" json:"issue_number"`
	Type        ArtifactType      `yaml:"type" json:"type"`                           // "design", "test-plan", "review", "implementation", "validation"
	Producer    string            `yaml:"producer" json:"producer"`                   // e.g., "claude:planner", "omp:reviewer", "human:supernovae"
	Verdict     string            `yaml:"verdict,omitempty" json:"verdict,omitempty"` // "APPROVED", "CHANGES_REQUESTED", "PASS", "FAIL"
	Summary     string            `yaml:"summary" json:"summary"`
	Body        string            `yaml:"body" json:"body"`
	CreatedAt   time.Time         `yaml:"created_at" json:"created_at"`
	Metadata    map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}
