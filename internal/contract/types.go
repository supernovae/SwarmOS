package contract

import "time"

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

type CheckSpec struct {
	Name    string `yaml:"name" json:"name"`
	Command string `yaml:"command" json:"command"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

type SwarmConfig struct {
	Version      int                    `yaml:"version" json:"version"`
	DefaultAgent string                 `yaml:"default_agent" json:"default_agent"`
	DefaultGate  GateMode               `yaml:"default_gate" json:"default_gate"`
	Roles        map[string]string      `yaml:"roles,omitempty" json:"roles,omitempty"`
	Lease        LeaseConfig            `yaml:"lease" json:"lease"`
	Security     SecurityConfig         `yaml:"security" json:"security"`
	Agents       map[string]AgentConfig `yaml:"agents" json:"agents"`
}

type LeaseConfig struct {
	Duration    string `yaml:"duration" json:"duration"`
	Renewable   bool   `yaml:"renewable" json:"renewable"`
	MaxRenewals int    `yaml:"max_renewals" json:"max_renewals"`
}

type SecurityConfig struct {
	RequireReadyLabel bool   `yaml:"require_ready_label" json:"require_ready_label"`
	ReadyLabel        string `yaml:"ready_label" json:"ready_label"`
}

type AgentConfig struct {
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
}
