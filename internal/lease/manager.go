package lease

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

var (
	ErrUntrustedIssue    = errors.New("untrusted issue: missing required ready label")
	ErrAlreadyClaimed    = errors.New("issue is already claimed by another active lease")
	ErrClaimRaceLost     = errors.New("claim race lost to a concurrent claimant")
	ErrNoActiveLease     = errors.New("no active lease found for issue")
	ErrMaxRenewals       = errors.New("maximum lease renewals reached")
	ErrDependenciesUnmet = errors.New("prerequisite depends_on issues are not merged or closed")
	ErrStoreUnconfigured = errors.New("coordination store is not configured")
	ErrClaimFence        = errors.New("claim token is absent, stale, or expired")
	ErrApprovalRequired  = errors.New("task approval is missing or stale")
)

type AlreadyClaimedError struct {
	IssueNumber int
	Owner       string
	ExpiresAt   time.Time
	CommentID   int64
}

func (e *AlreadyClaimedError) Error() string {
	return fmt.Sprintf("%s: issue #%d is claimed by %q until %s", ErrAlreadyClaimed, e.IssueNumber, e.Owner, e.ExpiresAt.Format(time.RFC3339))
}
func (e *AlreadyClaimedError) Unwrap() error { return ErrAlreadyClaimed }

// PRRevision tracks unpublished work on the linked pull request. The remote
// head is an immutable publication expectation until a successful submit.
type PRRevision struct {
	Intent        string `json:"intent,omitempty"`
	Number        int    `json:"number"`
	RemoteHeadSHA string `json:"remote_head_sha"`
	LocalHeadSHA  string `json:"local_head_sha"`
	Branch        string `json:"branch"`
}

// ApprovalTransition is coordinator-owned evidence connecting an authenticated
// review origin to the current approved baseline. Worker artifacts cannot set it.
type ApprovalTransition struct {
	OriginBase       string `json:"origin_base"`
	OriginPolicy     string `json:"origin_policy"`
	TargetBase       string `json:"target_base"`
	TargetPolicy     string `json:"target_policy"`
	TaskDigest       string `json:"task_digest"`
	PolicyTreeDigest string `json:"policy_tree_digest"`
	ReviewDigest     string `json:"review_digest"`
}

type IssueState struct {
	SubmittedBaseSHA    string                     `json:"submitted_base_sha,omitempty"`
	PreviousBaseSHA     string                     `json:"previous_base_sha,omitempty"`
	LastRebase          *RebaseEvidence            `json:"last_rebase,omitempty"`
	PolicyTreeDigest    string                     `json:"policy_tree_digest,omitempty"`
	ApprovalTransition  *ApprovalTransition        `json:"approval_transition,omitempty"`
	CarryReason         string                     `json:"carry_reason,omitempty"`
	ChangedPolicyPaths  []string                   `json:"changed_policy_paths,omitempty"`
	CleanupHistory      []CleanupReservation       `json:"cleanup_history,omitempty"`
	Cleanup             *CleanupReservation        `json:"cleanup,omitempty"`
	PROutcome           *PROutcome                 `json:"pr_outcome,omitempty"`
	SubmittedHeadSHA    string                     `json:"submitted_head_sha,omitempty"`
	Lease               contract.Lease             `json:"lease"`
	Artifacts           []contract.Artifact        `json:"artifacts,omitempty"`
	Attestations        []contract.RoleAttestation `json:"attestations,omitempty"`
	ProjectionCommentID int64                      `json:"projection_comment_id,omitempty"`
	TaskDigest          string                     `json:"task_digest"`
	PolicyDigest        string                     `json:"policy_digest"`
	BaseSHA             string                     `json:"base_sha"`
	HeadSHA             string                     `json:"head_sha,omitempty"`
	Branch              string                     `json:"branch,omitempty"`
	ApprovedBy          string                     `json:"approved_by,omitempty"`
	ApprovedAt          time.Time                  `json:"approved_at,omitempty"`
	PRNumber            int                        `json:"pr_number,omitempty"`
	PRRevision          *PRRevision                `json:"pr_revision,omitempty"`
	EffectiveGate       contract.GateMode          `json:"effective_gate,omitempty"`
	PRURL               string                     `json:"pr_url,omitempty"`
	ForkRepo            string                     `json:"fork_repo,omitempty"`
	LastForkRequestID   int64                      `json:"last_fork_request_id,omitempty"`
}
type Manager struct {
	tracker     tracker.Tracker
	security    contract.SecurityConfig
	store       coordination.Store
	repoRoot    string
	nodeProfile *contract.NodeProfile
	nowFn       func() time.Time
	gateway     WorkerGateway
	mu          sync.Mutex
	tokens      map[int]string
}

func NewManager(tr tracker.Tracker, security contract.SecurityConfig) *Manager {
	return &Manager{tracker: tr, security: security, nowFn: time.Now, tokens: make(map[int]string)}
}

func (m *Manager) SetNodeProfile(profile *contract.NodeProfile) { m.nodeProfile = profile }
func (m *Manager) ConfigureStore(root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("empty repository root")
	}
	m.repoRoot = root
	if strings.HasPrefix(m.tracker.BackendName(), "github") {
		m.store = coordination.NewGitStore(root)
	} else {
		m.store = coordination.NewFileStore(root)
	}
	return nil
}
func (m *Manager) Store() coordination.Store      { return m.store }
func (m *Manager) SetNowFunc(fn func() time.Time) { m.nowFn = fn }
func (m *Manager) SetStore(root string, s coordination.Store) {
	if strings.TrimSpace(root) != "" {
		m.repoRoot = root
	}
	m.store = s
}
func (m *Manager) SetWorkerGateway(g WorkerGateway) { m.gateway = g }
func (m *Manager) HasWorkerGateway() bool           { return m.gateway != nil }
func (m *Manager) BindClaim(issue int, claimID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if claimID == "" {
		delete(m.tokens, issue)
	} else {
		m.tokens[issue] = claimID
	}
}
func (m *Manager) token(issue int) string { m.mu.Lock(); defer m.mu.Unlock(); return m.tokens[issue] }
func verifyTrust(sec contract.SecurityConfig, issue *tracker.Issue) error {
	if issue == nil {
		return ErrUntrustedIssue
	}
	if !sec.RequireReadyLabel {
		return fmt.Errorf("security policy must require explicit readiness")
	}
	for _, l := range issue.Labels {
		if l == sec.ReadyLabel {
			return nil
		}
	}
	return fmt.Errorf("%w: issue #%d requires label %q", ErrUntrustedIssue, issue.Number, sec.ReadyLabel)
}

// VerifyTrustPolicy is retained for queue prefiltering; acquisition reloads
// the immutable approved policy before it can create a lease.
func (m *Manager) VerifyTrustPolicy(issue *tracker.Issue) error {
	return verifyTrust(m.security, issue)
}

func matchesCollaboratorAllowlist(actor string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	actor = strings.TrimSpace(actor)
	for _, allowed := range allowlist {
		if strings.EqualFold(strings.TrimSpace(allowed), actor) {
			return true
		}
	}
	return false
}

// checkAgentReviewer enforces the reviewer identity declared by the approved
// base policy. Identity comes from the reviewer's recorded attestation.
func checkAgentReviewer(sec contract.SecurityConfig, st *IssueState, meta map[string]string) error {
	agent := strings.TrimSpace(meta["agent"])
	if agent == "" {
		agent = strings.TrimSpace(meta["harness"])
	}
	if len(sec.ReviewerAgents) > 0 && !slices.Contains(sec.ReviewerAgents, agent) {
		return fmt.Errorf("%w: reviewer agent %q is not in security.reviewer_agents %v", ErrApprovalRequired, agent, sec.ReviewerAgents)
	}
	if !sec.IndependentReview {
		return nil
	}
	harnessName, model := strings.TrimSpace(meta["harness"]), strings.TrimSpace(meta["model"])
	if harnessName == "" {
		harnessName = agent
	}
	if model == "" {
		model = "default"
	}
	for _, att := range st.Attestations {
		if att.Role == "planner" && strings.EqualFold(att.Harness, harnessName) && strings.EqualFold(att.Model, model) {
			return fmt.Errorf("%w: security.independent_review requires a reviewer other than the planner (%s/%s)", ErrApprovalRequired, harnessName, model)
		}
	}
	return nil
}

// checkHumanReviewer keeps the claim owner from approving their own plan
// when the approved policy asks for independent review.
func checkHumanReviewer(sec contract.SecurityConfig, st *IssueState, actor string) error {
	if sec.IndependentReview && strings.EqualFold(strings.TrimSpace(st.Lease.Owner), strings.TrimSpace(actor)) {
		return fmt.Errorf("%w: security.independent_review forbids %q from reviewing a plan they claimed", ErrApprovalRequired, actor)
	}
	return nil
}

func (m *Manager) VerifyDependencies(ctx context.Context, wu *contract.WorkUnit) error {
	for _, n := range wu.DependsOn {
		if n == wu.IssueNumber {
			continue
		}
		iss, err := m.tracker.GetIssue(ctx, n)
		if err != nil || !strings.EqualFold(iss.State, "closed") {
			return fmt.Errorf("%w: issue #%d", ErrDependenciesUnmet, n)
		}
	}
	return nil
}
func (m *Manager) load(ctx context.Context, n int) (*IssueState, string, error) {
	st, rev, err := m.loadReadOnly(ctx, n)
	if err == nil && st.Cleanup != nil {
		return nil, "", fmt.Errorf("%w: issue cleanup reservation is active", ErrClaimFence)
	}
	return st, rev, err
}

func (m *Manager) loadReadOnly(ctx context.Context, n int) (*IssueState, string, error) {
	if m.store == nil {
		return nil, "", ErrStoreUnconfigured
	}
	b, rev, err := m.store.Load(ctx, n)
	if err != nil {
		return nil, "", err
	}
	var st IssueState
	if err = json.Unmarshal(b, &st); err != nil {
		return nil, "", fmt.Errorf("invalid authoritative state: %w", err)
	}
	if st.Lease.IssueNumber != 0 && st.Lease.IssueNumber != n {
		return nil, "", fmt.Errorf("state issue identity mismatch")
	}
	return &st, rev, nil
}
func (m *Manager) save(ctx context.Context, n int, rev string, st *IssueState) (*IssueState, error) {
	m.stampForSave(ctx, n, st)
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	if _, err = m.store.CompareAndSwap(ctx, n, rev, b); err != nil {
		return nil, err
	}
	m.project(ctx, n, st)
	return st, nil
}

// stampForSave fills bookkeeping fields every persisted state carries.
func (m *Manager) stampForSave(ctx context.Context, n int, st *IssueState) {
	if st.ProjectionCommentID == 0 {
		if existingID := m.findProjectionCommentID(ctx, n); existingID > 0 {
			st.ProjectionCommentID = existingID
		}
	}
	if stamper, ok := m.store.(interface{ RequestID() int64 }); ok {
		if reqID := stamper.RequestID(); reqID > 0 {
			st.LastForkRequestID = reqID
		}
	}
}

func (m *Manager) findProjectionCommentID(ctx context.Context, n int) int64 {
	if m.tracker == nil {
		return 0
	}
	comments, err := m.tracker.ListComments(ctx, n)
	if err != nil {
		return 0
	}
	for _, c := range comments {
		if strings.HasPrefix(strings.TrimSpace(c.Body), "<!-- swarm:audit:v1 -->") {
			return c.ID
		}
	}
	return 0
}

func (m *Manager) project(ctx context.Context, n int, st *IssueState) {
	if m.tracker == nil || st == nil {
		return
	}
	var wu *contract.WorkUnit
	if iss, err := m.tracker.GetIssue(ctx, n); err == nil && iss != nil {
		wu, _ = contract.ParseWorkUnitFromIssue(n, iss.Title, iss.Body)
	}
	body := RenderStateProjection(wu, st)
	commentID := st.ProjectionCommentID
	if commentID == 0 {
		commentID = m.findProjectionCommentID(ctx, n)
	}
	if commentID > 0 {
		if _, err := m.tracker.UpdateComment(ctx, commentID, body); err == nil {
			st.ProjectionCommentID = commentID
			return
		}
	}
	if created, err := m.tracker.CreateComment(ctx, n, body); err == nil && created != nil {
		st.ProjectionCommentID = created.ID
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

const maxProjectedArtifactChars = 12000

func TruncateArtifactBody(body string, issueNum int) string {
	trimmed := strings.TrimSpace(body)
	if len(trimmed) <= maxProjectedArtifactChars {
		return trimmed
	}
	return fmt.Sprintf("%s\n\n...(truncated at %d chars; full artifact stored in `refs/heads/swarm-state/issue-%d`)", trimmed[:maxProjectedArtifactChars], maxProjectedArtifactChars, issueNum)
}

func roleOrder(role string) int {
	switch role {
	case "planner":
		return 0
	case "reviewer":
		return 1
	case "developer":
		return 2
	case "tester":
		return 3
	default:
		return 4
	}
}

func RecordRoleAttestation(st *IssueState, att contract.RoleAttestation) {
	if st == nil || strings.TrimSpace(att.Role) == "" {
		return
	}
	if att.Model == "" {
		att.Model = "default"
	}
	if att.HarnessVersion == "" {
		att.HarnessVersion = "unknown"
	}
	if att.Sandbox == "" {
		att.Sandbox = "host"
	}
	if att.Harness == "" {
		att.Harness = att.Agent
	}
	if att.Agent == "" {
		att.Agent = att.Harness
	}
	for i := range st.Attestations {
		if st.Attestations[i].Role == att.Role {
			if att.ArtifactType == "" {
				att.ArtifactType = st.Attestations[i].ArtifactType
			}
			if att.ArtifactDigest == "" {
				att.ArtifactDigest = st.Attestations[i].ArtifactDigest
			}
			st.Attestations[i] = att
			return
		}
	}
	st.Attestations = append(st.Attestations, att)
	// Keep canonical role ordering: planner, reviewer, developer, tester.
	for i := 0; i < len(st.Attestations); i++ {
		for j := i + 1; j < len(st.Attestations); j++ {
			if roleOrder(st.Attestations[j].Role) < roleOrder(st.Attestations[i].Role) {
				st.Attestations[i], st.Attestations[j] = st.Attestations[j], st.Attestations[i]
			}
		}
	}
}

func BuildProvenanceAttestation(wu *contract.WorkUnit, st *IssueState) *contract.ProvenanceAttestation {
	att := &contract.ProvenanceAttestation{
		SchemaVersion: 1,
	}
	if st == nil {
		if wu != nil {
			att.IssueNumber = wu.IssueNumber
			att.Workflow = wu.Workflow
		}
		return att
	}
	att.IssueNumber = st.Lease.IssueNumber
	if wu != nil {
		if att.IssueNumber == 0 {
			att.IssueNumber = wu.IssueNumber
		}
		att.Workflow = wu.Workflow
	}
	att.EffectiveGate = st.EffectiveGate
	att.TaskDigest = st.TaskDigest
	att.PolicyDigest = st.PolicyDigest
	att.BaseSHA = st.BaseSHA
	att.HeadSHA = st.HeadSHA

	for _, a := range st.Artifacts {
		digest := a.Metadata["content_sha256"]
		if digest == "" {
			digest = hash(a.Body)
		}
		switch a.Type {
		case contract.ArtifactDesign:
			att.PlanDigest = digest
		case contract.ArtifactTestPlan:
			att.TestPlanDigest = digest
		case contract.ArtifactReview:
			att.ReviewDigest = digest
			att.ReviewVerdict = a.Verdict
		case contract.ArtifactValidation:
			att.ValidationPass = strings.EqualFold(a.Verdict, "PASS")
		}
	}
	att.Roles = append([]contract.RoleAttestation(nil), st.Attestations...)
	return att
}

func RenderStateProjection(wu *contract.WorkUnit, st *IssueState) string {
	if st == nil {
		return "<!-- swarm:audit:v1 -->\n### 🐝 SwarmOS Orchestration & Provenance State\n"
	}
	issueNum := st.Lease.IssueNumber
	if issueNum == 0 && wu != nil {
		issueNum = wu.IssueNumber
	}
	phase := st.Lease.Phase
	if phase == "" {
		phase = "approved"
	}
	status := st.Lease.Status
	if status == "" {
		status = "draft"
	}
	gate := string(st.EffectiveGate)
	if gate == "" {
		gate = "none"
	}
	head := short(st.HeadSHA)
	if head == "" {
		head = "-"
	} else {
		head = "`" + head + "`"
	}
	prDisplay := "-"
	if st.PRNumber > 0 {
		if st.PRURL != "" {
			prDisplay = fmt.Sprintf("[#%d](%s)", st.PRNumber, st.PRURL)
		} else {
			prDisplay = fmt.Sprintf("#%d", st.PRNumber)
		}
	}

	var sb strings.Builder
	sb.WriteString("<!-- swarm:audit:v1 -->\n")
	sb.WriteString("### 🐝 SwarmOS Orchestration & Provenance State\n\n")
	sb.WriteString("| Field | Value |\n| :--- | :--- |\n")
	sb.WriteString(fmt.Sprintf("| **Phase** | `%s` |\n", phase))
	sb.WriteString(fmt.Sprintf("| **Status** | `%s` |\n", status))
	if o := st.PROutcome; o != nil {
		sb.WriteString(fmt.Sprintf("| **PR outcome** | `%s` |\n", o.Status))
		if o.MergeCommitSHA != "" {
			sb.WriteString(fmt.Sprintf("| **Merge commit** | `%s` |\n", o.MergeCommitSHA))
		}
		if o.MergedAt != nil {
			sb.WriteString(fmt.Sprintf("| **Merged at** | %s |\n", o.MergedAt.UTC().Format(time.RFC3339)))
		}
	}
	sb.WriteString(fmt.Sprintf("| **Effective Gate** | `%s` |\n", gate))
	sb.WriteString(fmt.Sprintf("| **Base SHA** | `%s` |\n", short(st.BaseSHA)))
	sb.WriteString(fmt.Sprintf("| **Checkpoint SHA** | %s |\n", head))
	sb.WriteString(fmt.Sprintf("| **Task Digest** | `%s` |\n", short(st.TaskDigest)))
	sb.WriteString(fmt.Sprintf("| **Policy Digest** | `%s` |\n", short(st.PolicyDigest)))
	sb.WriteString(fmt.Sprintf("| **Draft PR** | %s |\n", prDisplay))

	prov := BuildProvenanceAttestation(wu, st)
	if len(prov.Roles) > 0 {
		sb.WriteString("\n#### 🔒 Harness & Model Provenance Attestation\n\n")
		sb.WriteString("| Role | Configured Agent | Harness | Version | Model | Boundary | Artifact SHA-256 |\n")
		sb.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- | :--- |\n")
		for _, r := range prov.Roles {
			digest := "-"
			if r.ArtifactDigest != "" {
				digest = "`" + short(r.ArtifactDigest) + "`"
			}
			sb.WriteString(fmt.Sprintf("| `%s` | `%s` | `%s` | `%s` | `%s` | `%s` | %s |\n",
				r.Role, r.Agent, r.Harness, r.HarnessVersion, r.Model, r.Sandbox, digest))
		}
	}

	if len(st.Artifacts) > 0 {
		sb.WriteString("\n#### 📦 Workflow Artifacts\n\n")
		for _, a := range st.Artifacts {
			digest := a.Metadata["content_sha256"]
			if digest == "" {
				digest = hash(a.Body)
			}
			var title string
			switch a.Type {
			case contract.ArtifactDesign:
				title = fmt.Sprintf("📋 Implementation Plan (PLAN.md — sha256:%s)", short(digest))
			case contract.ArtifactTestPlan:
				title = fmt.Sprintf("🧪 Test Plan (TEST_PLAN.md — sha256:%s)", short(digest))
			case contract.ArtifactReview:
				title = fmt.Sprintf("🔍 Plan Review (REVIEW.md — Verdict: %s — sha256:%s)", a.Verdict, short(digest))
			case contract.ArtifactImplementation:
				title = fmt.Sprintf("🛠️ Implementation Summary (sha256:%s)", short(digest))
			case contract.ArtifactValidation:
				title = fmt.Sprintf("✅ Validation Checks (%s — sha256:%s)", a.Verdict, short(digest))
			default:
				title = fmt.Sprintf("📄 Artifact (%s — sha256:%s)", a.Type, short(digest))
			}
			sb.WriteString(fmt.Sprintf("<details>\n<summary>%s</summary>\n\n%s\n\n</details>\n\n", title, TruncateArtifactBody(a.Body, issueNum)))
		}
	}
	return sb.String()
}
func (m *Manager) ApproveTask(ctx context.Context, n int, owner string) (*IssueState, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("task approval is not permitted in fork mode")
	}
	actor, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	if owner == "" || owner != actor {
		return nil, fmt.Errorf("approval owner must be authenticated actor")
	}
	if err = m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return nil, err
	}
	issue, err := m.tracker.GetIssue(ctx, n)
	if err != nil {
		return nil, err
	}
	if issue.IsEpic() {
		return nil, fmt.Errorf("issue #%d has type \"Epic\" and cannot be approved as an executable WorkUnit", n)
	}
	wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
	if err != nil {
		return nil, err
	}
	base, err := m.trustedBase()
	if err != nil {
		return nil, err
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, base)
	if err != nil {
		return nil, fmt.Errorf("load approved policy: %w", err)
	}
	wf, err := harness.LoadWorkflowAt(m.repoRoot, wu.Workflow, base)
	if err != nil {
		return nil, err
	}
	gate, err := contract.ResolveGate(wu.Gate, wf.DefaultGate, cfg.DefaultGate)
	if err != nil {
		return nil, err
	}
	sec := cfg.Security
	digest := contract.DigestApprovedPolicy(base, cfg)
	tree, err := workspace.PolicyTreeDigest(ctx, m.repoRoot, base)
	if err != nil {
		return nil, err
	}
	if old, rev, err := m.load(ctx, n); err == nil {
		if old.Lease.Status == "active" && old.Lease.ExpiresAt.After(m.nowFn()) {
			return nil, &AlreadyClaimedError{IssueNumber: n, Owner: old.Lease.Owner, ExpiresAt: old.Lease.ExpiresAt, CommentID: old.ProjectionCommentID}
		}
		if old.BaseSHA != base || old.TaskDigest != contract.DigestWorkUnit(wu) || old.PolicyDigest != digest {
			old.PreviousBaseSHA = old.BaseSHA
			m.carryApproval(ctx, old, wu, base, digest, tree)
		}
		old.PolicyTreeDigest = tree
		old.TaskDigest = contract.DigestWorkUnit(wu)
		old.PolicyDigest = digest
		old.BaseSHA = base
		old.EffectiveGate = gate
		old.ApprovedBy = owner
		old.ApprovedAt = m.nowFn()
		if _, err = m.save(ctx, n, rev, old); err != nil {
			return nil, err
		}
		for _, staleLabel := range []string{"swarm:draft", "swarm:needs-scope", "swarm:blocked"} {
			_ = m.tracker.RemoveLabel(ctx, n, staleLabel)
		}
		if err = m.tracker.AddLabels(ctx, n, []string{sec.ReadyLabel}); err != nil {
			return nil, err
		}
		return old, nil
	} else if !errors.Is(err, coordination.ErrNotFound) {
		return nil, err
	}
	st := &IssueState{Lease: contract.Lease{Version: 1, IssueNumber: n, Status: "draft"}, TaskDigest: contract.DigestWorkUnit(wu), PolicyDigest: digest, PolicyTreeDigest: tree, BaseSHA: base, EffectiveGate: gate, ApprovedBy: owner, ApprovedAt: m.nowFn()}
	if _, err = m.save(ctx, n, "", st); err != nil {
		return nil, err
	}
	for _, staleLabel := range []string{"swarm:draft", "swarm:needs-scope", "swarm:blocked"} {
		_ = m.tracker.RemoveLabel(ctx, n, staleLabel)
	}
	if err = m.tracker.AddLabels(ctx, n, []string{sec.ReadyLabel}); err != nil {
		return nil, err
	}
	return st, nil
}
func (m *Manager) trustedBase() (string, error) {
	if m.repoRoot == "" {
		return "", ErrStoreUnconfigured
	}
	return workspace.ResolveBase(m.repoRoot)
}

// AcquirePR reuses the approved WorkUnit lease for a linked PR revision.
// Through a contributor gateway, the upstream coordinator verifies that the
// actor owns the fork the PR's head lives in before granting the claim.
func (m *Manager) AcquirePR(ctx context.Context, n, number int, owner, agent, phase string, duration time.Duration, max int) (*IssueState, error) {
	if m.gateway != nil {
		st, err := m.gateway.Apply(ctx, ForkRequest{Version: 1, Operation: "acquire-pr", IssueNumber: n, PRNumber: number, Agent: agent, Phase: phase, Duration: duration.String()})
		if err != nil {
			return nil, err
		}
		if st != nil && st.Lease.ClaimID != "" {
			m.BindClaim(n, st.Lease.ClaimID)
		}
		return st, nil
	}
	return m.acquire(ctx, n, number, owner, agent, phase, duration, max)
}
func (m *Manager) Acquire(ctx context.Context, n int, owner, agent, phase string, duration time.Duration, max int) (*IssueState, error) {
	return m.acquire(ctx, n, 0, owner, agent, phase, duration, max)
}

func (m *Manager) acquire(ctx context.Context, n, prNumber int, owner, agent, phase string, duration time.Duration, max int) (*IssueState, error) {
	if m.gateway != nil {
		st, err := m.gateway.Apply(ctx, ForkRequest{
			Version:     1,
			Operation:   "acquire",
			IssueNumber: n,
			Agent:       agent,
			Phase:       phase,
			Duration:    duration.String(),
		})
		if err != nil {
			return nil, err
		}
		if st != nil && st.Lease.ClaimID != "" {
			m.BindClaim(n, st.Lease.ClaimID)
		}
		return st, nil
	}
	actor, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	if owner == "" || owner != actor {
		return nil, fmt.Errorf("claim owner must be authenticated actor")
	}
	if err := contract.ValidatePhase(phase); err != nil {
		return nil, err
	}
	if duration <= 0 {
		return nil, fmt.Errorf("lease duration must be positive")
	}
	issue, err := m.tracker.GetIssue(ctx, n)
	if err != nil {
		return nil, err
	}
	if issue.IsEpic() {
		return nil, fmt.Errorf("issue #%d has type \"Epic\" and cannot be claimed for execution", n)
	}
	wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
	if err != nil {
		return nil, err
	}
	if err = m.VerifyDependencies(ctx, wu); err != nil {
		return nil, err
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		if errors.Is(err, ErrClaimFence) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: approve task first", ErrApprovalRequired)
	}
	if st.PRRevision != nil && prNumber != st.PRRevision.Number {
		return nil, fmt.Errorf("issue #%d has a pending PR revision; use swarm pr work %d", n, st.PRRevision.Number)
	}
	if prNumber != 0 && st.PRNumber != prNumber {
		return nil, fmt.Errorf("PR #%d is not linked to issue #%d", prNumber, n)
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("load approved policy: %w", err)
	}
	if !matchesCollaboratorAllowlist(owner, cfg.Security.ApprovedCollaborators) {
		return nil, fmt.Errorf("actor %q is not in security.approved_collaborators", owner)
	}
	if err = verifyTrust(cfg.Security, issue); err != nil {
		return nil, err
	}
	if !TaskApprovalCurrent(st, wu, cfg) {
		return nil, ErrApprovalRequired
	}
	now := m.nowFn()
	if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now) {
		return nil, &AlreadyClaimedError{IssueNumber: n, Owner: st.Lease.Owner, ExpiresAt: st.Lease.ExpiresAt}
	}
	id := claimID()
	contributors := append([]string(nil), st.Lease.Contributors...)
	known := false
	for _, contributor := range contributors {
		known = known || contributor == owner
	}
	if !known {
		contributors = append(contributors, owner)
	}
	// A fresh issue claim replaces a fork contributor's checkpoint. PR work keeps
	// the PR link and its head repository so the revision can be published there.
	if st.ForkRepo != "" && prNumber == 0 {
		clearCheckpointAndHeadArtifacts(st)
		st.ForkRepo = ""
	}
	st.Lease = contract.Lease{Version: 1, IssueNumber: n, ClaimID: id, Owner: owner, Agent: agent, Phase: phase, Status: "active", ClaimedAt: now, ExpiresAt: now.Add(duration), MaxRenewals: max}
	st.Lease.Contributors = contributors
	if m.nodeProfile != nil {
		st.Lease.NodeID = m.nodeProfile.NodeID
		st.Lease.Capabilities = append([]string(nil), m.nodeProfile.Capabilities...)
	}
	if _, err = m.save(ctx, n, rev, st); err != nil {
		if errors.Is(err, coordination.ErrConflict) {
			return nil, ErrClaimRaceLost
		}
		return nil, err
	}
	m.BindClaim(n, id)
	_ = m.tracker.AddLabels(ctx, n, []string{"swarm:claimed", "swarm:in-progress"})
	return st, nil
}

func clearCheckpointAndHeadArtifacts(st *IssueState) {
	if st == nil {
		return
	}
	st.Branch = ""
	st.HeadSHA = ""
	st.PRNumber = 0
	st.PRURL = ""
	st.PRRevision = nil
	var keptArtifacts []contract.Artifact
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactDesign || a.Type == contract.ArtifactTestPlan || a.Type == contract.ArtifactReview {
			keptArtifacts = append(keptArtifacts, a)
		}
	}
	st.Artifacts = keptArtifacts
	var keptAttestations []contract.RoleAttestation
	for _, att := range st.Attestations {
		if att.ArtifactType == string(contract.ArtifactDesign) ||
			att.ArtifactType == string(contract.ArtifactTestPlan) ||
			att.ArtifactType == string(contract.ArtifactReview) {
			keptAttestations = append(keptAttestations, att)
		}
	}
	st.Attestations = keptAttestations
}

func (m *Manager) AcquireFork(ctx context.Context, issue int, owner, agent, phase, forkRepo string, duration time.Duration, maxRenewals int) (*IssueState, error) {
	actor, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	if owner == "" || owner != actor {
		return nil, fmt.Errorf("claim owner must be authenticated actor")
	}
	if err := contract.ValidatePhase(phase); err != nil {
		return nil, err
	}
	forkRepo = strings.TrimSpace(forkRepo)
	if forkRepo == "" || !strings.Contains(forkRepo, "/") {
		return nil, fmt.Errorf("fork repository owner/repo is required")
	}
	iss, err := m.tracker.GetIssue(ctx, issue)
	if err != nil {
		return nil, err
	}
	if iss.IsEpic() {
		return nil, fmt.Errorf("issue #%d has type \"Epic\" and cannot be claimed for execution", issue)
	}
	wu, err := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
	if err != nil {
		return nil, err
	}
	if err = m.VerifyDependencies(ctx, wu); err != nil {
		return nil, err
	}
	for attempt := range 4 {
		st, rev, err := m.load(ctx, issue)
		if err != nil {
			if errors.Is(err, ErrClaimFence) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: approve task first", ErrApprovalRequired)
		}
		if st.PRRevision != nil {
			return nil, fmt.Errorf("issue #%d has a pending PR revision; use swarm pr work %d", issue, st.PRRevision.Number)
		}
		cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
		if err != nil {
			return nil, fmt.Errorf("load approved policy: %w", err)
		}
		if !matchesCollaboratorAllowlist(owner, cfg.Security.ApprovedCollaborators) {
			return nil, fmt.Errorf("actor %q is not in security.approved_collaborators", owner)
		}
		if err = verifyTrust(cfg.Security, iss); err != nil {
			return nil, err
		}
		if !TaskApprovalCurrent(st, wu, cfg) {
			return nil, ErrApprovalRequired
		}
		policyDur, err := time.ParseDuration(cfg.Lease.Duration)
		if err != nil || policyDur <= 0 {
			return nil, fmt.Errorf("invalid approved lease duration")
		}
		effectiveDur := duration
		if effectiveDur <= 0 || effectiveDur > policyDur {
			effectiveDur = policyDur
		}
		effectiveMax := cfg.Lease.MaxRenewals
		if !cfg.Lease.Renewable {
			effectiveMax = 0
		} else if maxRenewals >= 0 && maxRenewals < effectiveMax {
			effectiveMax = maxRenewals
		}
		now := m.nowFn()
		if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now) {
			if attempt > 0 {
				return nil, ErrClaimRaceLost
			}
			return nil, &AlreadyClaimedError{IssueNumber: issue, Owner: st.Lease.Owner, ExpiresAt: st.Lease.ExpiresAt}
		}
		id := claimID()
		contributors := append([]string(nil), st.Lease.Contributors...)
		known := false
		for _, contributor := range contributors {
			known = known || contributor == owner
		}
		if !known {
			contributors = append(contributors, owner)
		}
		clearCheckpointAndHeadArtifacts(st)
		st.ForkRepo = forkRepo
		st.Lease = contract.Lease{
			Version:      1,
			IssueNumber:  issue,
			ClaimID:      id,
			Owner:        owner,
			Agent:        agent,
			Phase:        phase,
			Status:       "active",
			ClaimedAt:    now,
			ExpiresAt:    now.Add(effectiveDur),
			MaxRenewals:  effectiveMax,
			Contributors: contributors,
		}
		if m.nodeProfile != nil {
			st.Lease.NodeID = m.nodeProfile.NodeID
			st.Lease.Capabilities = append([]string(nil), m.nodeProfile.Capabilities...)
		}
		if _, err = m.save(ctx, issue, rev, st); err != nil {
			if errors.Is(err, coordination.ErrConflict) {
				continue
			}
			return nil, err
		}
		m.BindClaim(issue, id)
		_ = m.tracker.AddLabels(ctx, issue, []string{"swarm:claimed", "swarm:in-progress"})
		return st, nil
	}
	return nil, ErrClaimRaceLost
}
func claimID() string { b := make([]byte, 16); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func (m *Manager) Resume(ctx context.Context, n int, owner, id string) (*IssueState, error) {
	m.BindClaim(n, id)
	st, _, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if err = m.CheckClaim(ctx, n, owner); err != nil {
		return nil, err
	}
	return st, nil
}
func (m *Manager) CheckClaim(ctx context.Context, n int, owner string) error {
	st, _, err := m.load(ctx, n)
	if err != nil {
		return err
	}
	return m.checkStateClaim(ctx, n, owner, st)
}

func (m *Manager) checkApprovedState(ctx context.Context, n int, st *IssueState) error {
	issue, err := m.tracker.GetIssue(ctx, n)
	if err != nil {
		return err
	}
	if issue.IsEpic() {
		return fmt.Errorf("issue #%d has type \"Epic\" and cannot be executed as a WorkUnit", n)
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
	if err != nil {
		return fmt.Errorf("load approved policy: %w", err)
	}
	if err = verifyTrust(cfg.Security, issue); err != nil {
		return err
	}
	wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
	if err != nil {
		return err
	}
	if !TaskApprovalCurrent(st, wu, cfg) {
		return ErrApprovalRequired
	}
	return nil
}

func (m *Manager) checkStateClaim(ctx context.Context, n int, owner string, st *IssueState) error {
	if st.Lease.Status != "active" || !st.Lease.ExpiresAt.After(m.nowFn()) || owner == "" || st.Lease.Owner != owner || st.Lease.ClaimID != m.token(n) {
		return ErrClaimFence
	}
	return m.checkApprovedState(ctx, n, st)
}
func (m *Manager) mutate(ctx context.Context, n int, owner string, fn func(*IssueState) error) (*IssueState, error) {
	// Heartbeats and human decisions can legitimately race this same attempt.
	// Re-read on CAS conflict; a successor's token never passes the next check.
	for range 4 {
		st, rev, err := m.load(ctx, n)
		if err != nil {
			return nil, err
		}
		if err = m.checkStateClaim(ctx, n, owner, st); err != nil {
			return nil, err
		}
		if err = fn(st); err != nil {
			return nil, err
		}
		out, err := m.save(ctx, n, rev, st)
		if errors.Is(err, coordination.ErrConflict) {
			continue
		}
		return out, err
	}
	return nil, coordination.ErrConflict
}
func (m *Manager) Renew(ctx context.Context, n int, owner string, d time.Duration) (*IssueState, error) {
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{
			Version:     1,
			Operation:   "renew",
			IssueNumber: n,
			ClaimID:     m.token(n),
			Duration:    d.String(),
		})
	}
	if d <= 0 {
		return nil, fmt.Errorf("renew duration must be positive")
	}
	return m.mutate(ctx, n, owner, func(st *IssueState) error {
		cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
		if err != nil {
			return err
		}
		if !cfg.Lease.Renewable {
			return fmt.Errorf("approved policy disallows lease renewal")
		}
		if st.Lease.Renewals >= st.Lease.MaxRenewals {
			return ErrMaxRenewals
		}
		effectiveDur := d
		if policyDur, err := time.ParseDuration(cfg.Lease.Duration); err == nil && policyDur > 0 && st.ForkRepo != "" && effectiveDur > policyDur {
			effectiveDur = policyDur
		}
		st.Lease.Renewals++
		st.Lease.ExpiresAt = m.nowFn().Add(effectiveDur)
		return nil
	})
}
func (m *Manager) Release(ctx context.Context, n int, owner, reason string) (*IssueState, error) {
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{
			Version:     1,
			Operation:   "release",
			IssueNumber: n,
			ClaimID:     m.token(n),
			Reason:      reason,
		})
	}
	st, err := m.mutate(ctx, n, owner, func(st *IssueState) error { st.Lease.Status = "released"; st.Lease.ExpiresAt = m.nowFn(); return nil })
	if err == nil {
		_ = m.tracker.RemoveLabel(ctx, n, "swarm:claimed")
		_ = m.tracker.RemoveLabel(ctx, n, "swarm:in-progress")
	}
	return st, err
}
func (m *Manager) Handoff(ctx context.Context, n int, owner, done, next string) (*IssueState, error) {
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{
			Version:     1,
			Operation:   "handoff",
			IssueNumber: n,
			ClaimID:     m.token(n),
			Phase:       done,
			NextPhase:   next,
		})
	}
	st, err := m.mutate(ctx, n, owner, func(st *IssueState) error {
		if err := contract.ValidatePhase(next); err != nil {
			return err
		}
		st.Lease.Phase = done
		st.Lease.NextPhase = next
		st.Lease.Status = "handoff"
		st.Lease.ExpiresAt = m.nowFn()
		return nil
	})
	if err == nil && m.tracker != nil && st.EffectiveGate != contract.GateNone && (done == "plan" || next == "review") {
		_ = m.tracker.AddLabels(ctx, n, []string{"swarm:plan-review"})
	}
	return st, err
}

// RequireGate persists only stronger execution requirements for this approval.
func (m *Manager) RequireGate(ctx context.Context, issue int, owner string, requested contract.GateMode) (*IssueState, error) {
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{
			Version:     1,
			Operation:   "require-gate",
			IssueNumber: issue,
			ClaimID:     m.token(issue),
			Gate:        requested,
		})
	}
	return m.mutate(ctx, issue, owner, func(state *IssueState) error {
		gate, err := contract.ResolveGate(requested, state.EffectiveGate, "")
		if err != nil {
			return err
		}
		state.EffectiveGate = gate
		return nil
	})
}
func (m *Manager) UpdatePhase(ctx context.Context, n int, owner, phase, status string) (*IssueState, error) {
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{
			Version:     1,
			Operation:   "update-phase",
			IssueNumber: n,
			ClaimID:     m.token(n),
			Phase:       phase,
			Status:      status,
		})
	}
	st, err := m.mutate(ctx, n, owner, func(st *IssueState) error {
		if phase != "" {
			if err := contract.ValidatePhase(phase); err != nil {
				return err
			}
			st.Lease.Phase = phase
		}
		if status != "" {
			st.Lease.Status = status
		}
		return nil
	})
	if err == nil && m.tracker != nil && st.EffectiveGate != contract.GateNone && phase == "review" {
		_ = m.tracker.AddLabels(ctx, n, []string{"swarm:plan-review"})
	}
	return st, err
}

// NotifyPlanReviewers assigns the approved policy's reviewer_collaborators and
// mentions them once per plan revision. The notice is a projection only: it
// grants nothing, and delivery failures never block the workflow.
func (m *Manager) NotifyPlanReviewers(ctx context.Context, n int) ([]string, error) {
	if m.gateway != nil || m.tracker == nil {
		return nil, nil
	}
	st, err := m.GetCurrentState(ctx, n)
	if err != nil {
		return nil, err
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("load approved reviewer policy: %w", err)
	}
	var reviewers []string
	for _, login := range cfg.Security.ReviewerCollaborators {
		login = strings.TrimSpace(login)
		if login == "" || (cfg.Security.IndependentReview && strings.EqualFold(login, st.Lease.Owner)) {
			continue
		}
		reviewers = append(reviewers, login)
	}
	if len(reviewers) == 0 {
		return nil, nil
	}
	var planDigest string
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactDesign {
			planDigest = hash(a.Body)
		}
	}
	marker := fmt.Sprintf("<!-- swarm:plan-review-request:v1 plan=%s -->", planDigest)
	self, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	comments, err := m.tracker.ListComments(ctx, n)
	if err != nil {
		return nil, err
	}
	for _, c := range comments {
		if strings.EqualFold(c.Author, self) && strings.Contains(c.Body, marker) {
			return reviewers, nil
		}
	}
	mentions := make([]string, len(reviewers))
	for i, login := range reviewers {
		mentions[i] = "@" + login
	}
	body := fmt.Sprintf("%s\n**Plan ready for review:** %s\n\nPlease review `PLAN.md` and `TEST_PLAN.md` for this WorkUnit, then record your decision:\n\n```bash\nswarm issue review-plan %d --approve\nswarm issue review-plan %d --reject --notes \"what to change\"\n```\n", marker, strings.Join(mentions, " "), n, n)
	if err := m.tracker.AddAssignees(ctx, n, reviewers); err != nil {
		return reviewers, err
	}
	if _, err := m.tracker.CreateComment(ctx, n, body); err != nil {
		return reviewers, err
	}
	return reviewers, nil
}

// RecordHumanReview records an authenticated maintainer decision without
// borrowing or extending the worker's lease.
func (m *Manager) RecordHumanReview(ctx context.Context, n int, verdict, notes string) (*IssueState, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("human review is not permitted in fork mode")
	}
	if verdict != "APPROVED" && verdict != "CHANGES_REQUESTED" {
		return nil, fmt.Errorf("invalid human review verdict %q", verdict)
	}
	if err := m.tracker.Authorize(ctx, "review-human"); err != nil {
		return nil, err
	}
	actor, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if st.Lease.Status == "completed" {
		return nil, ErrApprovalRequired
	}
	if err := m.checkApprovedState(ctx, n, st); err != nil {
		return nil, err
	}
	cfg, cfgErr := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
	if cfgErr != nil {
		return nil, fmt.Errorf("load approved reviewer policy: %w", cfgErr)
	}
	if !matchesCollaboratorAllowlist(actor, cfg.Security.ReviewerCollaborators) {
		return nil, fmt.Errorf("actor %q is not in security.reviewer_collaborators", actor)
	}
	if err := checkHumanReviewer(cfg.Security, st, actor); err != nil {
		return nil, err
	}
	var plan, test *contract.Artifact
	for i := range st.Artifacts {
		switch st.Artifacts[i].Type {
		case contract.ArtifactDesign:
			plan = &st.Artifacts[i]
		case contract.ArtifactTestPlan:
			test = &st.Artifacts[i]
		}
	}
	if plan == nil || test == nil {
		return nil, ErrApprovalRequired
	}
	body := "VERDICT: " + verdict + "\n\n" + notes
	if !reviewBodyMatchesVerdict(body, verdict) {
		return nil, fmt.Errorf("review notes contain a conflicting verdict")
	}
	st.ApprovalTransition = nil
	st.CarryReason = ""
	digest := hash(body)
	art := contract.Artifact{Version: 1, ID: fmt.Sprintf("art-%d-review", n), IssueNumber: n, Type: contract.ArtifactReview, Producer: "human:" + actor, Verdict: verdict, Summary: "human plan review", Body: body, CreatedAt: m.nowFn(), Metadata: map[string]string{"kind": "human", "actor": actor, "task_digest": st.TaskDigest, "policy_digest": st.PolicyDigest, "policy_tree_digest": st.PolicyTreeDigest, "plan_digest": hash(plan.Body), "test_plan_digest": hash(test.Body), "content_sha256": digest, "review_source": "authenticated-human", "base_sha": st.BaseSHA}}
	if err := contract.ValidateArtifact(&art); err != nil {
		return nil, err
	}
	RecordRoleAttestation(st, contract.RoleAttestation{
		Role:           "reviewer",
		Agent:          "human",
		Harness:        "human",
		HarnessVersion: "maintainer",
		Model:          "human",
		Sandbox:        "tracker",
		ArtifactType:   string(contract.ArtifactReview),
		ArtifactDigest: digest,
	})
	for i := range st.Artifacts {
		if st.Artifacts[i].Type == contract.ArtifactReview {
			art.Version = st.Artifacts[i].Version + 1
			st.Artifacts[i] = art
			return m.save(ctx, n, rev, st)
		}
	}
	st.Artifacts = append(st.Artifacts, art)
	return m.save(ctx, n, rev, st)
}

// RecordSubmission is fenced by the owner token while an attempt is active
// and makes PR identity durable for retry reconciliation.
func (m *Manager) RecordSubmission(ctx context.Context, n int, owner string, number int, url string) (*IssueState, error) {
	if number <= 0 {
		return nil, fmt.Errorf("submission requires PR number and URL")
	}
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{
			Version:     1,
			Operation:   "record-submission",
			IssueNumber: n,
			ClaimID:     m.token(n),
			PRNumber:    number,
		})
	}
	if url == "" {
		return nil, fmt.Errorf("submission requires PR number and URL")
	}
	st, err := m.mutate(ctx, n, owner, func(st *IssueState) error {
		if st.PRRevision != nil {
			return fmt.Errorf("issue #%d has a pending PR revision; use swarm pr submit %d", n, st.PRRevision.Number)
		}
		issue, err := m.tracker.GetIssue(ctx, n)
		if err != nil {
			return err
		}
		wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
		if err != nil {
			return err
		}
		if err := VerifyApproval(st, wu); err != nil {
			return err
		}

		st.PRNumber = number
		st.PRURL = url
		st.SubmittedHeadSHA = st.HeadSHA
		st.SubmittedBaseSHA = st.BaseSHA
		st.PROutcome = nil
		st.Lease.Status = "completed"
		st.Lease.Phase = "submitted"
		return nil
	})
	if err != nil {
		return nil, err
	}
	if m.tracker != nil {
		for _, label := range []string{"swarm:in-progress", "swarm:claimed"} {
			if err := m.tracker.RemoveLabel(ctx, n, label); err != nil {
				return st, err
			}
		}
		if err := m.tracker.AddLabels(ctx, n, []string{"swarm:pr-open"}); err != nil {
			return st, err
		}
	}
	return st, nil
}

// UpdateAdvisoryReview records contributor feedback without granting approval authority.
// The authenticated tracker identity, never artifact metadata, identifies its author.
func (m *Manager) UpdateAdvisoryReview(ctx context.Context, n int, owner string, art contract.Artifact) (*IssueState, error) {
	if art.Type != contract.ArtifactReview || art.Metadata["kind"] == "human" {
		return nil, fmt.Errorf("advisory review requires an agent review artifact")
	}
	return m.updateArtifact(ctx, n, owner, art, true)
}

func (m *Manager) UpdateArtifact(ctx context.Context, n int, owner string, art contract.Artifact) (*IssueState, error) {
	return m.updateArtifact(ctx, n, owner, art, false)
}

func (m *Manager) updateArtifact(ctx context.Context, n int, owner string, art contract.Artifact, advisory bool) (*IssueState, error) {
	if m.gateway != nil {
		artCopy := art
		return m.gateway.Apply(ctx, ForkRequest{
			Version:     1,
			Operation:   "update-artifact",
			IssueNumber: n,
			ClaimID:     m.token(n),
			Artifact:    &artCopy,
		})
	}
	st, err := m.mutate(ctx, n, owner, func(st *IssueState) error {
		if art.IssueNumber == 0 {
			art.IssueNumber = n
		}
		if art.IssueNumber != n {
			return fmt.Errorf("artifact issue identity mismatch")
		}
		if art.Version == 0 {
			art.Version = 1
		}
		if art.ID == "" {
			art.ID = fmt.Sprintf("art-%d-%s", n, art.Type)
		}
		if art.CreatedAt.IsZero() {
			art.CreatedAt = m.nowFn()
		}
		if art.Type == contract.ArtifactReview {
			if art.Metadata == nil {
				return fmt.Errorf("review approval metadata is required")
			}
			kind := art.Metadata["kind"]
			if kind != "agent" && kind != "human" {
				return fmt.Errorf("review kind must be agent or human")
			}
			actor, err := m.tracker.CurrentUser(ctx)
			if err != nil || strings.TrimSpace(actor) == "" {
				return fmt.Errorf("cannot authenticate review actor: %w", err)
			}
			art.Metadata["actor"] = actor
			art.Metadata["review_source"] = "contributor"
			if !advisory {
				permission := "approve-task"
				if kind == "human" {
					permission = "review-human"
				}
				if err := m.tracker.Authorize(ctx, permission); err != nil {
					return err
				}
				cfg, cfgErr := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
				if cfgErr != nil {
					return fmt.Errorf("load approved reviewer policy: %w", cfgErr)
				}
				if !matchesCollaboratorAllowlist(actor, cfg.Security.ReviewerCollaborators) {
					return fmt.Errorf("actor %q is not in security.reviewer_collaborators", actor)
				}
				if kind == "agent" {
					if err := checkAgentReviewer(cfg.Security, st, art.Metadata); err != nil {
						return err
					}
				} else if err := checkHumanReviewer(cfg.Security, st, actor); err != nil {
					return err
				}
				art.Metadata["review_source"] = "authenticated-" + kind
			}
			if !reviewBodyMatchesVerdict(art.Body, art.Verdict) {
				return fmt.Errorf("review body must contain one exact first-line verdict matching its verdict field")
			}
			var plan, test *contract.Artifact
			for i := range st.Artifacts {
				switch st.Artifacts[i].Type {
				case contract.ArtifactDesign:
					plan = &st.Artifacts[i]
				case contract.ArtifactTestPlan:
					test = &st.Artifacts[i]
				}
			}
			if plan == nil || test == nil {
				return fmt.Errorf("review requires current design and test-plan artifacts")
			}
			st.ApprovalTransition = nil
			st.CarryReason = ""
			bindings := ReviewBindings(st)
			if kind == "agent" && !advisory {
				for key, expected := range bindings {
					if expected == "" || art.Metadata[key] != expected {
						return fmt.Errorf("%w: reviewed inputs changed (%s)", ErrApprovalRequired, key)
					}
				}
			}
			for key, value := range bindings {
				art.Metadata[key] = value
			}
		}
		if art.Metadata == nil {
			art.Metadata = make(map[string]string)
		}
		digest := hash(art.Body)
		art.Metadata["content_sha256"] = digest
		if err := contract.ValidateArtifact(&art); err != nil {
			return err
		}
		if harnessName := strings.TrimSpace(art.Metadata["harness"]); harnessName != "" {
			role := strings.TrimSpace(art.Metadata["role"])
			if role == "" {
				switch art.Type {
				case contract.ArtifactDesign, contract.ArtifactTestPlan:
					role = "planner"
				case contract.ArtifactReview:
					role = "reviewer"
				case contract.ArtifactImplementation:
					role = "developer"
				case contract.ArtifactValidation:
					role = "tester"
				}
			}
			agentName := strings.TrimSpace(art.Metadata["agent"])
			if agentName == "" {
				agentName = harnessName
			}
			RecordRoleAttestation(st, contract.RoleAttestation{
				ExecutionMode:  art.Metadata["execution_mode"],
				Authentication: art.Metadata["authentication"],
				Role:           role,
				Agent:          agentName,
				Harness:        harnessName,
				HarnessVersion: art.Metadata["harness_version"],
				Model:          art.Metadata["model"],
				Sandbox:        art.Metadata["sandbox"],
				ArtifactType:   string(art.Type),
				ArtifactDigest: digest,
			})
		}
		if art.Type == contract.ArtifactDesign || art.Type == contract.ArtifactTestPlan {
			var kept []contract.Artifact
			for _, a := range st.Artifacts {
				if a.Type != contract.ArtifactReview {
					kept = append(kept, a)
				}
			}
			st.Artifacts = kept
		}
		for i, a := range st.Artifacts {
			if a.Type == art.Type {
				art.Version = a.Version + 1
				st.Artifacts[i] = art
				return nil
			}
		}
		st.Artifacts = append(st.Artifacts, art)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if m.tracker != nil {
		switch art.Type {
		case contract.ArtifactTestPlan:
			if st.EffectiveGate != contract.GateNone {
				_ = m.tracker.AddLabels(ctx, n, []string{"swarm:plan-review"})
			}
		case contract.ArtifactReview:
			if art.Verdict == "APPROVED" && !advisory {
				if err := m.tracker.RemoveLabel(ctx, n, "swarm:plan-review"); err != nil {
					return st, err
				}
				if err := m.tracker.AddLabels(ctx, n, []string{"swarm:plan-approved"}); err != nil {
					return st, err
				}
			} else {
				_ = m.tracker.RemoveLabel(ctx, n, "swarm:plan-approved")
				if err := m.tracker.AddLabels(ctx, n, []string{"swarm:plan-review"}); err != nil {
					return st, err
				}
			}
		}
	}
	return st, nil
}
func (m *Manager) RecordCheckpoint(ctx context.Context, n int, owner, branch, head string) (*IssueState, error) {
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{
			Version:     1,
			Operation:   "record-checkpoint",
			IssueNumber: n,
			ClaimID:     m.token(n),
			Branch:      branch,
			HeadSHA:     head,
		})
	}
	if branch == "" || head == "" || !strings.HasPrefix(branch, fmt.Sprintf("swarm/issue-%d-", n)) {
		return nil, fmt.Errorf("invalid checkpoint branch")
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if err = m.checkStateClaim(ctx, n, owner, st); err != nil {
		return nil, err
	}
	if st.PRRevision != nil {
		return nil, fmt.Errorf("issue #%d has a pending PR revision; issue checkpoint publication is blocked", n)
	}
	if st.ForkRepo != "" && branch != fmt.Sprintf("swarm/issue-%d-%s", n, st.Lease.ClaimID) {
		return nil, fmt.Errorf("fork checkpoint branch %q does not match active claim %q", branch, st.Lease.ClaimID)
	}
	if st.Branch != "" && st.Branch != branch {
		return nil, fmt.Errorf("checkpoint branch differs from approved branch")
	}
	if st.ProjectionCommentID == 0 {
		if existingID := m.findProjectionCommentID(ctx, n); existingID > 0 {
			st.ProjectionCommentID = existingID
		}
	}
	st.Branch = branch
	st.HeadSHA = head
	if ps, ok := m.store.(coordination.PublishingStore); ok {
		raw, err := json.Marshal(st)
		if err != nil {
			return nil, err
		}
		if _, err = ps.CompareAndSwapAndPublish(ctx, n, rev, raw, branch, head); err != nil {
			if errors.Is(err, coordination.ErrConflict) {
				return nil, ErrClaimFence
			}
			return nil, err
		}
		m.project(ctx, n, st)
		return st, nil
	}
	return m.save(ctx, n, rev, st)
}
func (m *Manager) GetCurrentState(ctx context.Context, n int) (*IssueState, error) {
	st, _, err := m.loadReadOnly(ctx, n)
	return st, err
}

// BeginPRRevision fences a new local revision without changing the submitted
// issue checkpoint. Development invalidates approval; rebase retains only
// authenticated eligible plans and always renews head-dependent evidence.
func (m *Manager) BeginPRRevision(ctx context.Context, n int, owner string, number int, remoteHead, localHead, branch string, intent ...string) (*IssueState, error) {
	if number <= 0 || remoteHead == "" || localHead == "" || branch == "" {
		return nil, fmt.Errorf("incomplete PR revision identity")
	}
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{Version: 1, Operation: "begin-pr-revision", IssueNumber: n, ClaimID: m.token(n), PRNumber: number, ExpectedHeadSHA: remoteHead, HeadSHA: localHead, Branch: branch, RevisionIntent: revisionIntent(intent)})
	}
	return m.mutate(ctx, n, owner, func(st *IssueState) error {
		if st.PRNumber != number {
			return fmt.Errorf("PR #%d is not linked to issue #%d", number, n)
		}
		if st.PRRevision != nil {
			if st.PRRevision.Number != number || st.PRRevision.RemoteHeadSHA != remoteHead || st.PRRevision.Branch != branch {
				return fmt.Errorf("pending PR revision identity changed; submit or resolve the existing revision first")
			}
			if st.PRRevision.LocalHeadSHA != localHead {
				return fmt.Errorf("local PR head changed outside the recorded revision")
			}
			if revisionIntent(intent) == "rebase" {
				startRebaseEvidence(st, localHead)
				m.rebaseEvidence(ctx, st, n)
				st.PRRevision.Intent = "rebase"
			} else if st.PRRevision.Intent == "rebase" {
				invalidatePRRevisionEvidence(st)
				st.CarryReason = "new development requires plan review"
				st.PRRevision.Intent = "work"
			}
			return nil
		}
		st.PRRevision = &PRRevision{Number: number, RemoteHeadSHA: remoteHead, LocalHeadSHA: localHead, Branch: branch, Intent: revisionIntent(intent)}
		if revisionIntent(intent) == "rebase" {
			startRebaseEvidence(st, localHead)
			verifiedHead := st.SubmittedHeadSHA
			if verifiedHead == "" {
				verifiedHead = st.HeadSHA
			}
			if verifiedHead != remoteHead {
				invalidatePRRevisionEvidence(st)
				st.CarryReason = "published head changed outside SwarmOS"
			} else {
				m.rebaseEvidence(ctx, st, n)
			}
		} else {
			invalidatePRRevisionEvidence(st)
		}
		return nil
	})
}

func invalidatePRRevisionEvidence(st *IssueState) {
	st.ApprovalTransition = nil
	st.Artifacts = nil
	st.Attestations = nil
}

// RecordPRHead publishes a local checkpoint to the issue coordination store,
// not to the PR's remote head ref.
func (m *Manager) RecordPRHead(ctx context.Context, n int, owner string, number int, branch, head string) (*IssueState, error) {
	if branch == "" || head == "" {
		return nil, fmt.Errorf("incomplete PR checkpoint")
	}
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{Version: 1, Operation: "record-pr-head", IssueNumber: n, ClaimID: m.token(n), PRNumber: number, Branch: branch, HeadSHA: head})
	}
	return m.mutate(ctx, n, owner, func(st *IssueState) error {
		if st.PRRevision == nil || st.PRRevision.Number != number || st.PRRevision.Branch != branch {
			return fmt.Errorf("PR checkpoint does not match pending revision")
		}
		if st.PRRevision.LocalHeadSHA != head {
			kept := st.Artifacts[:0]
			for _, art := range st.Artifacts {
				if art.Type != contract.ArtifactImplementation && art.Type != contract.ArtifactValidation {
					kept = append(kept, art)
				}
			}
			st.Artifacts = kept
			attestations := st.Attestations[:0]
			for _, att := range st.Attestations {
				if att.ArtifactType != string(contract.ArtifactImplementation) && att.ArtifactType != string(contract.ArtifactValidation) {
					attestations = append(attestations, att)
				}
			}
			st.Attestations = attestations
		}
		st.PRRevision.LocalHeadSHA = head
		return nil
	})
}

// CompletePRRevision clears the pending revision only if both remote and
// locally validated heads are exactly the publication snapshot.
func (m *Manager) CompletePRRevision(ctx context.Context, n int, owner string, number int, expected, published string) (*IssueState, error) {
	return m.completePRRevision(ctx, n, owner, number, expected, published, true)
}

// CompletePRRevisionLocal records a publication already fenced by the local
// tracker or a separately authorized fork head remote. Origin publications
// must use CompletePRRevision's atomic coordination-and-head CAS instead.
func (m *Manager) CompletePRRevisionLocal(ctx context.Context, n int, owner string, number int, expected, published string) (*IssueState, error) {
	return m.completePRRevision(ctx, n, owner, number, expected, published, false)
}

func (m *Manager) completePRRevision(ctx context.Context, n int, owner string, number int, expected, published string, atomicOrigin bool) (*IssueState, error) {
	if m.gateway != nil {
		if atomicOrigin {
			return nil, fmt.Errorf("contributors publish PR revisions to their fork, not origin")
		}
		// The coordinator re-verifies the published head on GitHub before completing.
		return m.gateway.Apply(ctx, ForkRequest{Version: 1, Operation: "complete-pr-revision", IssueNumber: n, ClaimID: m.token(n), PRNumber: number, ExpectedHeadSHA: expected, HeadSHA: published})
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if err = m.checkStateClaim(ctx, n, owner, st); err != nil {
		return nil, err
	}
	r := st.PRRevision
	if r == nil || st.PRNumber != number || r.Number != number || r.RemoteHeadSHA != expected || r.LocalHeadSHA != published {
		return nil, fmt.Errorf("PR publication snapshot changed")
	}
	verifyRebaseEvidence(ctx, m.repoRoot, st, published)
	st.PRRevision = nil
	st.HeadSHA = published
	st.SubmittedHeadSHA = published
	st.SubmittedBaseSHA = st.BaseSHA
	st.PROutcome = nil
	st.Lease.Status = "completed"
	st.Lease.Phase = "submitted"
	if atomicOrigin {
		ps, ok := m.store.(coordination.ExpectedPRPublishingStore)
		if !ok {
			return nil, fmt.Errorf("origin PR publication requires a coordination store that publishes the PR head atomically")
		}
		m.stampForSave(ctx, n, st)
		raw, err := json.Marshal(st)
		if err != nil {
			return nil, err
		}
		if _, err = ps.CompareAndSwapAndPublishPR(ctx, n, rev, raw, st.Branch, expected, published); err != nil {
			return nil, fmt.Errorf("atomic PR publication failed without changing either remote ref: %w", err)
		}
		m.project(ctx, n, st)
		return st, nil
	}
	return m.save(ctx, n, rev, st)
}

// InvalidatePRRevisionEvidence requires the same claim fence as execution.
func (m *Manager) InvalidatePRRevisionEvidence(ctx context.Context, n int, owner string, number int, intent ...string) (*IssueState, error) {
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{Version: 1, Operation: "invalidate-pr-evidence", IssueNumber: n, ClaimID: m.token(n), PRNumber: number, RevisionIntent: revisionIntent(intent)})
	}
	return m.mutate(ctx, n, owner, func(st *IssueState) error {
		if st.PRRevision == nil || st.PRRevision.Number != number {
			return fmt.Errorf("no pending revision for PR #%d", number)
		}
		if revisionIntent(intent) == "rebase" {
			m.rebaseEvidence(ctx, st, n)
		} else {
			invalidatePRRevisionEvidence(st)
		}
		return nil
	})
}

// TesterMetadata adds the recorded tester attestation, if any, to a validation
// artifact's metadata so every validation path reports the same provenance.
func TesterMetadata(st *IssueState, base map[string]string) map[string]string {
	if st != nil {
		for _, att := range st.Attestations {
			if att.Role == "tester" {
				return att.Metadata(base)
			}
		}
	}
	return base
}

// ReviewBindings identifies the exact coordinator snapshot a reviewer receives.
// The publisher must return these bindings unchanged; a newer plan or lease
// cannot inherit the verdict of a review that is still running.
func ReviewBindings(st *IssueState) map[string]string {
	bindings := map[string]string{}
	if st == nil {
		return bindings
	}
	bindings["task_digest"] = st.TaskDigest
	bindings["policy_digest"] = st.PolicyDigest
	if st.PolicyTreeDigest != "" {
		bindings["policy_tree_digest"] = st.PolicyTreeDigest
	}
	bindings["base_sha"] = st.BaseSHA
	bindings["review_claim_id"] = st.Lease.ClaimID
	for _, art := range st.Artifacts {
		switch art.Type {
		case contract.ArtifactDesign:
			bindings["plan_digest"] = hash(art.Body)
		case contract.ArtifactTestPlan:
			bindings["test_plan_digest"] = hash(art.Body)
		}
	}
	return bindings
}

// TaskApprovalCurrent reports whether state still approves exactly this
// WorkUnit revision under the policy approved at its base commit.
func TaskApprovalCurrent(st *IssueState, wu *contract.WorkUnit, cfg *contract.SwarmConfig) bool {
	return st != nil && st.TaskDigest == contract.DigestWorkUnit(wu) && st.PolicyDigest == contract.DigestApprovedPolicy(st.BaseSHA, cfg)
}

func VerifyApproval(st *IssueState, wu *contract.WorkUnit) error {
	if st == nil || wu == nil || st.TaskDigest != contract.DigestWorkUnit(wu) {
		return ErrApprovalRequired
	}
	gate := st.EffectiveGate
	if gate == "" {
		var err error
		gate, err = contract.ResolveGate(wu.Gate, "", "")
		if err != nil {
			return err
		}
	}
	if gate == contract.GateNone {
		return nil
	}
	var plan, test, review *contract.Artifact
	for i := range st.Artifacts {
		a := &st.Artifacts[i]
		switch a.Type {
		case contract.ArtifactDesign:
			plan = a
		case contract.ArtifactTestPlan:
			test = a
		case contract.ArtifactReview:
			review = a
		}
	}
	if plan == nil || test == nil || review == nil || review.Verdict != "APPROVED" {
		return ErrApprovalRequired
	}
	if review.Metadata["actor"] == "" || review.Metadata["review_source"] != "authenticated-"+review.Metadata["kind"] ||
		!reviewBaseMatches(st, review) || review.Metadata["content_sha256"] != hash(review.Body) ||
		!reviewBodyMatchesVerdict(review.Body, review.Verdict) {
		return ErrApprovalRequired
	}
	if review.Metadata["task_digest"] != st.TaskDigest || review.Metadata["plan_digest"] != hash(plan.Body) || review.Metadata["test_plan_digest"] != hash(test.Body) {
		return ErrApprovalRequired
	}
	if tree := review.Metadata["policy_tree_digest"]; tree != "" && tree != st.PolicyTreeDigest {
		return ErrApprovalRequired
	}
	kind := review.Metadata["kind"]
	if gate == contract.GateAgent && kind != "agent" && !(kind == "human" && review.Metadata["actor"] != "") {
		return ErrApprovalRequired
	}
	if gate == contract.GateHuman && (kind != "human" || review.Metadata["actor"] == "") {
		return ErrApprovalRequired
	}
	return nil
}

func hash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// Review bodies are machine-readable decisions followed by optional narrative.
func reviewBodyMatchesVerdict(body, verdict string) bool {
	if verdict != "APPROVED" && verdict != "CHANGES_REQUESTED" {
		return false
	}
	seen := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !seen {
			if line != "VERDICT: "+verdict {
				return false
			}
			seen = true
		} else if strings.HasPrefix(line, "VERDICT:") {
			return false
		}
	}
	return seen
}
