package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/supernovae/swarmos/internal/workspace"
	"strings"
)

// CleanupReservation never expires into reuse. Interrupted operations retain
// their token; explicit CAS recovery fences the old operation before reuse.
type CleanupReservation struct {
	OperationID   string    `json:"operation_id"`
	Operator      string    `json:"operator"`
	ClaimID       string    `json:"claim_id"`
	StateRevision string    `json:"state_revision"`
	Evidence      PROutcome `json:"evidence"`
	Branch        string    `json:"branch"`
	ExpectedHead  string    `json:"expected_head"`
	RemoteDeleted bool      `json:"remote_deleted"`
}

func (m *Manager) BeginCleanup(ctx context.Context, n int, observed *PROutcome) (*CleanupReservation, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("cleanup requires a repository operator")
	}
	if err := m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return nil, err
	}
	actor, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	if actor == "" {
		return nil, fmt.Errorf("cleanup operator identity missing")
	}
	st, rev, err := m.loadReadOnly(ctx, n)
	if err != nil {
		return nil, err
	}
	if st.Lease.Status == "active" || st.PRRevision != nil {
		return nil, fmt.Errorf("cleanup cannot reserve an active attempt or revision")
	}
	o, err := m.observeSubmission(ctx, st)
	if err != nil {
		return nil, err
	}
	if observed == nil || o.Status != "merged" || o.MergeCommitSHA == "" || o.MergedAt == nil || o.MergedAt.IsZero() || o.SubmittedHeadSHA == "" || o.FinalHeadSHA != o.SubmittedHeadSHA || observed.FinalHeadSHA != o.FinalHeadSHA || observed.MergeCommitSHA != o.MergeCommitSHA || observed.Number != o.Number || observed.Branch != o.Branch || st.ForkRepo != "" {
		return nil, fmt.Errorf("cleanup merge evidence changed or incomplete")
	}
	if reason := m.cleanupPolicy(ctx, st, o, false); reason != "" {
		return nil, fmt.Errorf("cleanup refused: %s", reason)
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return nil, err
	}
	r := &CleanupReservation{OperationID: hex.EncodeToString(token), Operator: actor, ClaimID: st.Lease.ClaimID, StateRevision: rev, Evidence: *o, Branch: st.Branch, ExpectedHead: o.SubmittedHeadSHA}
	st.PROutcome = o
	st.Cleanup = r
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	_, err = m.store.CompareAndSwap(ctx, n, rev, raw)
	return r, err
}

func (m *Manager) reservedCleanup(ctx context.Context, n int, token string) (*IssueState, string, error) {
	raw, rev, err := m.store.Load(ctx, n)
	if err != nil {
		return nil, "", err
	}
	var st IssueState
	if err = json.Unmarshal(raw, &st); err != nil {
		return nil, "", err
	}
	if (st.Lease.IssueNumber != 0 && st.Lease.IssueNumber != n) || st.Cleanup == nil || token == "" || st.Cleanup.OperationID != token || st.Cleanup.ClaimID != st.Lease.ClaimID {
		return nil, "", fmt.Errorf("%w: stale cleanup operation", ErrClaimFence)
	}
	return &st, rev, nil
}

// DeleteCleanupBranch performs only the atomic remote transaction. Local
// resources are handled separately by RetireCleanupLocalResources.
func (m *Manager) DeleteCleanupBranch(ctx context.Context, n int, token string) error {
	st, rev, err := m.reservedCleanup(ctx, n, token)
	if err != nil {
		return err
	}
	if m.gateway != nil {
		return fmt.Errorf("cleanup requires a repository operator")
	}
	if err = m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return err
	}
	o, err := m.observeSubmission(ctx, st)
	if err != nil {
		return err
	}
	r := st.Cleanup
	if reason := m.cleanupPolicy(ctx, st, o, true); reason != "" {
		return fmt.Errorf("cleanup refused: %s", reason)
	}
	if m.cleanupLocal() {
		return fmt.Errorf("remote cleanup not applicable to local backend")
	}
	if err = m.verifyCleanupRemote(ctx, o); err != nil {
		return err
	}
	if r.Branch == o.TargetBranch || o.TargetBranch == "" {
		return fmt.Errorf("target branch cannot be retired")
	}
	if o.Status != "merged" || o.FinalHeadSHA != r.ExpectedHead || o.MergeCommitSHA != r.Evidence.MergeCommitSHA || o.MergedAt == nil {
		return fmt.Errorf("merge evidence changed during cleanup")
	}
	out, err := cleanupGitOutput(ctx, m.repoRoot, "ls-remote", "--refs", "origin", "refs/heads/"+r.Branch)
	if err != nil {
		return err
	}
	fields := strings.Fields(out)
	absent := len(fields) == 0
	if !absent && (len(fields) != 2 || fields[0] != r.ExpectedHead || fields[1] != "refs/heads/"+r.Branch) {
		return fmt.Errorf("remote cleanup branch head changed")
	}
	ds, ok := m.store.(interface {
		CompareAndSwapAndDeleteBranch(context.Context, int, string, []byte, string, string) (string, error)
	})
	if !ok {
		return fmt.Errorf("cleanup requires atomic Git transport")
	}
	st.Cleanup.RemoteDeleted = true
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if absent {
		_, err = m.store.CompareAndSwap(ctx, n, rev, raw)
	} else {
		_, err = ds.CompareAndSwapAndDeleteBranch(ctx, n, rev, raw, r.Branch, r.ExpectedHead)
	}
	return err
}

func (m *Manager) FinishCleanup(ctx context.Context, n int, token string) error {
	unlock, err := m.cleanupLock()
	if err != nil {
		return err
	}
	defer unlock()
	if m.gateway != nil {
		return fmt.Errorf("cleanup requires an operator")
	}
	if err := m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return err
	}
	st, rev, err := m.reservedCleanup(ctx, n, token)
	if err != nil {
		return err
	}
	st.CleanupHistory = append(st.CleanupHistory, *st.Cleanup)
	st.Cleanup = nil
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = m.store.CompareAndSwap(ctx, n, rev, raw)
	return err
}

func (m *Manager) verifyCleanupRemote(ctx context.Context, o *PROutcome) error {
	fetchURL, err := cleanupGitOutput(ctx, m.repoRoot, "remote", "get-url", "--all", "origin")
	if err != nil {
		return err
	}
	pushURL, err := cleanupGitOutput(ctx, m.repoRoot, "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return err
	}
	if strings.TrimSpace(fetchURL) != strings.TrimSpace(pushURL) || len(strings.Split(strings.TrimSpace(fetchURL), "\n")) != 1 {
		return fmt.Errorf("cleanup requires one identical fetch and push destination")
	}
	upstream, err := workspace.ResolveRemoteGitHubSlug(m.repoRoot, "origin")
	if err != nil || upstream == "" || !strings.EqualFold(upstream, o.BaseRepo) || !strings.EqualFold(upstream, o.HeadRepo) {
		return fmt.Errorf("cleanup origin repository identity mismatch")
	}
	out, err := cleanupGitOutput(ctx, m.repoRoot, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return err
	}
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "ref:" || fields[2] != "HEAD" {
		return fmt.Errorf("default branch identity unavailable")
	}
	if fields[1] == "refs/heads/"+o.Branch {
		return fmt.Errorf("default branch cannot be retired")
	}
	return nil
}

// RecoverCleanup serializes with local actions and releases an interrupted
// reservation with a CAS. The state transition also fences an old atomic push.
func (m *Manager) RecoverCleanup(ctx context.Context, n int) error {
	unlock, err := m.cleanupLock()
	if err != nil {
		return err
	}
	defer unlock()
	if m.gateway != nil {
		return fmt.Errorf("cleanup recovery requires a repository operator")
	}
	if err := m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return err
	}
	st, rev, err := m.loadReadOnly(ctx, n)
	if err != nil {
		return err
	}
	if st.Cleanup == nil {
		return nil
	}
	o, err := m.observeSubmission(ctx, st)
	if err != nil {
		return err
	}
	if o.Status != "merged" || o.FinalHeadSHA != st.Cleanup.ExpectedHead {
		return fmt.Errorf("cleanup recovery requires unchanged merge evidence")
	}
	st.CleanupHistory = append(st.CleanupHistory, *st.Cleanup)
	st.Cleanup = nil
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = m.store.CompareAndSwap(ctx, n, rev, raw)
	return err
}
