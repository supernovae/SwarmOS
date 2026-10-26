package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workflow"
)

// The approved policy is the scaffold default lease.duration of 4h.
const approvedTestLease = 4 * time.Hour

func leaseLength(t *testing.T, st *lease.IssueState, from time.Time) time.Duration {
	t.Helper()
	if st == nil || st.Lease.Status != "active" {
		t.Fatalf("no active lease: %+v", st)
	}
	return st.Lease.ExpiresAt.Sub(from)
}

func TestCLIClaimAndRenewDurationCappedAtApprovedPolicy(t *testing.T) {
	for _, tc := range []struct {
		requested string
		want      time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"4h", approvedTestLease},
		{"100h", approvedTestLease},
	} {
		t.Run(tc.requested, func(t *testing.T) {
			f := newCLIFixture(t)
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			raw := f.must(t, "issue", "claim", n, "--agent", "mock", "--duration", tc.requested, "--json")
			var claimed struct {
				State *lease.IssueState `json:"state"`
			}
			if err := json.Unmarshal([]byte(raw), &claimed); err != nil {
				t.Fatal(err)
			}
			if got := leaseLength(t, claimed.State, claimed.State.Lease.ClaimedAt); got != tc.want {
				t.Fatalf("claim --duration %s granted %s, want %s", tc.requested, got, tc.want)
			}
			before := time.Now()
			raw = f.must(t, "issue", "renew", n, "--duration", tc.requested, "--json")
			after := time.Now()
			var renewed struct {
				State *lease.IssueState `json:"state"`
			}
			if err := json.Unmarshal([]byte(raw), &renewed); err != nil {
				t.Fatal(err)
			}
			stored := f.state(t, issue)
			if stored.Lease.Renewals != 1 || !stored.Lease.ExpiresAt.Equal(renewed.State.Lease.ExpiresAt) {
				t.Fatalf("renewal not authoritative: %+v", stored.Lease)
			}
			if lo, hi := leaseLength(t, stored, after), leaseLength(t, stored, before); lo > tc.want || hi < tc.want {
				t.Fatalf("renew --duration %s granted %s..%s, want %s", tc.requested, lo, hi, tc.want)
			}
		})
	}
}

// node run selects work with no duration override; a longer checkout policy
// cannot raise the lease above the approved base.
func TestCLINodeSelectionDurationUsesApprovedPolicy(t *testing.T) {
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	cfgPath := filepath.Join(f.root, ".swarm", "config.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	longer := strings.Replace(string(cfg), "duration: 4h", "duration: 999h", 1)
	if longer == string(cfg) {
		t.Fatal("fixture policy has no 4h lease")
	}
	if err := os.WriteFile(cfgPath, []byte(longer), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, override := range map[string]time.Duration{"node default": 0, "oversized override": 100 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			pulled, err := pullNextReadyIssue(context.Background(), cmd, &RootOptions{RepoRoot: f.root, Backend: "local"}, false, workflow.WorkOptions{AgentName: "mock", Phase: "all"}, override)
			if err != nil {
				t.Fatal(err)
			}
			if got := leaseLength(t, pulled.State, pulled.State.Lease.ClaimedAt); got != approvedTestLease {
				t.Fatalf("selection granted %s, want %s", got, approvedTestLease)
			}
			stored := f.state(t, issue)
			if !stored.Lease.ExpiresAt.Equal(pulled.State.Lease.ExpiresAt) {
				t.Fatalf("authoritative lease = %+v", stored.Lease)
			}
			f.manager.BindClaim(issue, stored.Lease.ClaimID)
			if _, err := f.manager.Release(context.Background(), issue, stored.Lease.Owner, "next case"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
