package lease

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/workspace"
)

// RebaseEvidence is durable review context, never an authorization input.
type RebaseEvidence struct {
	RepairScopeSet        bool     `json:"repair_scope_set,omitempty"`
	RepairPaths           []string `json:"repair_paths,omitempty"`
	RepairAttempts        int      `json:"repair_attempts,omitempty"`
	ConflictPathsReported bool     `json:"conflict_paths_reported,omitempty"`
	OldHead               string   `json:"old_head"`
	NewHead               string   `json:"new_head,omitempty"`
	OldBase               string   `json:"old_base"`
	NewBase               string   `json:"new_base"`
	ConflictedPaths       []string `json:"conflicted_paths,omitempty"`
	Status                string   `json:"status"`
	PatchIdentity         string   `json:"patch_identity"`
	Verified              bool     `json:"verified"`
	// DroppedPaths lists paths the PR changed against OldBase but no longer
	// changes against NewBase. Absent DroppedPathsVerified means unknown.
	DroppedPaths         []string `json:"dropped_paths,omitempty"`
	DroppedPathsVerified bool     `json:"dropped_paths_verified,omitempty"`
	// Resolutions record agent conflict resolutions as review evidence only.
	Plans       []RebasePlanReport `json:"plans,omitempty"`
	Resolutions []RebaseResolution `json:"resolutions,omitempty"`
}

// RebasePlanReport is bounded, non-authoritative evidence for one conflict stop.
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
	RepairPaths     *[]string               `json:"repair_paths,omitempty"`
	RepairAttempt   *int                    `json:"repair_attempt,omitempty"`
	ConflictedPaths []string                `json:"conflicted_paths,omitempty"`
	Status          string                  `json:"status"`
	Plan            *RebasePlanReport       `json:"plan,omitempty"`
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
	r.VerifyDroppedPaths(ctx, root)
}

// VerifyDroppedPaths recomputes dropped paths from Git objects in root.
func (r *RebaseEvidence) VerifyDroppedPaths(ctx context.Context, root string) {
	r.resetDroppedPaths()
	if dropped, err := workspace.RebaseDroppedPaths(ctx, root, r.OldBase, r.OldHead, r.NewBase, r.NewHead); err == nil {
		r.DroppedPaths = dropped
		r.DroppedPathsVerified = true
	}
}

func (r *RebaseEvidence) resetDroppedPaths() {
	r.DroppedPaths = nil
	r.DroppedPathsVerified = false
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
		if report.RepairPaths != nil {
			paths := append([]string(nil), (*report.RepairPaths)...)
			if len(paths) > 4096 {
				return fmt.Errorf("too many repair paths")
			}
			for _, p := range paths {
				if !validRebasePath(p) {
					return fmt.Errorf("invalid repair path")
				}
			}
			slices.Sort(paths)
			paths = slices.Compact(paths)
			if r.RepairScopeSet && !slices.Equal(r.RepairPaths, paths) {
				return fmt.Errorf("repair scope is already fixed")
			}
			r.RepairPaths = paths
			r.RepairScopeSet = true
		}
		if report.RepairAttempt != nil {
			cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
			if err != nil {
				return err
			}
			if *report.RepairAttempt != r.RepairAttempts+1 || *report.RepairAttempt > cfg.Lease.RebaseRepairAttempts() {
				return fmt.Errorf("repair budget exhausted or attempt changed")
			}
			r.RepairAttempts = *report.RepairAttempt
		}
		for _, p := range report.ConflictedPaths {
			if !validRebasePath(p) {
				return fmt.Errorf("invalid conflict path")
			}
			r.ConflictedPaths = append(r.ConflictedPaths, p)
		}
		if report.Plan != nil {
			plan := *report.Plan
			if len(r.Plans) >= maxRebaseResolutions || len(plan.Body) == 0 || len(plan.Body) > 64<<10 || hash(plan.Body) != plan.Digest || plan.BaseSHA != st.BaseSHA {
				return fmt.Errorf("invalid rebase plan evidence")
			}
			if err := validateRebaseResolution(&RebaseResolutionReport{Commit: plan.Commit, Paths: plan.Paths, Agent: plan.Agent, Harness: plan.Harness, HarnessVersion: plan.HarnessVersion, Model: plan.Model, Sandbox: plan.Sandbox}); err != nil {
				return err
			}
			approved := ""
			for _, a := range st.Artifacts {
				if a.Type == contract.ArtifactDesign {
					approved = hash(a.Body)
				}
			}
			if approved == "" || approved != plan.PlanDigest {
				return fmt.Errorf("rebase plan does not match approved design")
			}
			plan.Paths = append([]string(nil), plan.Paths...)
			r.Plans = append(r.Plans, plan)
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
		r.resetDroppedPaths()
		if r.Status == "completed" {
			r.NewHead = st.PRRevision.LocalHeadSHA
			if st.ForkRepo == "" {
				verifyRebaseEvidence(ctx, m.repoRoot, st, r.NewHead)
			}
		}
		return nil
	})
}
