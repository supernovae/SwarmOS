package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
)

func (f *upstreamRemoteFixture) maintainerOpts() RootOptions {
	return RootOptions{RepoRoot: f.upstreamRoot, Backend: "github", trackerOverride: f.maintainer}
}

// sameRepoClaim claims issue 42 as login directly through the lease manager,
// as a same-repository collaborator's worker would.
func (f *upstreamRemoteFixture) sameRepoClaim(login string) (*lease.Manager, *lease.IssueState) {
	f.t.Helper()
	cfg, err := harness.LoadConfig(f.upstreamRoot)
	if err != nil {
		f.t.Fatal(err)
	}
	m := lease.NewManager(f.githubTracker("octo/swarm", login, "User"), cfg.Security)
	m.SetNowFunc(func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now })
	if err := m.ConfigureStore(f.upstreamRoot); err != nil {
		f.t.Fatal(err)
	}
	st, err := m.Acquire(context.Background(), 42, login, "mock", "plan", time.Hour, 1)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.issues[42].Labels = append(f.issues[42].Labels, "swarm:claimed", "swarm:in-progress")
	f.mu.Unlock()
	return m, st
}

func (f *upstreamRemoteFixture) revokeComments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments {
		if strings.HasPrefix(c.Body, "<!-- swarm:revoke:v1 -->") {
			out = append(out, c.Body)
		}
	}
	return out
}

func TestCLIRevokeSameRepositoryClaim(t *testing.T) {
	f := newUpstreamRemoteFixture(t)
	holder, claimed := f.sameRepoClaim("bob")
	out, err := f.run(f.maintainerOpts(), "issue", "revoke", "42", "--reason", "Abandoned since March", "--json")
	if err != nil {
		t.Fatalf("revoke: %v\n%s", err, out)
	}
	var result struct {
		Kind       string                `json:"kind"`
		Revocation lease.ClaimRevocation `json:"revocation"`
		Warning    string                `json:"projection_warning"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&result); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if result.Kind != "revoke" || result.Revocation.RevokedBy != "maintainer" || result.Revocation.Owner != "bob" || result.Revocation.ClaimID != claimed.Lease.ClaimID || result.Warning != "" {
		t.Fatalf("result = %+v", result)
	}
	st := f.state()
	if st.Lease.Status != "revoked" || len(st.ClaimRevocations) != 1 || st.ClaimRevocations[0].RevokedBy != "maintainer" || st.ClaimRevocations[0].Reason != "Abandoned since March" {
		t.Fatalf("authoritative state = %+v", st)
	}
	if notices := f.revokeComments(); len(notices) != 1 || !strings.Contains(notices[0], "@maintainer") || !strings.Contains(notices[0], "Abandoned since March") {
		t.Fatalf("audit comments = %q", notices)
	}
	f.mu.Lock()
	labels := append([]string(nil), f.issues[42].Labels...)
	f.mu.Unlock()
	for _, l := range labels {
		if l == "swarm:claimed" || l == "swarm:in-progress" {
			t.Fatalf("label %s kept: %v", l, labels)
		}
	}
	if _, err := holder.Renew(context.Background(), 42, "bob", time.Hour); !errors.Is(err, lease.ErrClaimFence) {
		t.Fatalf("stale heartbeat = %v", err)
	}
	if out, err := f.run(f.maintainerOpts(), "issue", "revoke", "42", "--reason", "again"); err == nil || !errors.Is(err, lease.ErrNoActiveLease) {
		t.Fatalf("second revoke = %v\n%s", err, out)
	}
	// The issue is claimable again.
	if _, next := f.sameRepoClaim("carol"); next.Lease.ClaimID == claimed.Lease.ClaimID {
		t.Fatal("successor reused revoked claim")
	}
}

func TestCLIRevokeForkClaimClearsContributorIndex(t *testing.T) {
	f := newUpstreamRemoteFixture(t)
	alice := f.aliceClone("alice")
	if out, err := f.run(f.as("alice", alice), "--upstream-remote", "upstream", "--fork", "issue", "claim", "42", "--agent", "mock"); err != nil {
		t.Fatalf("fork claim: %v\n%s", err, out)
	}
	claimed := f.state()
	index := func() string { return f.git(f.projectBare, "show", "refs/heads/swarm-contributors/alice:index.json") }
	if claimed.ForkRepo != "alice/swarm" || !strings.Contains(index(), claimed.Lease.ClaimID) {
		t.Fatalf("fork claim fixture: %+v %s", claimed.Lease, index())
	}
	if out, err := f.run(f.maintainerOpts(), "issue", "revoke", "42", "--reason", "Fork contributor unresponsive"); err != nil {
		t.Fatalf("revoke: %v\n%s", err, out)
	}
	st := f.state()
	if st.Lease.Status != "revoked" || st.ForkRepo != "alice/swarm" || st.LastForkRequestID != claimed.LastForkRequestID {
		t.Fatalf("state after revoke = %+v", st)
	}
	if got := index(); strings.Contains(got, claimed.Lease.ClaimID) {
		t.Fatalf("contributor index still counts revoked claim: %s", got)
	}
}

func TestCLIRevokeRefusals(t *testing.T) {
	f := newUpstreamRemoteFixture(t)
	f.sameRepoClaim("bob")
	alice := f.aliceClone("alice")
	bob := RootOptions{RepoRoot: f.upstreamRoot, Backend: "github", trackerOverride: f.githubTracker("octo/swarm", "bob", "User")}
	for _, tc := range []struct {
		name string
		opts RootOptions
		args []string
		want string
	}{
		{"without approve-task permission", bob, []string{"issue", "revoke", "42", "--reason", "mine"}, "permission"},
		{"fork contributor mode", f.as("alice", alice), []string{"--upstream-remote", "upstream", "--fork", "issue", "revoke", "42", "--reason", "via fork"}, "--fork contributor mode"},
		{"missing reason", f.maintainerOpts(), []string{"issue", "revoke", "42"}, "--reason is required"},
		{"blank reason", f.maintainerOpts(), []string{"issue", "revoke", "42", "--reason", "  "}, "--reason is required"},
		{"invalid issue", f.maintainerOpts(), []string{"issue", "revoke", "abc", "--reason", "typo"}, "invalid issue number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, comments := f.refs(), f.postedComments()
			out, err := f.run(tc.opts, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("revoke = %v, want %q\n%s", err, tc.want, out)
			}
			if f.refs() != before || f.postedComments() != comments {
				t.Fatal("refused revoke wrote refs or posted comments")
			}
			if st := f.state(); st.Lease.Status != "active" || st.Lease.Owner != "bob" {
				t.Fatalf("refused revoke changed claim: %+v", st.Lease)
			}
		})
	}
}

func TestCLIRevokeRefusesLocalBackend(t *testing.T) {
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	if out, err := f.run("issue", "revoke", "1", "--reason", "offline"); err == nil || !strings.Contains(err.Error(), "github backend") {
		t.Fatalf("local revoke = %v\n%s", err, out)
	}
	if _, err := f.manager.GetCurrentState(context.Background(), issue); err == nil {
		t.Fatal("local revoke created state")
	}
}
