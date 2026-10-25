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
	// Resolutions record agent conflict resolutions as review evidence only.
	Resolutions []RebaseResolution `json:"resolutions,omitempty"`
}

// RebaseResolutionReport is the wire form of one agent conflict resolution.
// It has no authority fields: the coordinator decides how it is labeled.
type RebaseResolutionReport struct {
	Commit         string   `json:"commit"`
	Paths          []string `json:"paths"`
	Agent          string   `json:"agent"`
	Harness        string   `json:"harness,omitempty"`
	HarnessVersion string   `json:"harness_version,omitempty"`
	Model          string   `json:"model,omitempty"`
	Sandbox        string   `json:"sandbox,omitempty"`
}

// RebaseResolution is a stored resolution. Reported marks fork contributor
// reports, which upstream cannot inspect before publication.
type RebaseResolution struct {
	RebaseResolutionReport
	Reported bool `json:"reported,omitempty"`
}

const (
	maxRebaseResolutions     = 64
	maxRebaseResolutionPaths = 256
	maxRebaseResolutionField = 200
)

// RebaseContext reports local progress. Fork reports cannot assert Git identity.
type RebaseContext struct {
	ConflictedPaths []string                `json:"conflicted_paths,omitempty"`
	Status          string                  `json:"status"`
	Resolution      *RebaseResolutionReport `json:"resolution,omitempty"`
}

func validRebasePath(p string) bool {
	return p != "" && !strings.ContainsRune(p, 0) && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

func validateRebaseResolution(r *RebaseResolutionReport) error {
	if len(r.Commit) != 40 || strings.Trim(r.Commit, "0123456789abcdef") != "" {
		return fmt.Errorf("invalid resolution commit")
	}
	if len(r.Paths) == 0 || len(r.Paths) > maxRebaseResolutionPaths {
		return fmt.Errorf("invalid resolution path count")
	}
	for _, p := range r.Paths {
		if !validRebasePath(p) {
			return fmt.Errorf("invalid resolution path")
		}
	}
	if strings.TrimSpace(r.Agent) == "" {
		return fmt.Errorf("resolution agent is required")
	}
	for _, field := range []string{r.Agent, r.Harness, r.HarnessVersion, r.Model, r.Sandbox} {
		if len(field) > maxRebaseResolutionField || strings.IndexFunc(field, func(c rune) bool { return c < 0x20 || c == 0x7f }) >= 0 {
			return fmt.Errorf("invalid resolution provenance")
		}
	}
	return nil
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
			if !validRebasePath(p) {
				return fmt.Errorf("invalid conflict path")
			}
			r.ConflictedPaths = append(r.ConflictedPaths, p)
		}
		if report.Resolution != nil {
			res := *report.Resolution
			res.Paths = append([]string(nil), res.Paths...)
			slices.Sort(res.Paths)
			res.Paths = slices.Compact(res.Paths)
			if err := validateRebaseResolution(&res); err != nil {
				return err
			}
			if len(r.Resolutions) >= maxRebaseResolutions {
				return fmt.Errorf("too many rebase resolutions")
			}
			// Only the coordinator labels evidence; fork reports stay reports.
			r.Resolutions = append(r.Resolutions, RebaseResolution{RebaseResolutionReport: res, Reported: st.ForkRepo != ""})
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
