package lease

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
)

// Cleanup on a named upstream keeps its fetch/push and identity checks there,
// and never consults or changes a decoy origin.
func TestCleanupUsesNamedUpstreamRemote(t *testing.T) {
	for _, kind := range []string{"valid", "upstream-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			m, tr, n, remote := mergedCleanupFixture(t)
			ctx := context.Background()
			git := func(args ...string) string {
				t.Helper()
				out, e := exec.Command("git", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
				if e != nil {
					t.Fatalf("git %v: %s %v", args, out, e)
				}
				return strings.TrimSpace(string(out))
			}
			git("remote", "rename", "origin", "upstream")
			decoy := filepath.Join(t.TempDir(), "decoy.git")
			git("init", "--bare", decoy)
			git("remote", "add", "origin", decoy)
			git("config", "remote.origin.swarmSlug", "alice/swarm")
			m.SetUpstreamRemote("upstream")
			m.SetStore(m.repoRoot, coordination.NewGitStoreWithRemote(m.repoRoot, "upstream"))
			if kind == "upstream-mismatch" {
				git("config", "remote.upstream.swarmSlug", "other/repo")
			}
			m.tracker = &cleanupIdentityTracker{lifecycleTracker: tr, upstream: "octo/swarm"}
			_, observed, err := m.InspectSubmission(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.BeginCleanup(ctx, n, observed)
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if out, _ := exec.Command("git", "--git-dir="+remote, "rev-parse", fmt.Sprintf("refs/heads/swarm-state/issue-%d", n)).CombinedOutput(); len(strings.TrimSpace(string(out))) != 40 {
					t.Fatalf("state not on upstream: %s", out)
				}
			} else if err == nil || !strings.Contains(err.Error(), "cleanup upstream repository identity mismatch") {
				t.Fatalf("mismatch = %v", err)
			}
			if out, _ := exec.Command("git", "--git-dir="+decoy, "for-each-ref").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
				t.Fatalf("decoy origin changed: %s", out)
			}
		})
	}
}

func TestManagerUpstreamRemoteDefault(t *testing.T) {
	m := NewManager(nil, contract.SecurityConfig{})
	if m.UpstreamRemote() != "origin" {
		t.Fatal(m.UpstreamRemote())
	}
	m.SetUpstreamRemote(" upstream ")
	if m.UpstreamRemote() != "upstream" {
		t.Fatal(m.UpstreamRemote())
	}
}
