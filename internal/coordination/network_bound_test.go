package coordination

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/testutil/gittransport"
)

// Each row exercises a real publication path before its conflict/retry mapping.
func TestNetworkAllCoordinationPushSites(t *testing.T) {
	for _, mode := range []string{"hang", "land-hang", "holder", "holder-reject"} {
		for _, site := range []string{"CAS", "checkpoint", "PR", "fork update", "fork acquire", "retain", "revoke upstream", "revoke fork", "delete", "migrate"} {
			t.Run(mode+"/"+site, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				f := newRevokeFixture(t, "origin")
				s := NewGitStore(f.clone)
				head := f.git(f.clone, "rev-parse", "HEAD")
				branch := "swarm/issue-1-network"
				f.git(f.clone, "push", "origin", head+":refs/heads/"+branch)
				body := f.state(1, "c1", "alice", "active", "")
				if site == "migrate" {
					body = f.state(1, "c1", "alice", "completed", "")
				}
				if site == "fork acquire" {
					body = []byte(`{"lease":{"issue_number":1,"status":"draft"}}`)
				}
				rev, err := s.CompareAndSwap(ctx, 1, "", body)
				if err != nil {
					t.Fatal(err)
				}
				if site == "revoke fork" {
					// Acquire through the indexed store, then preserve the recorded claim id.
					rev, err = f.forkClaim(s, "alice", 1, 4, 101)
					if err != nil {
						t.Fatal(err)
					}
					body, _, err = s.Load(ctx, 1)
					if err != nil {
						t.Fatal(err)
					}
				}
				f.git(f.clone, "commit", "--allow-empty", "-m", "candidate")
				candidate := f.git(f.clone, "rev-parse", "HEAD")
				tr := gittransport.New(t, f.remote)
				tr.Configure(t, f.clone, "origin")
				tr.Set("receive", mode)
				bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				start := time.Now()
				tr.Watch(15*time.Second, func() {
					switch site {
					case "CAS":
						_, err = s.CompareAndSwap(bounded, 1, rev, body)
					case "checkpoint":
						_, err = s.CompareAndSwapAndPublishExpected(bounded, 1, rev, body, branch, candidate, head)
					case "PR":
						_, err = s.CompareAndSwapAndPublishPR(bounded, 1, rev, body, branch, head, candidate)
					case "fork update", "fork acquire":
						indexed := NewForkIndexedStore(f.clone, "alice", 4, 102)
						indexed.SetNowFunc(func() time.Time { return f.now })
						next := body
						if site == "fork acquire" {
							next = f.state(1, "c1", "alice", "active", "alice/fork")
						}
						_, err = indexed.CompareAndSwap(bounded, 1, rev, next)
					case "retain":
						_, err = s.CompareAndSwapRetaining(bounded, 1, rev, body, candidate)
					case "revoke upstream", "revoke fork":
						_, err = s.CompareAndSwapAndRevokeClaim(bounded, 1, rev, "c1", revokedFrom(t, body))
					case "delete":
						_, err = s.CompareAndSwapAndDeleteBranch(bounded, 1, rev, body, branch, head)
					case "migrate":
						_, err = s.MigrateLedgerRefs(bounded, f.now, false)
					}
				})
				if time.Since(start) > 15*time.Second {
					t.Error("publication exceeded bound")
				}
				if mode == "holder" || mode == "holder-reject" {
					var known *gitcredential.KnownResultCleanupError
					if !errors.As(err, &known) || (mode == "holder" && known.ExitCode != 0 || mode == "holder-reject" && known.ExitCode == 0) || errors.Is(err, ErrPublicationUnknown) || errors.Is(err, ErrConflict) {
						t.Errorf("known landed result lost: %v", err)
					}
				} else if !errors.Is(err, ErrPublicationUnknown) || !errors.Is(err, gitcredential.ErrInterrupted) || errors.Is(err, ErrConflict) {
					t.Errorf("unknown result lost: %v", err)
				}
				if err != nil && (strings.Contains(err.Error(), "no refs changed") || (mode != "holder-reject" && strings.Contains(err.Error(), "rejected"))) {
					t.Errorf("misleading result: %v", err)
				}
				if mode == "holder-reject" {
					var exit *exec.ExitError
					if !errors.As(err, &exit) || !errors.Is(err, gitcredential.ErrCleanupIncomplete) {
						t.Errorf("known failure cleanup lost: %v", err)
					}
				}
				if tr.Receives() != 1 {
					t.Errorf("receive attempts: %d", tr.Receives())
				}
				if mode == "land-hang" {
					if !tr.Landed() {
						t.Error("expected landed transaction")
					} else {
						updates, e := os.ReadFile(filepath.Join(tr.Dir, "updates"))
						if e != nil {
							t.Error(e)
						}
						for _, line := range strings.Split(strings.TrimSpace(string(updates)), "\n") {
							fields := strings.Fields(line)
							if len(fields) != 3 {
								t.Errorf("invalid hook update: %q", line)
								continue
							}
							got, e := gitfixture.Command(t, f.remote, "rev-parse", "--verify", fields[2]).Output()
							if fields[1] == strings.Repeat("0", 40) {
								if e == nil {
									t.Errorf("deleted ref survived: %s", fields[2])
								}
							} else if e != nil || strings.TrimSpace(string(got)) != fields[1] {
								t.Errorf("remote did not land %s: %s %v", line, got, e)
							}
						}
					}
				}
				tr.Release()
				tr.Join()
			})
		}
	}
}
func TestInterruptedCASRetryReinspects(t *testing.T) {
	for _, scenario := range []string{"landed", "not landed", "successor"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			f := newRetentionFixture(t)
			ctx := context.Background()
			s := f.store
			old, err := s.CompareAndSwap(ctx, 1, "", []byte(`{"v":0}`))
			if err != nil {
				t.Fatal(err)
			}
			tr := gittransport.New(t, f.remote)
			tr.Configure(t, f.clone, "origin")
			mode := "hang"
			if scenario == "landed" {
				mode = "land-hang"
			}
			tr.Set("receive", mode)
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			tr.Watch(15*time.Second, func() { _, err = s.CompareAndSwap(bounded, 1, old, []byte(`{"v":1}`)) })
			if !errors.Is(err, ErrPublicationUnknown) {
				t.Fatalf("interrupted CAS: %v", err)
			}
			tr.Release()
			tr.Join()
			tr.Set("receive", "pass")
			if scenario == "successor" {
				other := newRetentionFixture(t)
				other.git(other.clone, "remote", "set-url", "origin", f.remote)
				remoteStore := NewGitStore(other.clone)
				_, loaded, e := remoteStore.Load(ctx, 1)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = remoteStore.CompareAndSwap(ctx, 1, loaded, []byte(`{"v":2}`)); e != nil {
					t.Fatal(e)
				}
			}
			data, observed, err := s.Load(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.CompareAndSwap(ctx, 1, old, []byte(`{"v":3}`))
			if scenario == "not landed" {
				if observed != old || err != nil {
					t.Errorf("normal retry: %s %v", observed, err)
				}
			} else {
				if observed == old || !errors.Is(err, ErrConflict) {
					t.Errorf("stale retry: %s %s %v", data, observed, err)
				}
				_, after, e := s.Load(ctx, 1)
				if e != nil || after != observed {
					t.Errorf("successor overwritten: %s %v", after, e)
				}
			}
		})
	}
}
func TestNetworkRejectsInput(t *testing.T) {
	f := newRetentionFixture(t)
	_, err := f.store.run(context.Background(), "not allowed", "fetch", "origin")
	if err == nil {
		t.Fatal("network input accepted")
	}
}
