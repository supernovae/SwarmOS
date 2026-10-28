package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
)

type ledgerCLIFixture struct {
	t                  *testing.T
	tmp, project, root string
}

// newLedgerCLIFixture publishes a scaffolded config selecting ns on the
// project's default branch and returns a maintainer clone of it.
func newLedgerCLIFixture(t *testing.T, ns string) *ledgerCLIFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SWARM_REGISTRY_PATH", filepath.Join(t.TempDir(), "repos.yaml"))
	t.Setenv("SWARM_NODE_CONFIG", filepath.Join(t.TempDir(), "node.yaml"))
	f := &ledgerCLIFixture{t: t, tmp: t.TempDir()}
	f.project = filepath.Join(f.tmp, "project.git")
	f.root = filepath.Join(f.tmp, "maintainer")
	f.git(f.tmp, "init", "--bare", "--initial-branch=main", f.project)
	f.git(f.tmp, "clone", f.project, f.root)
	f.git(f.root, "config", "user.name", "Maintainer")
	f.git(f.root, "config", "user.email", "maintainer@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: f.root}); err != nil {
		t.Fatal(err)
	}
	f.setNamespace(ns)
	f.git(f.root, "add", ".")
	f.git(f.root, "commit", "-m", "trusted config")
	f.git(f.root, "push", "origin", "main")
	return f
}

func (f *ledgerCLIFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := gitfixture.Command(f.t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// setNamespace edits the working-tree config only.
func (f *ledgerCLIFixture) setNamespace(ns string) {
	f.t.Helper()
	path := filepath.Join(f.root, ".swarm", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	next := regexp.MustCompile(`(?m)^ledger_namespace: .*$`).ReplaceAllString(string(data), "ledger_namespace: "+ns)
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ledgerCLIFixture) run(opts RootOptions, args ...string) (string, error) {
	f.t.Helper()
	opts.nowFn = func() time.Time { return time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC) }
	cmd := newRootCmdWithOptions(&opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// managerLayout resolves the lease manager as every github command does.
func (f *ledgerCLIFixture) managerLayout() (coordination.RefLayout, error) {
	f.t.Helper()
	opts := &RootOptions{RepoRoot: f.root, Backend: "github", trackerOverride: checkpointCLITracker{tracker.NewLocalTracker(f.root)}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	_, mgr, _, err := opts.resolveLeaseManager(cmd)
	if err != nil {
		return coordination.RefLayout{}, err
	}
	return mgr.LedgerLayout(), nil
}

func TestLeaseManagerNamespaceFollowsOnlyUpstreamDefaultBranch(t *testing.T) {
	for _, tc := range []struct{ upstream, local string }{{"branch", "hidden"}, {"hidden", "branch"}} {
		t.Run(tc.upstream, func(t *testing.T) {
			f := newLedgerCLIFixture(t, tc.upstream)
			// An unpushed commit plus an uncommitted edit select the other mode.
			f.setNamespace(tc.local)
			f.git(f.root, "commit", "-am", "candidate namespace")
			f.git(f.root, "commit", "--allow-empty", "-m", "detached candidate")
			f.git(f.root, "checkout", "--detach")
			layout, err := f.managerLayout()
			if err != nil || layout.Name() != tc.upstream {
				t.Fatalf("manager layout = %s, %v; want %s", layout.Name(), err, tc.upstream)
			}
			f.git(f.root, "push", "origin", "HEAD:main")
			layout, err = f.managerLayout()
			if err != nil || layout.Name() != tc.local {
				t.Fatalf("after default-branch update, layout = %s, %v; want %s", layout.Name(), err, tc.local)
			}
			f.git(f.root, "remote", "remove", "origin")
			if layout, err := f.managerLayout(); err == nil {
				t.Fatalf("missing upstream selected %s instead of failing", layout.Name())
			}
		})
	}
}

func TestRepoMigrateRefsCutover(t *testing.T) {
	ctx := context.Background()
	f := newLedgerCLIFixture(t, "branch")
	branchStore := coordination.NewGitStore(f.root)
	if _, err := branchStore.CompareAndSwap(ctx, 5, "", []byte(`{"lease":{"issue_number":5,"status":"released"}}`)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		opts RootOptions
		want string
	}{
		{"fork", RootOptions{RepoRoot: f.root, Fork: true}, "cannot run with --fork"},
		{"local backend", RootOptions{RepoRoot: f.root, Backend: "local"}, "local backend"},
	} {
		if out, err := f.run(tc.opts, "repo", "migrate-refs"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v\n%s", tc.name, err, out)
		}
	}
	out, err := f.run(RootOptions{RepoRoot: f.root}, "repo", "migrate-refs", "--dry-run")
	if err != nil || !strings.Contains(out, "WOULD MOVE refs/heads/swarm-state/issue-5 -> refs/swarm/state/issue-5") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if f.git(f.project, "for-each-ref", "refs/swarm") != "" {
		t.Fatal("dry run pushed")
	}
	out, err = f.run(RootOptions{RepoRoot: f.root}, "repo", "migrate-refs", "--json")
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	var result struct {
		Status string                       `json:"status"`
		Report coordination.MigrationReport `json:"report"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.Status != "ok" || len(result.Report.Moves) != 1 {
		t.Fatalf("migrate JSON: %v\n%s", err, out)
	}
	// Between migration and the config merge, branch-mode commands refuse.
	if _, err := f.managerLayout(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := branchStore.Load(ctx, 5); !errors.Is(err, coordination.ErrLedgerNamespace) {
		t.Fatalf("branch load during cutover: %v", err)
	}
	out, err = f.run(RootOptions{RepoRoot: f.root}, "repo", "migrate-refs")
	if err != nil || !strings.Contains(out, "Nothing to migrate; 1 ledger ref(s) already hidden.") {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	f.setNamespace("hidden")
	f.git(f.root, "commit", "-am", "select hidden ledger refs")
	f.git(f.root, "push", "origin", "main")
	layout, err := f.managerLayout()
	if err != nil || layout != coordination.HiddenLayout {
		t.Fatalf("after cutover layout = %s, %v", layout.Name(), err)
	}
	if body, _, err := coordination.NewGitStoreWithLayout(f.root, "", layout).Load(ctx, 5); err != nil || !strings.Contains(string(body), "released") {
		t.Fatalf("hidden load after cutover: %s, %v", body, err)
	}
}
