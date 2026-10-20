package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
)

var (
	ErrUntrustedIssue    = errors.New("untrusted issue: missing required ready label or trusted author association")
	ErrAlreadyClaimed    = errors.New("issue is already claimed by another active lease")
	ErrClaimRaceLost     = errors.New("claim race lost to a concurrent claimant with an earlier comment ID")
	ErrNoActiveLease     = errors.New("no active lease found for issue")
	ErrMaxRenewals       = errors.New("maximum lease renewals reached")
	ErrDependenciesUnmet = errors.New("prerequisite depends_on issues are not yet completed")
)

type AlreadyClaimedError struct {
	IssueNumber int
	Owner       string
	ExpiresAt   time.Time
	CommentID   int64
}

func (e *AlreadyClaimedError) Error() string {
	return fmt.Sprintf("%s: issue #%d is claimed by %q until %s (comment %d)",
		ErrAlreadyClaimed.Error(), e.IssueNumber, e.Owner, e.ExpiresAt.Format(time.RFC3339), e.CommentID)
}

func (e *AlreadyClaimedError) Unwrap() error {
	return ErrAlreadyClaimed
}

type IssueState struct {
	Lease     contract.Lease      `json:"lease"`
	Artifacts []contract.Artifact `json:"artifacts,omitempty"`
}

var stateMarkerRegex = regexp.MustCompile(`(?s)<!--\s*swarm:state:v1\s*(\{.*?\})\s*-->`)

type Manager struct {
	tracker     tracker.Tracker
	security    contract.SecurityConfig
	nodeProfile *contract.NodeProfile
	nowFn       func() time.Time
}

func NewManager(t tracker.Tracker, sec contract.SecurityConfig) *Manager {
	if sec.ReadyLabel == "" {
		sec.ReadyLabel = "swarm:ready"
	}
	return &Manager{
		tracker:  t,
		security: sec,
		nowFn:    func() time.Time { return time.Now().UTC() },
	}
}

func (m *Manager) SetNodeProfile(prof *contract.NodeProfile) {
	m.nodeProfile = prof
}

func (m *Manager) SetNowFunc(fn func() time.Time) {
	m.nowFn = fn
}

func (m *Manager) VerifyTrustPolicy(issue *tracker.Issue) error {
	if !m.security.RequireReadyLabel {
		return nil
	}
	hasReady := slices.Contains(issue.Labels, m.security.ReadyLabel)
	isTrustedAssoc := false
	for _, assoc := range m.security.TrustedAssociations {
		if strings.EqualFold(assoc, issue.AuthorAssociation) {
			isTrustedAssoc = true
			break
		}
	}
	if !hasReady && !isTrustedAssoc {
		return fmt.Errorf("%w: issue #%d requires label %q or author association in %v (got %q)",
			ErrUntrustedIssue, issue.Number, m.security.ReadyLabel, m.security.TrustedAssociations, issue.AuthorAssociation)
	}
	return nil
}

// VerifyDependencies checks whether all issue numbers in wu.DependsOn are either closed, have swarm:pr-open, or have Lease.Status == "completed".
func (m *Manager) VerifyDependencies(ctx context.Context, wu *contract.WorkUnit) error {
	if wu == nil || len(wu.DependsOn) == 0 {
		return nil
	}
	var blocking []string
	for _, depNum := range wu.DependsOn {
		if depNum == wu.IssueNumber {
			continue
		}
		depIss, err := m.tracker.GetIssue(ctx, depNum)
		if err != nil {
			blocking = append(blocking, fmt.Sprintf("#%d (not found)", depNum))
			continue
		}
		if strings.EqualFold(depIss.State, "closed") || slices.Contains(depIss.Labels, "swarm:pr-open") {
			continue
		}
		if st, err := m.GetCurrentState(ctx, depNum); err == nil && st != nil && st.Lease.Status == "completed" {
			continue
		}
		blocking = append(blocking, fmt.Sprintf("#%d (%s)", depNum, depIss.Title))
	}
	if len(blocking) > 0 {
		return fmt.Errorf("%w: issue #%d is blocked by [%s]", ErrDependenciesUnmet, wu.IssueNumber, strings.Join(blocking, ", "))
	}
	return nil
}

// Acquire claims an issue using optimistic concurrency and comment-ID tie-breaking.
// Also supports collaborative team handoffs: if the previous claimant placed the lease in status == "handoff",
// the new contributor seamlessly claims the next phase while preserving prior artifacts and contributor history.
func (m *Manager) Acquire(ctx context.Context, issueNum int, owner, agent, phase string, duration time.Duration, maxRenewals int) (*IssueState, error) {
	if duration <= 0 {
		duration = 4 * time.Hour
	}
	if maxRenewals <= 0 {
		maxRenewals = 3
	}
	if phase == "" {
		phase = "full"
	}

	issue, err := m.tracker.GetIssue(ctx, issueNum)
	if err != nil {
		return nil, err
	}

	if err := m.VerifyTrustPolicy(issue); err != nil {
		return nil, err
	}

	now := m.nowFn()
	comments, err := m.tracker.ListComments(ctx, issueNum)
	if err != nil {
		return nil, err
	}

	var reusableState *IssueState
	for _, c := range comments {
		st, ok := ParseStateComment(c.Body)
		if !ok {
			continue
		}
		st.Lease.CommentID = c.ID
		if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now) {
			if st.Lease.Owner != owner {
				return nil, &AlreadyClaimedError{
					IssueNumber: issueNum,
					Owner:       st.Lease.Owner,
					ExpiresAt:   st.Lease.ExpiresAt,
					CommentID:   c.ID,
				}
			}
			reusableState = st
		} else if st.Lease.Status == "handoff" || st.Lease.Owner == owner || (st.Lease.Status == "active" && !st.Lease.ExpiresAt.After(now)) {
			// Collaborative handoff or expired/own state comment: preserve artifacts and contributor chain!
			reusableState = st
		}
	}

	contributors := []string{owner}
	if reusableState != nil {
		for _, prev := range reusableState.Lease.Contributors {
			if prev != "" && !slices.Contains(contributors, prev) {
				contributors = append(contributors, prev)
			}
		}
		if reusableState.Lease.Owner != "" && !slices.Contains(contributors, reusableState.Lease.Owner) {
			contributors = append([]string{reusableState.Lease.Owner}, contributors...)
		}
		if (phase == "full" || phase == "all") && reusableState.Lease.Status == "handoff" && reusableState.Lease.NextPhase != "" {
			phase = reusableState.Lease.NextPhase
		}
	}

	var nodeID string
	var nodeCaps []string
	if m.nodeProfile != nil {
		nodeID = m.nodeProfile.NodeID
		nodeCaps = m.nodeProfile.Capabilities
	}

	newState := &IssueState{
		Lease: contract.Lease{
			Version:      1,
			IssueNumber:  issueNum,
			Owner:        owner,
			Agent:        agent,
			NodeID:       nodeID,
			Capabilities: nodeCaps,
			Contributors: contributors,
			Phase:        phase,
			Status:       "active",
			ClaimedAt:    now,
			ExpiresAt:    now.Add(duration),
			Renewals:     0,
			MaxRenewals:  maxRenewals,
		},
	}
	if reusableState != nil {
		newState.Artifacts = reusableState.Artifacts
	}

	if reusableState != nil && reusableState.Lease.CommentID > 0 && (reusableState.Lease.Status == "active" || reusableState.Lease.Status == "handoff") {
		newState.Lease.CommentID = reusableState.Lease.CommentID
		body, err := FormatStateComment(newState)
		if err != nil {
			return nil, err
		}
		if _, err := m.tracker.UpdateComment(ctx, reusableState.Lease.CommentID, body); err != nil {
			return nil, err
		}
	} else {
		body, err := FormatStateComment(newState)
		if err != nil {
			return nil, err
		}
		postedComment, err := m.tracker.CreateComment(ctx, issueNum, body)
		if err != nil {
			return nil, err
		}
		newState.Lease.CommentID = postedComment.ID
		updatedBody, err := FormatStateComment(newState)
		if err == nil {
			_, _ = m.tracker.UpdateComment(ctx, postedComment.ID, updatedBody)
		}
	}

	// Re-fetch comments to verify optimistic concurrency tie-breaking by lowest CommentID
	verifyComments, err := m.tracker.ListComments(ctx, issueNum)
	if err != nil {
		return nil, err
	}

	type activeClaim struct {
		commentID int64
		state     *IssueState
	}
	var activeClaims []activeClaim
	for _, c := range verifyComments {
		st, ok := ParseStateComment(c.Body)
		if !ok {
			continue
		}
		st.Lease.CommentID = c.ID
		if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now) {
			activeClaims = append(activeClaims, activeClaim{commentID: c.ID, state: st})
		}
	}

	sort.Slice(activeClaims, func(i, j int) bool {
		return activeClaims[i].commentID < activeClaims[j].commentID
	})

	if len(activeClaims) > 0 && activeClaims[0].commentID != newState.Lease.CommentID && activeClaims[0].state.Lease.Owner != owner {
		newState.Lease.Status = "superseded"
		if body, err := FormatStateComment(newState); err == nil {
			_, _ = m.tracker.UpdateComment(ctx, newState.Lease.CommentID, body)
		}
		return nil, fmt.Errorf("%w: issue #%d won by %q (comment %d < %d)",
			ErrClaimRaceLost, issueNum, activeClaims[0].state.Lease.Owner, activeClaims[0].commentID, newState.Lease.CommentID)
	}

	_ = m.tracker.AddLabels(ctx, issueNum, []string{"swarm:claimed", "swarm:in-progress"})
	return newState, nil
}

// Handoff transitions an active lease into collaborative handoff mode ("status": "handoff") so another contributor or specialist node
// can claim the next workflow phase (e.g., "review" or "implement") while retaining all artifacts.
func (m *Manager) Handoff(ctx context.Context, issueNum int, owner, completedPhase, nextPhase string) (*IssueState, error) {
	st, commentID, err := m.findLatestState(ctx, issueNum, owner)
	if err != nil {
		return nil, err
	}
	if completedPhase != "" {
		st.Lease.Phase = completedPhase
	}
	st.Lease.NextPhase = nextPhase
	st.Lease.Status = "handoff"
	st.Lease.ExpiresAt = m.nowFn()
	if owner != "" && !slices.Contains(st.Lease.Contributors, owner) {
		st.Lease.Contributors = append(st.Lease.Contributors, owner)
	}

	body, err := FormatStateComment(st)
	if err != nil {
		return nil, err
	}
	if _, err := m.tracker.UpdateComment(ctx, commentID, body); err != nil {
		return nil, err
	}
	_ = m.tracker.RemoveLabel(ctx, issueNum, "swarm:in-progress")
	return st, nil
}

func (m *Manager) Renew(ctx context.Context, issueNum int, owner string, duration time.Duration) (*IssueState, error) {
	if duration <= 0 {
		duration = 4 * time.Hour
	}
	st, commentID, err := m.findOwnerState(ctx, issueNum, owner)
	if err != nil {
		return nil, err
	}
	if st.Lease.Status != "active" {
		return nil, fmt.Errorf("%w: lease status is %q", ErrNoActiveLease, st.Lease.Status)
	}
	if st.Lease.MaxRenewals > 0 && st.Lease.Renewals >= st.Lease.MaxRenewals {
		return nil, fmt.Errorf("%w (%d/%d)", ErrMaxRenewals, st.Lease.Renewals, st.Lease.MaxRenewals)
	}
	now := m.nowFn()
	st.Lease.Renewals++
	st.Lease.ExpiresAt = now.Add(duration)
	st.Lease.CommentID = commentID

	body, err := FormatStateComment(st)
	if err != nil {
		return nil, err
	}
	if _, err := m.tracker.UpdateComment(ctx, commentID, body); err != nil {
		return nil, err
	}
	return st, nil
}

func (m *Manager) Release(ctx context.Context, issueNum int, owner, reason string) (*IssueState, error) {
	st, commentID, err := m.findOwnerState(ctx, issueNum, owner)
	if err != nil {
		return nil, err
	}
	st.Lease.Status = "released"
	st.Lease.ExpiresAt = m.nowFn()
	st.Lease.CommentID = commentID

	body, err := FormatStateComment(st)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		body += fmt.Sprintf("\n\n_Lease released by `%s`: %s_\n", owner, reason)
	}
	if _, err := m.tracker.UpdateComment(ctx, commentID, body); err != nil {
		return nil, err
	}
	_ = m.tracker.RemoveLabel(ctx, issueNum, "swarm:claimed")
	_ = m.tracker.RemoveLabel(ctx, issueNum, "swarm:in-progress")
	return st, nil
}

func (m *Manager) UpdateArtifact(ctx context.Context, issueNum int, owner string, art contract.Artifact) (*IssueState, error) {
	if art.Version == 0 {
		art.Version = 1
	}
	if art.IssueNumber == 0 {
		art.IssueNumber = issueNum
	}
	if art.CreatedAt.IsZero() {
		art.CreatedAt = m.nowFn()
	}
	if art.ID == "" {
		art.ID = fmt.Sprintf("art-%d-%s", issueNum, art.Type)
	}
	if err := contract.ValidateArtifact(&art); err != nil {
		return nil, err
	}

	st, commentID, err := m.findLatestState(ctx, issueNum, owner)
	if err != nil {
		return nil, err
	}

	replaced := false
	for idx, existing := range st.Artifacts {
		if existing.Type == art.Type {
			art.Version = existing.Version + 1
			st.Artifacts[idx] = art
			replaced = true
			break
		}
	}
	if !replaced {
		st.Artifacts = append(st.Artifacts, art)
	}

	body, err := FormatStateComment(st)
	if err != nil {
		return nil, err
	}
	if _, err := m.tracker.UpdateComment(ctx, commentID, body); err != nil {
		return nil, err
	}
	return st, nil
}

func (m *Manager) UpdatePhase(ctx context.Context, issueNum int, owner, phase, status string) (*IssueState, error) {
	st, commentID, err := m.findLatestState(ctx, issueNum, owner)
	if err != nil {
		return nil, err
	}
	if phase != "" {
		st.Lease.Phase = phase
	}
	if status != "" {
		st.Lease.Status = status
	}
	body, err := FormatStateComment(st)
	if err != nil {
		return nil, err
	}
	if _, err := m.tracker.UpdateComment(ctx, commentID, body); err != nil {
		return nil, err
	}
	return st, nil
}

// GetCurrentState inspects issue comments and returns the latest active (or most recent) IssueState,
// also recognizing `/swarm approve-plan`, `/swarm reject-plan`, and `/swarm release` comments from humans.
func (m *Manager) GetCurrentState(ctx context.Context, issueNum int) (*IssueState, error) {
	comments, err := m.tracker.ListComments(ctx, issueNum)
	if err != nil {
		return nil, err
	}
	now := m.nowFn()
	var best *IssueState
	var humanReview *contract.Artifact
	var releasedViaComment bool

	for _, c := range comments {
		if st, ok := ParseStateComment(c.Body); ok {
			st.Lease.CommentID = c.ID
			if best == nil {
				best = st
				continue
			}
			bestActive := best.Lease.Status == "active" && best.Lease.ExpiresAt.After(now)
			curActive := st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now)
			if curActive && !bestActive {
				best = st
			} else if !curActive && !bestActive {
				best = st
			}
			continue
		}

		trimmed := strings.TrimSpace(c.Body)
		switch {
		case strings.HasPrefix(trimmed, "/swarm approve-plan"):
			humanReview = &contract.Artifact{
				Version:     1,
				ID:          fmt.Sprintf("art-%d-review", issueNum),
				IssueNumber: issueNum,
				Type:        contract.ArtifactReview,
				Producer:    "human:" + c.Author,
				Verdict:     "APPROVED",
				Summary:     "Approved via /swarm approve-plan comment",
				Body:        trimmed,
				CreatedAt:   c.CreatedAt,
			}
		case strings.HasPrefix(trimmed, "/swarm reject-plan"):
			humanReview = &contract.Artifact{
				Version:     1,
				ID:          fmt.Sprintf("art-%d-review", issueNum),
				IssueNumber: issueNum,
				Type:        contract.ArtifactReview,
				Producer:    "human:" + c.Author,
				Verdict:     "CHANGES_REQUESTED",
				Summary:     "Rejected via /swarm reject-plan comment",
				Body:        trimmed,
				CreatedAt:   c.CreatedAt,
			}
		case strings.HasPrefix(trimmed, "/swarm release"):
			releasedViaComment = true
		}
	}

	if best != nil {
		if releasedViaComment && best.Lease.Status == "active" {
			best.Lease.Status = "released"
		}
		if humanReview != nil {
			replaced := false
			for idx, a := range best.Artifacts {
				if a.Type == contract.ArtifactReview {
					best.Artifacts[idx] = *humanReview
					replaced = true
					break
				}
			}
			if !replaced {
				best.Artifacts = append(best.Artifacts, *humanReview)
			}
		}
	}

	if best == nil {
		return nil, ErrNoActiveLease
	}
	return best, nil
}

func (m *Manager) findOwnerState(ctx context.Context, issueNum int, owner string) (*IssueState, int64, error) {
	comments, err := m.tracker.ListComments(ctx, issueNum)
	if err != nil {
		return nil, 0, err
	}
	var found *IssueState
	var foundID int64
	for _, c := range comments {
		st, ok := ParseStateComment(c.Body)
		if !ok {
			continue
		}
		if owner == "" || st.Lease.Owner == owner {
			st.Lease.CommentID = c.ID
			found = st
			foundID = c.ID
		}
	}
	if found == nil {
		return nil, 0, fmt.Errorf("%w for issue #%d (owner=%s)", ErrNoActiveLease, issueNum, owner)
	}
	return found, foundID, nil
}

func (m *Manager) findLatestState(ctx context.Context, issueNum int, owner string) (*IssueState, int64, error) {
	st, cid, err := m.findOwnerState(ctx, issueNum, owner)
	if err == nil {
		return st, cid, nil
	}
	return m.findOwnerState(ctx, issueNum, "")
}

func ParseStateComment(body string) (*IssueState, bool) {
	matches := stateMarkerRegex.FindStringSubmatch(body)
	if len(matches) < 2 {
		return nil, false
	}
	var st IssueState
	if err := json.Unmarshal([]byte(matches[1]), &st); err != nil {
		return nil, false
	}
	return &st, true
}

func FormatStateComment(st *IssueState) (string, error) {
	rawJSON, err := json.Marshal(st)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<!-- swarm:state:v1 %s -->\n", string(rawJSON)))
	sb.WriteString("### SwarmOS Orchestration State\n\n")
	sb.WriteString("| Field | Value |\n| :--- | :--- |\n")
	sb.WriteString(fmt.Sprintf("| **Issue** | `#%d` |\n", st.Lease.IssueNumber))
	sb.WriteString(fmt.Sprintf("| **Owner** | `%s` |\n", st.Lease.Owner))
	if len(st.Lease.Contributors) > 1 {
		sb.WriteString(fmt.Sprintf("| **Contributors** | `%s` |\n", strings.Join(st.Lease.Contributors, ", ")))
	}
	if st.Lease.NodeID != "" {
		sb.WriteString(fmt.Sprintf("| **Node ID** | `%s` |\n", st.Lease.NodeID))
	}
	if st.Lease.Agent != "" {
		sb.WriteString(fmt.Sprintf("| **Agent** | `%s` |\n", st.Lease.Agent))
	}
	sb.WriteString(fmt.Sprintf("| **Phase** | `%s` |\n", st.Lease.Phase))
	if st.Lease.NextPhase != "" && st.Lease.Status == "handoff" {
		sb.WriteString(fmt.Sprintf("| **Next Phase (Handoff)** | `%s` |\n", st.Lease.NextPhase))
	}
	sb.WriteString(fmt.Sprintf("| **Status** | `%s` |\n", st.Lease.Status))
	sb.WriteString(fmt.Sprintf("| **Expires At** | `%s` |\n", st.Lease.ExpiresAt.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("| **Renewals** | `%d / %d` |\n\n", st.Lease.Renewals, st.Lease.MaxRenewals))

	if len(st.Artifacts) > 0 {
		sb.WriteString("#### Published Artifacts\n\n")
		sb.WriteString("| Type | Producer | Verdict | Summary | Updated |\n| :--- | :--- | :--- | :--- | :--- |\n")
		for _, a := range st.Artifacts {
			verdict := a.Verdict
			if verdict == "" {
				verdict = "-"
			}
			sb.WriteString(fmt.Sprintf("| `%s` (v%d) | `%s` | `%s` | %s | `%s` |\n",
				a.Type, a.Version, a.Producer, verdict, a.Summary, a.CreatedAt.Format(time.RFC3339)))
		}
		sb.WriteString("\n")

		for _, a := range st.Artifacts {
			sb.WriteString(fmt.Sprintf("<details>\n<summary>Artifact: <strong>%s</strong> (%s)</summary>\n\n%s\n\n</details>\n\n",
				a.Type, a.Summary, strings.TrimSpace(a.Body)))
		}
	}

	return sb.String(), nil
}
