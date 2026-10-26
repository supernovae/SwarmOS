package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/harness"
)

func TestTrustedLedgerNamespaceIgnoresLocalAndForkConfig(t *testing.T) {
	for _, tc := range []struct{ upstream, other string }{{"branch", "hidden"}, {"hidden", "branch"}} {
		t.Run(tc.upstream, func(t *testing.T) {
			tmp := t.TempDir()
			git := func(dir string, args ...string) string {
				t.Helper()
				out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			setNamespace := func(dir, ns string) {
				t.Helper()
				if _, err := os.Stat(filepath.Join(dir, ".swarm", "config.yaml")); err != nil {
					if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: dir}); err != nil {
						t.Fatal(err)
					}
				}
				path := filepath.Join(dir, ".swarm", "config.yaml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var lines []string
				for _, line := range strings.Split(string(data), "\n") {
					if !strings.HasPrefix(line, "ledger_namespace:") {
						lines = append(lines, line)
					}
				}
				lines = append(lines, "ledger_namespace: "+ns, "")
				if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			project := filepath.Join(tmp, "project.git")
			git(tmp, "init", "--bare", "--initial-branch=main", project)
			maintainer := filepath.Join(tmp, "maintainer")
			git(tmp, "clone", project, maintainer)
			git(maintainer, "config", "user.name", "Maintainer")
			git(maintainer, "config", "user.email", "maintainer@example.invalid")
			setNamespace(maintainer, tc.upstream)
			git(maintainer, "add", ".")
			git(maintainer, "commit", "-m", "trusted config")
			git(maintainer, "push", "origin", "main")
			fork := filepath.Join(tmp, "fork.git")
			git(tmp, "clone", "--bare", project, fork)

			worker := filepath.Join(tmp, "worker")
			git(tmp, "clone", fork, worker)
			git(worker, "config", "user.name", "Worker")
			git(worker, "config", "user.email", "worker@example.invalid")
			git(worker, "remote", "add", "upstream", project)
			// The fork's default branch, a local commit, a detached HEAD, and an
			// uncommitted edit all select the other namespace.
			setNamespace(worker, tc.other)
			git(worker, "commit", "-am", "candidate selects the other namespace")
			git(worker, "push", "origin", "HEAD:main")
			git(worker, "checkout", "--detach")
			setNamespace(worker, tc.other)

			expect := func(remote, want string) {
				t.Helper()
				got, err := TrustedLedgerNamespace(worker, remote)
				if err != nil || got != want {
					t.Fatalf("TrustedLedgerNamespace(%s) = %q, %v; want %q", remote, got, err, want)
				}
			}
			expect("upstream", tc.upstream)
			// origin is the contributor fork here; it is only trusted when named.
			expect("origin", tc.other)

			// Updating the trusted default branch takes effect on the next read.
			setNamespace(maintainer, tc.other)
			git(maintainer, "commit", "-am", "maintainer switches namespace")
			git(maintainer, "push", "origin", "main")
			expect("upstream", tc.other)

			for name, remote := range map[string]string{"missing named remote": "absent", "missing origin": ""} {
				if name == "missing origin" {
					git(worker, "remote", "remove", "origin")
				}
				if got, err := TrustedLedgerNamespace(worker, remote); err == nil {
					t.Fatalf("%s selected %q instead of failing", name, got)
				}
			}
			if err := os.WriteFile(filepath.Join(maintainer, ".swarm", "config.yaml"), []byte("version: ["), 0o600); err != nil {
				t.Fatal(err)
			}
			git(maintainer, "commit", "-am", "malformed")
			git(maintainer, "push", "origin", "main")
			if got, err := TrustedLedgerNamespace(worker, "upstream"); err == nil {
				t.Fatalf("malformed trusted config selected %q", got)
			}
			git(worker, "remote", "set-url", "upstream", filepath.Join(tmp, "missing.git"))
			if got, err := TrustedLedgerNamespace(worker, "upstream"); err == nil {
				t.Fatalf("unfetchable upstream selected %q", got)
			}
		})
	}
}
