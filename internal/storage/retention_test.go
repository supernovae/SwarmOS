package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/tracker"
)

func TestInventoryLocalAuthorityAndMissingRegistrations(t *testing.T) {
	opts := fixture(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	opts.Repository = ""
	opts.SelectedRoot = repo
	opts.Backend = "local"
	state := filepath.Join(repo, ".swarm", "coordination", "issue-7.json")
	put(t, state, `{"lease":{"version":1,"issue_number":7,"status":"active","expires_at":"2000-01-01T00:00:00Z","claim_id":"must-never-appear"},"branch":"swarm/issue-7-test","pr_number":11}`)
	trackerPath := filepath.Join(repo, ".swarm", "local-tracker", "state.json")
	put(t, trackerPath, `{"pull_requests":{"11":{"number":11,"state":"closed","merged":true}}}`)
	calls := 0
	opts.Tracker = func(owner, root, backend string) (tracker.PullRequestVisitor, error) {
		calls++
		return tracker.NewLocalTracker(root), nil
	}
	beforeState, _ := os.Stat(state)
	beforeTracker, _ := os.Stat(trackerPath)
	snap, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("traversals=%d", calls)
	}
	found := map[string]bool{}
	for _, r := range snap.Resources {
		if r.Class == "issue-worktree" || r.Class == "pr-worktree" || r.Class == "code-branch" {
			found[r.Class] = true
			if r.Eligibility != "blocked" || r.Active == nil || !*r.Active {
				t.Fatalf("lost active state: %+v", r)
			}
			if strings.Contains(fmt.Sprint(r), "must-never-appear") {
				t.Fatal("claim token leaked")
			}
		}
	}
	for _, class := range []string{"issue-worktree", "pr-worktree", "code-branch"} {
		if !found[class] {
			t.Fatal("missing", class)
		}
	}
	afterState, _ := os.Stat(state)
	afterTracker, _ := os.Stat(trackerPath)
	if !beforeState.ModTime().Equal(afterState.ModTime()) || !beforeTracker.ModTime().Equal(afterTracker.ModTime()) {
		t.Fatal("state modified")
	}
	for _, dir := range []string{filepath.Dir(state), filepath.Dir(trackerPath)} {
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatal("created files", entries)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".swarm", "worktrees")); !os.IsNotExist(err) {
		t.Fatal("created missing worktree", err)
	}
}
func TestInventoryCachesDoNotTriggerPRTraversal(t *testing.T) {
	opts := fixture(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	opts.Repository = ""
	opts.SelectedRoot = repo
	opts.Backend = "github"
	put(t, filepath.Join(opts.CacheRoot, "agent-build", "orphan", "go", "data"), "cache")
	probe := &cacheOnlyRemoteProbe{}
	opts.Tracker = func(string, string, string) (tracker.PullRequestVisitor, error) { return probe, nil }
	if _, err := Scan(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if probe.prCalls != 0 || probe.stateCalls != 1 {
		t.Fatalf("cache initiated PR calls: %+v", probe)
	}
}

func TestInventoryIndexedMissingOrphanRemainsUnknown(t *testing.T) {
	opts := fixture(t)
	opts.UseRoot = filepath.Join(t.TempDir(), "control")
	m, err := storageuse.Open(opts.UseRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	path := filepath.Join(opts.CacheRoot, "agent-build", "orphan", "go")
	resource := storageuse.Resource{Repository: "gone/repository", Class: "agent-build", Identity: "cache:" + path, Location: path}
	use, err := m.AcquireUse(context.Background(), resource)
	if err != nil {
		t.Fatal(err)
	}
	if err = use.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Resources) != 1 {
		t.Fatalf("registration lost: %+v", out)
	}
	r := out.Resources[0]
	if r.Path != path || r.Repository != "" || r.Eligibility != "unknown" || r.Active != nil || r.LogicalBytes != nil {
		t.Fatal(r)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("created missing cache")
	}
}

type inventoryRemoteFixture struct {
	sha                 string
	stateCalls, prCalls int
}

func (f *inventoryRemoteFixture) VisitStateRefs(ctx context.Context, b tracker.InventoryBudget, visit func(int, string) error) error {
	f.stateCalls++
	return visit(9, f.sha)
}
func (f *inventoryRemoteFixture) VisitPullRequests(ctx context.Context, b tracker.InventoryBudget, visit func(*tracker.PullRequest) error) error {
	f.prCalls++
	return visit(&tracker.PullRequest{Number: 17, State: "open"})
}
func TestInventoryRemotePinnedStateWithoutFetch(t *testing.T) {
	opts := fixture(t)
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	put(t, filepath.Join(root, "state.json"), `{"lease":{"version":1,"issue_number":9,"status":"active","claim_id":"private-claim"},"pr_number":17,"branch":"swarm/issue-9-test"}`)
	git("add", "state.json")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "state")
	sha := git("rev-parse", "HEAD")
	put(t, filepath.Join(root, ".swarm", "worktrees", "issue-9", "data"), "unregistered")
	opts.Repository = ""
	opts.SelectedRoot = root
	opts.Backend = "github"
	remote := &inventoryRemoteFixture{sha: sha}
	opts.Tracker = func(string, string, string) (tracker.PullRequestVisitor, error) { return remote, nil }
	before := git("show-ref")
	index, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	out, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if remote.prCalls != 1 || remote.stateCalls != 1 {
		t.Fatal(remote)
	}
	found := false
	for _, r := range out.Resources {
		if r.Class == "pr-worktree" {
			found = true
			if r.Eligibility != "blocked" || r.Active == nil || !*r.Active {
				t.Fatal(r)
			}
		}
		if strings.Contains(fmt.Sprint(r), "private-claim") {
			t.Fatal("claim leaked")
		}
	}
	if !found {
		t.Fatal("missing remote-linked registration")
	}
	if git("show-ref") != before {
		t.Fatal("changed refs")
	}
	after, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	if string(index) != string(after) {
		t.Fatal("changed index")
	}
}

func TestInventoryCleanMergedWorktreeAndDirtyRetention(t *testing.T) {
	opts := fixture(t)
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	put(t, filepath.Join(root, "file"), "committed\n")
	git("add", "file")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "base")
	sha := git("rev-parse", "HEAD")
	branch := "swarm/issue-1-fixture"
	git("branch", branch)
	path := filepath.Join(root, ".swarm", "worktrees", "issue-1")
	git("worktree", "add", "-q", path, branch)
	put(t, filepath.Join(root, ".swarm", "coordination", "issue-1.json"), fmt.Sprintf(`{"lease":{"version":1,"issue_number":1,"status":"completed"},"branch":%q,"submitted_head_sha":%q,"pr_number":3}`, branch, sha))
	put(t, filepath.Join(root, ".swarm", "local-tracker", "state.json"), fmt.Sprintf(`{"pull_requests":{"3":{"number":3,"state":"closed","head":%q,"base":"main","head_sha":%q,"merged":true,"merged_at":"2026-01-01T00:00:00Z","merge_commit_sha":%q}}}`, branch, sha, sha))
	opts.SelectedRoot = root
	opts.Repository = ""
	opts.Backend = "local"
	opts.Tracker = func(string, string, string) (tracker.PullRequestVisitor, error) {
		return tracker.NewLocalTracker(root), nil
	}
	find := func() Resource {
		t.Helper()
		out, err := Scan(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range out.Resources {
			if r.Path == path {
				return r
			}
		}
		t.Fatal("missing worktree")
		return Resource{}
	}
	if r := find(); r.Eligibility != "eligible" || r.Active == nil || *r.Active {
		t.Fatalf("clean worktree: %+v", r)
	}
	put(t, opts.RegistryPath, fmt.Sprintf("version: 1\nrepos:\n  local/registered:\n    repo: local/registered\n    path: %s\n    backend: local\n", root))
	opts.Backend = ""
	opts.SelectedRoot = path
	if r := find(); r.Eligibility != "eligible" || r.Repository != "local/registered" {
		t.Fatalf("linked-checkout selection lost main inventory: %+v", r)
	}
	opts.SelectedRoot = root
	put(t, filepath.Join(path, "file"), "changed\n")
	if r := find(); r.Eligibility != "blocked" || !strings.Contains(r.Reason, "differs") {
		t.Fatalf("dirty worktree: %+v", r)
	}
}

// Provider construction and repository-ledger discovery are distinct from a
// PR traversal. Count actual visitor operations rather than construction.
type cacheOnlyRemoteProbe struct{ stateCalls, prCalls int }

func (p *cacheOnlyRemoteProbe) VisitStateRefs(context.Context, tracker.InventoryBudget, func(int, string) error) error {
	p.stateCalls++
	return nil
}
func (p *cacheOnlyRemoteProbe) VisitPullRequests(context.Context, tracker.InventoryBudget, func(*tracker.PullRequest) error) error {
	p.prCalls++
	return nil
}
