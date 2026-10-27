package contract

import (
	"fmt"
	"math"
	"time"
)

type GateMode string

const (
	GateNone  GateMode = "none"  // Plan phase runs and continues directly to implement
	GateAgent GateMode = "agent" // Reviewer agent evaluates PLAN.md and records verdict before implement
	GateHuman GateMode = "human" // Pauses after plan artifact until approved via comment/CLI
)

type ArtifactType string

const (
	ArtifactDesign         ArtifactType = "design"
	ArtifactTestPlan       ArtifactType = "test-plan"
	ArtifactReview         ArtifactType = "review"
	ArtifactImplementation ArtifactType = "implementation"
	ArtifactValidation     ArtifactType = "validation"
)

type WorkUnit struct {
	ID                   string     `yaml:"id" json:"id"`
	IssueNumber          int        `yaml:"issue_number,omitempty" json:"issue_number,omitempty"`
	ParentIssue          int        `yaml:"parent_issue,omitempty" json:"parent_issue,omitempty"`
	DependsOn            []int      `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	RequiredCapabilities []string   `yaml:"required_capabilities,omitempty" json:"required_capabilities,omitempty"`
	Title                string     `yaml:"title,omitempty" json:"title,omitempty"`
	Objective            string     `yaml:"objective" json:"objective"`
	Constraints          []string   `yaml:"constraints" json:"constraints"`
	Acceptance           []string   `yaml:"acceptance" json:"acceptance"`
	Workflow             string     `yaml:"workflow" json:"workflow"`
	Gate                 GateMode   `yaml:"gate,omitempty" json:"gate,omitempty"`
	AllowHarnessChanges  bool       `yaml:"allow_harness_changes,omitempty" json:"allow_harness_changes,omitempty"`
	Subtasks             []WorkUnit `yaml:"subtasks,omitempty" json:"subtasks,omitempty"`
}

type Lease struct {
	Version      int       `yaml:"version" json:"version"`
	IssueNumber  int       `yaml:"issue_number" json:"issue_number"`
	ClaimID      string    `yaml:"claim_id" json:"claim_id"`
	Owner        string    `yaml:"owner" json:"owner"`
	Agent        string    `yaml:"agent,omitempty" json:"agent,omitempty"`
	NodeID       string    `yaml:"node_id,omitempty" json:"node_id,omitempty"`
	Capabilities []string  `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Contributors []string  `yaml:"contributors,omitempty" json:"contributors,omitempty"`
	Phase        string    `yaml:"phase" json:"phase"`
	NextPhase    string    `yaml:"next_phase,omitempty" json:"next_phase,omitempty"`
	Status       string    `yaml:"status" json:"status"`
	ClaimedAt    time.Time `yaml:"claimed_at" json:"claimed_at"`
	ExpiresAt    time.Time `yaml:"expires_at" json:"expires_at"`
	Renewals     int       `yaml:"renewals" json:"renewals"`
	MaxRenewals  int       `yaml:"max_renewals" json:"max_renewals"`
	CommentID    int64     `yaml:"comment_id,omitempty" json:"comment_id,omitempty"`
}

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

type WorkflowSpec struct {
	Name        string         `yaml:"name" json:"name"`
	Description string         `yaml:"description,omitempty" json:"description,omitempty"`
	DefaultGate GateMode       `yaml:"default_gate,omitempty" json:"default_gate,omitempty"`
	Steps       []WorkflowStep `yaml:"steps" json:"steps"`
}

type WorkflowStep struct {
	Name     string       `yaml:"name" json:"name"` // "analyze", "design", "test_plan", "review_plan", "implement", "validate", "create_pr"
	Role     string       `yaml:"role" json:"role"` // "planner", "developer", "tester", "reviewer"
	Artifact ArtifactType `yaml:"artifact,omitempty" json:"artifact,omitempty"`
	Gate     GateMode     `yaml:"gate,omitempty" json:"gate,omitempty"`
}

type ValidationSpec struct {
	RequiredChecks []CheckSpec `yaml:"required_checks" json:"required_checks"`
	ProtectedPaths []string    `yaml:"protected_paths" json:"protected_paths"`
}

// DefaultProtectedPaths returns the protected paths `swarm repo init` writes:
// CI, SwarmOS policy, and every file that becomes part of an agent's
// instructions, so agent work cannot change what later agents are told.
func DefaultProtectedPaths() []string {
	return []string{
		".github/workflows/**",
		".swarm/validation/**",
		".swarm/contracts/**",
		".swarm/config.yaml",
		".swarm/prompts/**",
		".swarm/workflows/**",
		".swarm/goals.md",
		".swarm/SKILLS.md",
		".swarm/rules/**",
		".swarm/skills/**",
		".swarm/project.md",
		".swarm/architecture.md",
		".swarm/conventions.md",
		".claude/commands/**",
		".opencode/commands/**",
		"AGENTS.md",
		"CLAUDE.md",
	}
}

type CheckSpec struct {
	Name    string `yaml:"name" json:"name"`
	Command string `yaml:"command" json:"command"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

type SwarmConfig struct {
	Execution     ExecutionConfig      `yaml:"execution,omitempty" json:"execution,omitempty"`
	Version       int                  `yaml:"version" json:"version"`
	DefaultAgent  string               `yaml:"default_agent" json:"default_agent"`
	DefaultGate   GateMode             `yaml:"default_gate" json:"default_gate"`
	PlanMode      string               `yaml:"plan_mode,omitempty" json:"plan_mode,omitempty"`
	CommitMessage *CommitMessageConfig `yaml:"commit_message,omitempty" json:"commit_message,omitempty"`
	// LedgerNamespace is read only from the upstream default branch.
	LedgerNamespace string                 `yaml:"ledger_namespace,omitempty" json:"ledger_namespace,omitempty"`
	Roles           map[string]string      `yaml:"roles,omitempty" json:"roles,omitempty"`
	Lease           LeaseConfig            `yaml:"lease" json:"lease"`
	Security        SecurityConfig         `yaml:"security" json:"security"`
	Agents          map[string]AgentConfig `yaml:"agents" json:"agents"`
	Fork            *ForkConfig            `yaml:"fork,omitempty" json:"fork,omitempty"`
}

type CommitMessageConfig struct {
	Detail string `yaml:"detail" json:"detail"`
}

// CommitMessageDetail returns the effective checkpoint message detail without
// changing the serialized form of configurations that omit commit_message.
func (c *SwarmConfig) CommitMessageDetail() string {
	if c == nil || c.CommitMessage == nil || c.CommitMessage.Detail == "" {
		return "concise"
	}
	return c.CommitMessage.Detail
}

type LeaseConfig struct {
	PlanReplanLimit   *int   `yaml:"plan_replan_limit,omitempty" json:"plan_replan_limit,omitempty"`
	RebaseRepairLimit *int   `yaml:"rebase_repair_limit,omitempty" json:"rebase_repair_limit,omitempty"`
	Duration          string `yaml:"duration" json:"duration"`
	Renewable         bool   `yaml:"renewable" json:"renewable"`
	MaxRenewals       int    `yaml:"max_renewals" json:"max_renewals"`
}

type ForkConfig struct {
	MaxActiveClaims int `yaml:"max_active_claims" json:"max_active_claims"`
}

func (c *SwarmConfig) MaxActiveForkClaims() int {
	if c == nil || c.Fork == nil || c.Fork.MaxActiveClaims <= 0 {
		return 1
	}
	return c.Fork.MaxActiveClaims
}

type SecurityConfig struct {
	RequireIsolation      bool     `yaml:"require_isolation,omitempty" json:"require_isolation,omitempty"`
	RequireReadyLabel     bool     `yaml:"require_ready_label" json:"require_ready_label"`
	ReadyLabel            string   `yaml:"ready_label" json:"ready_label"`
	ApprovedCollaborators []string `yaml:"approved_collaborators,omitempty" json:"approved_collaborators,omitempty"`
	ReviewerCollaborators []string `yaml:"reviewer_collaborators,omitempty" json:"reviewer_collaborators,omitempty"`
	// ReviewerAgents names the configured agents whose verdicts may satisfy
	// an agent plan gate. Empty allows any configured reviewer.
	ReviewerAgents []string `yaml:"reviewer_agents,omitempty" json:"reviewer_agents,omitempty"`
	// IndependentReview requires plan reviewers to differ from the planner:
	// a different harness or model for agents, and a different login than
	// the claim owner for humans.
	IndependentReview bool `yaml:"independent_review,omitempty" json:"independent_review,omitempty"`
}

type AgentConfig struct {
	Authentication   string   `yaml:"authentication,omitempty" json:"authentication,omitempty"`
	Binary           string   `yaml:"binary" json:"binary"`
	Harness          string   `yaml:"harness,omitempty" json:"harness,omitempty"`
	Args             []string `yaml:"args,omitempty" json:"args,omitempty"`
	Model            string   `yaml:"model,omitempty" json:"model,omitempty"`
	Roles            []string `yaml:"roles,omitempty" json:"roles,omitempty"`
	Env              []string `yaml:"env,omitempty" json:"env,omitempty"`
	Sandbox          string   `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
	ReviewNetwork    bool     `yaml:"review_network,omitempty" json:"review_network,omitempty"`
	ContainerRuntime string   `yaml:"container_runtime,omitempty" json:"container_runtime,omitempty"`
	ContainerImage   string   `yaml:"container_image,omitempty" json:"container_image,omitempty"`
}

type NodeResources struct {
	OS   string `yaml:"os" json:"os"`
	Arch string `yaml:"arch" json:"arch"`
	CPUs int    `yaml:"cpus" json:"cpus"`
	RAM  string `yaml:"ram" json:"ram"`
}

type NodeProfile struct {
	NodeID       string        `yaml:"node_id" json:"node_id"`
	Capabilities []string      `yaml:"capabilities" json:"capabilities"` // "coding", "testing", "review", "docs"
	Models       []string      `yaml:"models" json:"models"`             // "claude", "codex", "opencode", "omp"
	Resources    NodeResources `yaml:"resources" json:"resources"`
	// UpstreamRemote names the project Git remote when it is not origin.
	UpstreamRemote string `yaml:"upstream_remote,omitempty" json:"upstream_remote,omitempty"`
	// ShareHostDependencyCaches mounts host-wide caches of adapter-backed
	// ecosystems (such as GOMODCACHE) read-only instead of per-repository
	// caches. It is a node setting only; .swarm/config.yaml cannot set it.
	ShareHostDependencyCaches bool `yaml:"share_host_dependency_caches,omitempty" json:"share_host_dependency_caches,omitempty"`
}

type RoleAttestation struct {
	ExecutionMode  string `yaml:"execution_mode,omitempty" json:"execution_mode,omitempty"`
	Authentication string `yaml:"authentication,omitempty" json:"authentication,omitempty"`
	Role           string `yaml:"role" json:"role"`                       // "planner", "reviewer", "developer", "tester"
	Agent          string `yaml:"agent" json:"agent"`                     // configured policy key in .swarm/config.yaml
	Harness        string `yaml:"harness" json:"harness"`                 // "claude", "codex", "opencode", "omp", "mock"
	HarnessVersion string `yaml:"harness_version" json:"harness_version"` // sanitized first-line CLI version or "builtin"
	Model          string `yaml:"model" json:"model"`                     // configured model selector or "default"
	Sandbox        string `yaml:"sandbox" json:"sandbox"`                 // "sandbox-exec", "bwrap", "container", "host", "mock"
	ArtifactType   string `yaml:"artifact_type,omitempty" json:"artifact_type,omitempty"`
	ArtifactDigest string `yaml:"artifact_digest,omitempty" json:"artifact_digest,omitempty"`
}

// Metadata returns base extended with the attestation's nonempty provenance
// fields, in the key format artifacts record.
func (a RoleAttestation) Metadata(base map[string]string) map[string]string {
	m := make(map[string]string, len(base))
	for k, v := range base {
		m[k] = v
	}
	for key, value := range map[string]string{
		"role":            a.Role,
		"agent":           a.Agent,
		"harness":         a.Harness,
		"harness_version": a.HarnessVersion,
		"model":           a.Model,
		"sandbox":         a.Sandbox,
		"execution_mode":  a.ExecutionMode,
		"authentication":  a.Authentication,
	} {
		if value != "" {
			m[key] = value
		}
	}
	return m
}

type ProvenanceAttestation struct {
	SchemaVersion  int               `yaml:"schema_version" json:"schema_version"` // 1
	IssueNumber    int               `yaml:"issue_number" json:"issue_number"`
	Workflow       string            `yaml:"workflow" json:"workflow"`
	EffectiveGate  GateMode          `yaml:"effective_gate" json:"effective_gate"`
	TaskDigest     string            `yaml:"task_digest" json:"task_digest"`
	PolicyDigest   string            `yaml:"policy_digest" json:"policy_digest"`
	BaseSHA        string            `yaml:"base_sha" json:"base_sha"`
	HeadSHA        string            `yaml:"head_sha,omitempty" json:"head_sha,omitempty"`
	PlanDigest     string            `yaml:"plan_digest,omitempty" json:"plan_digest,omitempty"`
	TestPlanDigest string            `yaml:"test_plan_digest,omitempty" json:"test_plan_digest,omitempty"`
	ReviewDigest   string            `yaml:"review_digest,omitempty" json:"review_digest,omitempty"`
	ReviewVerdict  string            `yaml:"review_verdict,omitempty" json:"review_verdict,omitempty"`
	ValidationPass bool              `yaml:"validation_passed" json:"validation_passed"`
	Roles          []RoleAttestation `yaml:"roles" json:"roles"`
}

// RebaseRepairAttempts returns the trusted policy's bounded repair budget.
func (c LeaseConfig) RebaseRepairAttempts() int {
	if c.RebaseRepairLimit == nil {
		return 2
	}
	return *c.RebaseRepairLimit
}

// ClaimBudget returns the lease duration and the total claim budget,
// duration * (1 + max_renewals), rejecting invalid or overflowing policy.
func (c LeaseConfig) ClaimBudget() (time.Duration, time.Duration, error) {
	d, err := time.ParseDuration(c.Duration)
	if err != nil || d <= 0 || c.MaxRenewals < 0 || uint64(c.MaxRenewals) >= uint64(math.MaxInt64)/uint64(d) {
		return 0, 0, fmt.Errorf("invalid or overflowing approved lease budget")
	}
	return d, d * (time.Duration(c.MaxRenewals) + 1), nil
}

// PlanReplanAttempts is the number of automatic retries after a rejected plan.
func (c LeaseConfig) PlanReplanAttempts() int {
	if c.PlanReplanLimit == nil {
		return 2
	}
	return *c.PlanReplanLimit
}
