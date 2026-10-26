package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/coordination"
)

// ClaimRevocation is authoritative evidence that a maintainer ended a claim.
// Issue comments only project it.
type ClaimRevocation struct {
	ClaimID   string    `json:"claim_id"`
	Owner     string    `json:"owner"`
	ForkRepo  string    `json:"fork_repo,omitempty"`
	RevokedBy string    `json:"revoked_by"`
	Reason    string    `json:"reason"`
	RevokedAt time.Time `json:"revoked_at"`
}

const revokeCommentMarker = "<!-- swarm:revoke:v1 -->"

// Revoke ends the issue's active claim on behalf of an authenticated
// maintainer. It changes only the lease status, expiry, and revocation history:
// approval, artifacts, checkpoints, and PR links stay for the next claimer.
// The stale holder's claim ID no longer matches an active lease, so its next
// fenced write fails with ErrClaimFence.
//
// A nil err means the revoke is committed. projectionErr reports an incomplete
// issue comment or label projection, which never undoes the revoke.
func (m *Manager) Revoke(ctx context.Context, n int, reason string) (st *IssueState, projectionErr, err error) {
	if m.gateway != nil {
		return nil, nil, fmt.Errorf("revoke requires a repository maintainer and is not available in --fork contributor mode")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, nil, fmt.Errorf("revoke reason is required")
	}
	if m.tracker == nil || !strings.HasPrefix(m.tracker.BackendName(), "github") {
		return nil, nil, fmt.Errorf("revoke requires the github backend")
	}
	store, ok := m.store.(coordination.ClaimRevokingStore)
	if !ok {
		return nil, nil, fmt.Errorf("revoke requires the authoritative git coordination store")
	}
	if err = m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return nil, nil, err
	}
	actor, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(actor) == "" {
		return nil, nil, fmt.Errorf("revoking maintainer identity missing")
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, nil, err
	}
	now := m.nowFn()
	if st.Lease.Status != "active" || !st.Lease.ExpiresAt.After(now) || st.Lease.ClaimID == "" {
		return nil, nil, fmt.Errorf("%w: issue #%d", ErrNoActiveLease, n)
	}
	revoked := ClaimRevocation{ClaimID: st.Lease.ClaimID, Owner: st.Lease.Owner, ForkRepo: st.ForkRepo, RevokedBy: actor, Reason: reason, RevokedAt: now}
	st.ClaimRevocations = append(st.ClaimRevocations, revoked)
	st.Lease.Status = "revoked"
	st.Lease.ExpiresAt = now
	m.stampForSave(ctx, n, st)
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, nil, err
	}
	// A conflict means the captured claim changed; never retry onto a successor.
	if _, err = store.CompareAndSwapAndRevokeClaim(ctx, n, rev, revoked.ClaimID, raw); err != nil {
		if errors.Is(err, coordination.ErrConflict) {
			return nil, nil, fmt.Errorf("issue #%d state changed while revoking claim %s; inspect it and retry: %w", n, short(revoked.ClaimID), err)
		}
		return nil, nil, err
	}
	return st, m.projectRevocation(ctx, n, st, revoked), nil
}

func (m *Manager) projectRevocation(ctx context.Context, n int, st *IssueState, r ClaimRevocation) error {
	m.project(ctx, n, st)
	var errs []error
	if _, err := m.tracker.CreateComment(ctx, n, RenderRevocationComment(r)); err != nil {
		errs = append(errs, fmt.Errorf("post revoke audit comment: %w", err))
	}
	for _, label := range []string{"swarm:claimed", "swarm:in-progress"} {
		if err := m.tracker.RemoveLabel(ctx, n, label); err != nil {
			errs = append(errs, fmt.Errorf("remove label %s: %w", label, err))
		}
	}
	return errors.Join(errs...)
}

// RenderRevocationComment renders the audit notice for a revoked claim. The
// reason is quoted and escaped so it cannot inject markers or markup.
func RenderRevocationComment(r ClaimRevocation) string {
	var sb strings.Builder
	sb.WriteString(revokeCommentMarker + "\n")
	sb.WriteString("### 🐝 SwarmOS claim revoked\n\n")
	fmt.Fprintf(&sb, "Maintainer @%s revoked claim `%s` held by @%s at %s.\n\n", escapeMarkdown(r.RevokedBy), short(r.ClaimID), escapeMarkdown(r.Owner), r.RevokedAt.UTC().Format(time.RFC3339))
	sb.WriteString("**Reason:**\n\n")
	sb.WriteString(quoteMarkdown(r.Reason))
	sb.WriteString("\n\nCheckpoints, artifacts, and approval are retained; the issue can be claimed again. This comment is a projection of authoritative state.\n")
	return sb.String()
}

func escapeMarkdown(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "`", "&#96;", "|", "&#124;").Replace(s)
}

func quoteMarkdown(s string) string {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = "> " + escapeMarkdown(line)
	}
	return strings.Join(lines, "\n")
}
