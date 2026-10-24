package lease

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/supernovae/swarmos/internal/workspace"
)

// RebaseEvidence is durable review context, never an authorization input.
type RebaseEvidence struct {
	ConflictPathsReported bool     `json:"conflict_paths_reported,omitempty"`
	OldHead               string   `json:"old_head"`
	NewHead               string   `json:"new_head,omitempty"`
	OldBase               string   `json:"old_base"`
	NewBase               string   `json:"new_base"`
	ConflictedPaths       []string `json:"conflicted_paths,omitempty"`
	Status                string   `json:"status"`
	PatchIdentity         string   `json:"patch_identity"`
	Verified              bool     `json:"verified"`
}

// RebaseContext reports local progress. Fork reports cannot assert Git identity.
type RebaseContext struct {
	ConflictedPaths []string `json:"conflicted_paths,omitempty"`
	Status          string   `json:"status"`
}

func startRebaseEvidence(st *IssueState, head string) {
	base := st.SubmittedBaseSHA
	if base == "" {
		base = st.PreviousBaseSHA
	}
	if base == "" {
		base = st.BaseSHA
	}
	if st.LastRebase != nil && st.LastRebase.NewHead == head && st.LastRebase.Status == "completed" {
		base = st.LastRebase.NewBase
	}
	st.LastRebase = &RebaseEvidence{OldHead: head, OldBase: base, NewBase: st.BaseSHA, Status: "in-progress", PatchIdentity: "unavailable"}
}

func verifyRebaseEvidence(ctx context.Context, root string, st *IssueState, head string) {
	r := st.LastRebase
	if r == nil || r.Status != "completed" {
		return
	}
	r.NewHead = head
	r.PatchIdentity = "unavailable"
	r.Verified = false
	if identity, err := workspace.RebasePatchIdentity(ctx, root, r.OldBase, r.OldHead, r.NewBase, r.NewHead); err == nil {
		r.PatchIdentity = identity
		r.Verified = true
	}
}

// UpdateRebaseContext uses the same fenced operation as evidence invalidation.
func (m *Manager) UpdateRebaseContext(ctx context.Context, n int, owner string, number int, report RebaseContext) (*IssueState, error) {
	if m.gateway != nil {
		return m.gateway.Apply(ctx, ForkRequest{Version: 1, Operation: "invalidate-pr-evidence", IssueNumber: n, ClaimID: m.token(n), PRNumber: number, RevisionIntent: "rebase", RebaseContext: &report})
	}
	return m.mutate(ctx, n, owner, func(st *IssueState) error {
		if st.PRRevision == nil || st.PRRevision.Number != number || st.LastRebase == nil {
			return fmt.Errorf("no pending rebase for PR #%d", number)
		}
		if report.Status != "in-progress" && report.Status != "completed" && report.Status != "aborted" {
			return fmt.Errorf("invalid rebase status")
		}
		r := st.LastRebase
		for _, p := range report.ConflictedPaths {
			if p == "" || strings.ContainsRune(p, 0) || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
				return fmt.Errorf("invalid conflict path")
			}
			r.ConflictedPaths = append(r.ConflictedPaths, p)
		}
		slices.Sort(r.ConflictedPaths)
		r.ConflictedPaths = slices.Compact(r.ConflictedPaths)
		if st.ForkRepo != "" {
			r.ConflictPathsReported = true
		}
		r.Status = report.Status
		r.PatchIdentity = "unavailable"
		r.Verified = false
		if r.Status == "completed" {
			r.NewHead = st.PRRevision.LocalHeadSHA
			if st.ForkRepo == "" {
				verifyRebaseEvidence(ctx, m.repoRoot, st, r.NewHead)
			}
		}
		return nil
	})
}
