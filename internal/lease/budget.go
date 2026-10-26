package lease

import (
	"fmt"
	"math"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

// ClaimBudget is a coordinator projection of the approved lease policy.
// Remaining is the nominal allowance; Available also accounts for final expiry.
type ClaimBudget struct {
	Active            bool          `json:"active"`
	RenewalsUsed      int           `json:"renewals_used"`
	RenewalsRemaining int           `json:"renewals_remaining"`
	Deadline          time.Time     `json:"deadline"`
	Remaining         time.Duration `json:"remaining"`
	Available         time.Duration `json:"available"`
	Threshold         time.Duration `json:"threshold"`

	expiryOnly bool
}

// LeaseExpiryOnly reports a WorkBudget timed from the current lease expiry
// because the upstream coordinator supplied no projection. Its renewal
// counts are placeholders, not coordinator facts.
func (b *ClaimBudget) LeaseExpiryOnly() bool { return b != nil && b.expiryOnly }

func policyBudget(cfg contract.LeaseConfig) (time.Duration, time.Duration, error) {
	d, err := time.ParseDuration(cfg.Duration)
	if err != nil || d <= 0 || cfg.MaxRenewals < 0 || uint64(cfg.MaxRenewals) >= uint64(math.MaxInt64)/uint64(d) {
		return 0, 0, fmt.Errorf("invalid or overflowing approved lease budget")
	}
	return d, d * (time.Duration(cfg.MaxRenewals) + 1), nil
}

// Budget loads policy from the approved base and uses the manager clock.
// In fork mode it returns nil when the upstream coordinator predates claim
// budgets and supplied no projection.
func (m *Manager) Budget(st *IssueState) (*ClaimBudget, error) {
	if st == nil || st.Lease.ClaimedAt.IsZero() {
		return nil, nil
	}
	if m.gateway != nil {
		if st.ClaimBudget == nil {
			return nil, nil
		}
		b := *st.ClaimBudget
		b.Active = st.Lease.Status == "active" && st.Lease.ExpiresAt.After(m.nowFn())
		b.Remaining = max(time.Duration(0), b.Deadline.Sub(m.nowFn()))
		b.Available = b.Remaining
		if b.RenewalsRemaining == 0 {
			b.Available = min(b.Available, max(time.Duration(0), st.Lease.ExpiresAt.Sub(m.nowFn())))
		}
		if !b.Active {
			b.Available = 0
		}
		return &b, nil
	}
	cfg, err := harness.LoadConfigAt(m.repoRoot, st.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("load approved budget policy: %w", err)
	}
	return projectBudget(st.Lease, cfg.Lease, m.nowFn())
}

// WorkBudget is Budget for wind-down timing. When the upstream coordinator
// supplied no projection, it times wind-down from the current lease expiry
// with a reserve of a third of duration. The fallback grants nothing: the
// coordinator still fences every renewal and claim check.
func (m *Manager) WorkBudget(st *IssueState, duration time.Duration) (*ClaimBudget, error) {
	b, err := m.Budget(st)
	if err != nil || b != nil || m.gateway == nil || st == nil || st.Lease.ClaimedAt.IsZero() {
		return b, err
	}
	if duration <= 0 {
		return nil, fmt.Errorf("lease duration is required to time wind-down without an upstream claim budget")
	}
	now := m.nowFn()
	b = &ClaimBudget{Active: st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now), RenewalsUsed: st.Lease.Renewals, Deadline: st.Lease.ExpiresAt, Threshold: duration / 3, expiryOnly: true}
	b.Remaining = max(time.Duration(0), st.Lease.ExpiresAt.Sub(now))
	b.Available = b.Remaining
	if !b.Active {
		b.Available = 0
	}
	return b, nil
}

func projectBudget(l contract.Lease, cfg contract.LeaseConfig, now time.Time) (*ClaimBudget, error) {
	d, total, err := policyBudget(cfg)
	if err != nil {
		return nil, err
	}
	limit := min(l.MaxRenewals, cfg.MaxRenewals)
	if !cfg.Renewable {
		limit = 0
	}
	b := &ClaimBudget{Active: l.Status == "active" && l.ExpiresAt.After(now), RenewalsUsed: l.Renewals, RenewalsRemaining: max(0, limit-l.Renewals), Deadline: l.ClaimedAt.Add(total), Threshold: d / 3}
	b.Remaining = max(time.Duration(0), b.Deadline.Sub(now))
	b.Available = b.Remaining
	if b.RenewalsRemaining == 0 {
		b.Available = min(b.Available, max(time.Duration(0), l.ExpiresAt.Sub(now)))
	}
	if !b.Active {
		b.Available = 0
	}
	return b, nil
}
