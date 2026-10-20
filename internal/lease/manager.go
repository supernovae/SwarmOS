package lease

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

type IssueState struct {
	Lease         contract.Lease      `json:"lease"`
	Artifacts     []contract.Artifact `json:"artifacts,omitempty"`
	TaskDigest    string              `json:"task_digest"`
	PolicyDigest  string              `json:"policy_digest"`
	BaseSHA       string              `json:"base_sha"`
	HeadSHA       string              `json:"head_sha,omitempty"`
	Branch        string              `json:"branch,omitempty"`
	ApprovedBy    string              `json:"approved_by,omitempty"`
	ApprovedAt    time.Time           `json:"approved_at,omitempty"`
	PRNumber      int                 `json:"pr_number,omitempty"`
	EffectiveGate contract.GateMode   `json:"effective_gate,omitempty"`
	PRURL         string              `json:"pr_url,omitempty"`
}
type Manager struct {
	tracker     tracker.Tracker
	security    contract.SecurityConfig
	store       coordination.Store
	repoRoot    string
	nodeProfile *contract.NodeProfile
	nowFn       func() time.Time
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
func (m *Manager) project(ctx context.Context, n int, st *IssueState) {
	summary := fmt.Sprintf("<!-- swarm:audit:v1 -->\n### SwarmOS state projection\n\nTask `%s`, claim `%s`, phase `%s`, status `%s`.\n", short(st.TaskDigest), st.Lease.ClaimID, st.Lease.Phase, st.Lease.Status)
	_, _ = m.tracker.CreateComment(ctx, n, summary)
}
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
func (m *Manager) ApproveTask(ctx context.Context, n int, owner string) (*IssueState, error) {
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
	if old, rev, err := m.load(ctx, n); err == nil {
		if old.Lease.Status == "active" && old.Lease.ExpiresAt.After(m.nowFn()) {
			return nil, ErrAlreadyClaimed
		}
		old.TaskDigest = contract.DigestWorkUnit(wu)
		old.PolicyDigest = digest
		old.BaseSHA = base
		old.EffectiveGate = gate
		old.ApprovedBy = owner
		old.ApprovedAt = m.nowFn()
		if _, err = m.save(ctx, n, rev, old); err != nil {
			return nil, err
		}
		if err = m.tracker.AddLabels(ctx, n, []string{sec.ReadyLabel}); err != nil {
			return nil, err
		}
		return old, nil
	} else if !errors.Is(err, coordination.ErrNotFound) {
		return nil, err
	}
	st := &IssueState{Lease: contract.Lease{Version: 1, IssueNumber: n, Status: "draft"}, TaskDigest: contract.DigestWorkUnit(wu), PolicyDigest: digest, BaseSHA: base, EffectiveGate: gate, ApprovedBy: owner, ApprovedAt: m.nowFn()}
	if _, err = m.save(ctx, n, "", st); err != nil {
		return nil, err
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
func (m *Manager) Acquire(ctx context.Context, n int, owner, agent, phase string, duration time.Duration, max int) (*IssueState, error) {
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
	wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
	if err != nil {
		return nil, err
	}
	if err = m.VerifyDependencies(ctx, wu); err != nil {
		return nil, err
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, fmt.Errorf("%w: approve task first", ErrApprovalRequired)
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("load approved policy: %w", err)
	}
	if err = verifyTrust(cfg.Security, issue); err != nil {
		return nil, err
	}
	if st.TaskDigest != contract.DigestWorkUnit(wu) || st.PolicyDigest != contract.DigestApprovedPolicy(st.BaseSHA, cfg) {
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
	if st.TaskDigest != contract.DigestWorkUnit(wu) || st.PolicyDigest != contract.DigestApprovedPolicy(st.BaseSHA, cfg) {
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
	for attempt := 0; attempt < 4; attempt++ {
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
		st.Lease.Renewals++
		st.Lease.ExpiresAt = m.nowFn().Add(d)
		return nil
	})
}
func (m *Manager) Release(ctx context.Context, n int, owner, reason string) (*IssueState, error) {
	st, err := m.mutate(ctx, n, owner, func(st *IssueState) error { st.Lease.Status = "released"; st.Lease.ExpiresAt = m.nowFn(); return nil })
	if err == nil {
		_ = m.tracker.RemoveLabel(ctx, n, "swarm:claimed")
		_ = m.tracker.RemoveLabel(ctx, n, "swarm:in-progress")
	}
	return st, err
}
func (m *Manager) Handoff(ctx context.Context, n int, owner, done, next string) (*IssueState, error) {
	return m.mutate(ctx, n, owner, func(st *IssueState) error {
		if err := contract.ValidatePhase(next); err != nil {
			return err
		}
		st.Lease.Phase = done
		st.Lease.NextPhase = next
		st.Lease.Status = "handoff"
		st.Lease.ExpiresAt = m.nowFn()
		return nil
	})
}

// RequireGate persists only stronger execution requirements for this approval.
func (m *Manager) RequireGate(ctx context.Context, issue int, owner string, requested contract.GateMode) (*IssueState, error) {
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
	return m.mutate(ctx, n, owner, func(st *IssueState) error {
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
}

// RecordHumanReview records an authenticated maintainer decision without
// borrowing or extending the worker's lease.
func (m *Manager) RecordHumanReview(ctx context.Context, n int, verdict, notes string) (*IssueState, error) {
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
	art := contract.Artifact{Version: 1, ID: fmt.Sprintf("art-%d-review", n), IssueNumber: n, Type: contract.ArtifactReview, Producer: "human:" + actor, Verdict: verdict, Summary: "human plan review", Body: "VERDICT: " + verdict + "\n\n" + notes, CreatedAt: m.nowFn(), Metadata: map[string]string{"kind": "human", "actor": actor, "task_digest": st.TaskDigest, "policy_digest": st.PolicyDigest, "plan_digest": hash(plan.Body), "test_plan_digest": hash(test.Body)}}
	if err := contract.ValidateArtifact(&art); err != nil {
		return nil, err
	}
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
	if number <= 0 || url == "" {
		return nil, fmt.Errorf("submission requires PR number and URL")
	}
	return m.mutate(ctx, n, owner, func(st *IssueState) error {
		st.PRNumber = number
		st.PRURL = url
		st.Lease.Status = "completed"
		st.Lease.Phase = "submitted"
		return nil
	})
}
func (m *Manager) UpdateArtifact(ctx context.Context, n int, owner string, art contract.Artifact) (*IssueState, error) {
	return m.mutate(ctx, n, owner, func(st *IssueState) error {
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
			if art.Metadata["actor"] == "" {
				return fmt.Errorf("review actor metadata is required")
			}
			if kind == "human" {
				if err := m.tracker.Authorize(ctx, "review-human"); err != nil {
					return err
				}
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
			art.Metadata["task_digest"] = st.TaskDigest
			art.Metadata["policy_digest"] = st.PolicyDigest
			art.Metadata["plan_digest"] = hash(plan.Body)
			art.Metadata["test_plan_digest"] = hash(test.Body)
		}
		if err := contract.ValidateArtifact(&art); err != nil {
			return err
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
}
func (m *Manager) RecordCheckpoint(ctx context.Context, n int, owner, branch, head string) (*IssueState, error) {
	if branch == "" || head == "" || !strings.HasPrefix(branch, fmt.Sprintf("swarm/issue-%d-", n)) {
		return nil, fmt.Errorf("invalid checkpoint branch")
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if st.Lease.Status != "active" || st.Lease.Owner != owner || st.Lease.ClaimID != m.token(n) || !st.Lease.ExpiresAt.After(m.nowFn()) {
		return nil, ErrClaimFence
	}
	if st.Branch != "" && st.Branch != branch {
		return nil, fmt.Errorf("checkpoint branch differs from approved branch")
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
	st, _, err := m.load(ctx, n)
	return st, err
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
	if review.Metadata["task_digest"] != st.TaskDigest || review.Metadata["policy_digest"] != st.PolicyDigest || review.Metadata["plan_digest"] != hash(plan.Body) || review.Metadata["test_plan_digest"] != hash(test.Body) {
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
