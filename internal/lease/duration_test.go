package lease

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Same-repository claims and renewals are capped at the approved policy
// duration (4h in testManager), matching fork mode. A longer checkout policy
// cannot raise the cap.
func TestSameRepoClaimAndRenewDurationCappedAtApprovedPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested time.Duration
		want      time.Duration
	}{
		{"shorter", time.Hour, time.Hour},
		{"equal", 4 * time.Hour, 4 * time.Hour},
		{"longer", 10 * time.Hour, 4 * time.Hour},
		{"much longer", 1000 * time.Hour, 4 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			m, _, iss := testManager(t)
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			m.SetNowFunc(func() time.Time { return now })
			if err := os.WriteFile(filepath.Join(m.repoRoot, ".swarm/config.yaml"), []byte("lease:\n  duration: 999h\n  renewable: true\n  max_renewals: 3\n"), 0600); err != nil {
				t.Fatal(err)
			}
			st, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", tc.requested, 3)
			if err != nil {
				t.Fatal(err)
			}
			if got := st.Lease.ExpiresAt.Sub(now); got != tc.want {
				t.Fatalf("acquire expiry = %s, want %s", got, tc.want)
			}
			stored, err := m.GetCurrentState(ctx, iss.Number)
			if err != nil || !stored.Lease.ExpiresAt.Equal(now.Add(tc.want)) {
				t.Fatalf("authoritative acquire expiry = %+v: %v", stored.Lease, err)
			}
			now = now.Add(30 * time.Minute)
			st, err = m.Renew(ctx, iss.Number, "local-dev", tc.requested)
			if err != nil {
				t.Fatal(err)
			}
			if got := st.Lease.ExpiresAt.Sub(now); got != tc.want {
				t.Fatalf("renew expiry = %s, want %s", got, tc.want)
			}
			stored, err = m.GetCurrentState(ctx, iss.Number)
			if err != nil || !stored.Lease.ExpiresAt.Equal(now.Add(tc.want)) || stored.Lease.Renewals != 1 {
				t.Fatalf("authoritative renew lease = %+v: %v", stored.Lease, err)
			}
		})
	}
}

func TestSameRepoRenewDurationCappedAfterShortClaim(t *testing.T) {
	ctx := t.Context()
	m, _, iss := testManager(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetNowFunc(func() time.Time { return now })
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	st, err := m.Renew(ctx, iss.Number, "local-dev", 10*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Lease.ExpiresAt.Sub(now); got != 4*time.Hour {
		t.Fatalf("renew expiry = %s, want 4h", got)
	}
}

func TestDurationRejectsNonPositiveRequests(t *testing.T) {
	ctx := t.Context()
	m, _, iss := testManager(t)
	for _, d := range []time.Duration{0, -time.Hour} {
		if _, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", d, 1); err == nil {
			t.Fatalf("acquire accepted duration %s", d)
		}
	}
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{0, -time.Hour} {
		if _, err := m.Renew(ctx, iss.Number, "local-dev", d); err == nil {
			t.Fatalf("renew accepted duration %s", d)
		}
	}
}

func TestApprovedLeaseDuration(t *testing.T) {
	for _, tc := range []struct {
		policy    string
		requested time.Duration
		want      time.Duration
		wantErr   bool
	}{
		{"4h", time.Hour, time.Hour, false},
		{"4h", 4 * time.Hour, 4 * time.Hour, false},
		{"4h", 5 * time.Hour, 4 * time.Hour, false},
		{"4h", 0, 4 * time.Hour, false},
		{"bad", time.Hour, 0, true},
		{"0s", time.Hour, 0, true},
		{"-1h", time.Hour, 0, true},
	} {
		got, err := approvedLeaseDuration(tc.policy, tc.requested)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Fatalf("approvedLeaseDuration(%q, %s) = %s, %v", tc.policy, tc.requested, got, err)
		}
	}
}
