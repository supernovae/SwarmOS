package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

type retentionFixture func(context.Context, RepositoryScope, func(RetentionEvidence) error) error

func (f retentionFixture) WithRetention(ctx context.Context, s RepositoryScope, visit func(RetentionEvidence) error) error {
	return f(ctx, s, visit)
}
func completeRetention() RetentionSource {
	return retentionFixture(func(_ context.Context, _ RepositoryScope, visit func(RetentionEvidence) error) error {
		return visit(RetentionEvidence{Complete: true, Stable: true})
	})
}

func reclaimFixture(t *testing.T, class string) (ReclaimOptions, *storageuse.Manager, storageuse.Resource) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	common := filepath.Join(repo, ".git")
	h := sha256.Sum256([]byte(common))
	key := hex.EncodeToString(h[:16])
	o := ReclaimOptions{Scopes: []RepositoryScope{{repo, common, "local"}}, CacheRoot: filepath.Join(root, "cache"), ScratchRoot: filepath.Join(root, "scratch"), ControlRoot: filepath.Join(root, "control"), Retention: completeRetention()}
	for _, p := range []string{o.CacheRoot, o.ScratchRoot} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := storageuse.Open(o.ControlRoot, o.CacheRoot, o.ScratchRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	path := filepath.Join(o.CacheRoot, class, key, "go")
	if strings.HasSuffix(class, "scratch") {
		path = filepath.Join(o.ScratchRoot, "swarm-native-fixture")
	}
	r := storageuse.ResourceFor(key, class, path)
	u, err := m.AcquireUse(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(path, "data"), "reproducible")
	if err := u.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	return o, m, r
}
func TestReclamationPreviewApplyAndRerun(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) {
			o, _, r := reclaimFixture(t, "agent-build")
			o.Scopes[0].Backend = backend
			before, _ := os.Stat(o.ControlRoot)
			records, _ := os.ReadDir(o.ControlRoot)
			preview, err := PreviewReclamation(context.Background(), o)
			if err != nil || len(preview.Resources) != 1 || preview.Resources[0].Outcome != ReclaimEligible {
				t.Fatalf("%+v %v", preview, err)
			}
			after, _ := os.Stat(o.ControlRoot)
			now, _ := os.ReadDir(o.ControlRoot)
			if !before.ModTime().Equal(after.ModTime()) || len(records) != len(now) {
				t.Fatal("preview wrote control storage")
			}
			applied, err := ApplyReclamation(context.Background(), o)
			if err != nil || applied.Resources[0].Outcome != ReclaimRemoved {
				t.Fatalf("%+v %v", applied, err)
			}
			if _, err := os.Stat(r.Location); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if applied.Resources[0].ObservedLogicalDelta == nil || *applied.Resources[0].ObservedLogicalDelta != 12 {
				t.Fatalf("accounting %+v", applied)
			}
			again, err := ApplyReclamation(context.Background(), o)
			var absent *ReclamationResult
			for i := range again.Resources {
				if again.Resources[i].Resource.Location == r.Location {
					absent = &again.Resources[i]
				}
			}
			if err != nil || absent == nil || absent.Outcome != ReclaimAbsent || absent.ObservedAllocatedDelta != nil {
				t.Fatalf("rerun %+v %v", again, err)
			}
			now, _ = os.ReadDir(o.ControlRoot)
			if len(now) != len(records) {
				t.Fatal("removed durable evidence")
			}
		})
	}
}
func TestReclamationRetainsUncertainty(t *testing.T) {
	for _, scenario := range []string{"missing", "error", "incomplete", "unstable", "unfinished", "pending", "changed", "symlink", "unknown-ecosystem", "parent", "outside-scope"} {
		t.Run(scenario, func(t *testing.T) {
			class := "agent-build"
			if scenario == "unfinished" {
				class = "native-scratch"
			}
			o, m, r := reclaimFixture(t, class)
			if _, err := PreviewReclamation(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing":
				o.Retention = nil
			case "error":
				o.Retention = retentionFixture(func(context.Context, RepositoryScope, func(RetentionEvidence) error) error {
					return errors.New("authority unavailable")
				})
			case "incomplete", "unstable", "unfinished":
				o.Retention = retentionFixture(func(_ context.Context, _ RepositoryScope, v func(RetentionEvidence) error) error {
					return v(RetentionEvidence{Complete: scenario != "incomplete", Stable: scenario != "unstable", Unfinished: scenario == "unfinished"})
				})
			case "pending":
				if _, err := m.AcquireUse(context.Background(), r); err != nil {
					t.Fatal(err)
				}
			case "changed":
				o.Retention = retentionFixture(func(_ context.Context, _ RepositoryScope, v func(RetentionEvidence) error) error {
					if err := os.Rename(r.Location, r.Location+"-old"); err != nil {
						t.Fatal(err)
					}
					put(t, filepath.Join(r.Location, "replacement"), "keep")
					return v(RetentionEvidence{Complete: true, Stable: true})
				})
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(r.Location, "link")); err != nil {
					t.Fatal(err)
				}
			case "unknown-ecosystem", "parent":
				newPath := filepath.Join(filepath.Dir(r.Location), "unknown")
				if scenario == "parent" {
					newPath = filepath.Dir(r.Location)
				}
				rr := storageuse.ResourceFor(r.Repository, r.Class, newPath)
				if _, err := m.AcquireUse(context.Background(), rr); err != nil {
					t.Fatal(err)
				}
				// Keep original pending as well so neither registration can be removed.
				if _, err := m.AcquireUse(context.Background(), r); err != nil {
					t.Fatal(err)
				}
			case "outside-scope":
				o.Scopes[0].CommonDirectory = filepath.Join(t.TempDir(), "missing")
			}
			report, _ := ApplyReclamation(context.Background(), o)
			for _, result := range report.Resources {
				if result.Outcome == ReclaimRemoved {
					t.Fatalf("unsafe removal %+v", report)
				}
			}
			if _, err := os.Stat(r.Location); err != nil {
				t.Fatal("resource lost", err)
			}
		})
	}
}
func TestReclamationExplicitScopesAndCancellation(t *testing.T) {
	o, _, r := reclaimFixture(t, "agent-build")
	for _, scopes := range [][]RepositoryScope{nil, {{Root: "relative", CommonDirectory: o.Scopes[0].CommonDirectory, Backend: "local"}}, {o.Scopes[0], {Root: o.Scopes[0].Root, CommonDirectory: o.Scopes[0].CommonDirectory, Backend: "github"}}} {
		bad := o
		bad.Scopes = scopes
		if _, err := ApplyReclamation(context.Background(), bad); err == nil {
			t.Fatal("accepted invalid scope")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report, err := ApplyReclamation(ctx, o); !errors.Is(err, context.Canceled) || report.Complete {
		t.Fatalf("%+v %v", report, err)
	}
	if _, err := os.Stat(r.Location); err != nil {
		t.Fatal(err)
	}
}
func TestReclamationHoldsExclusionThroughRetention(t *testing.T) {
	o, m, r := reclaimFixture(t, "agent-build")
	o.Retention = retentionFixture(func(ctx context.Context, _ RepositoryScope, v func(RetentionEvidence) error) error {
		waiting, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := m.AcquireUse(waiting, r); !errors.Is(err, context.Canceled) {
			t.Fatalf("admission during retention: %v", err)
		}
		err := v(RetentionEvidence{Complete: true, Stable: true})
		if _, statErr := os.Stat(r.Location); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("callback released before removal")
		}
		return err
	})
	if report, err := ApplyReclamation(context.Background(), o); err != nil || report.Resources[0].Outcome != ReclaimRemoved {
		t.Fatalf("%+v %v", report, err)
	}
	u, err := m.AcquireUse(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err = u.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func addReclaimResource(t *testing.T, o ReclaimOptions, m *storageuse.Manager, repo, class string) storageuse.Resource {
	t.Helper()
	common := filepath.Join(repo, ".git")
	h := sha256.Sum256([]byte(common))
	key := hex.EncodeToString(h[:16])
	r := storageuse.ResourceFor(key, class, filepath.Join(o.CacheRoot, class, key, "go"))
	u, err := m.AcquireUse(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(r.Location, "data"), "reproducible")
	if err = u.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestReclamationMultipleRepositoriesHardlinksAndScopes(t *testing.T) {
	o, m, first := reclaimFixture(t, "agent-build")
	repo := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	second := addReclaimResource(t, o, m, repo, "validate-deps")
	// Register a second repository but initially exclude it from authority.
	report, err := ApplyReclamation(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second.Location); err != nil {
		t.Fatal("outside scope removed", err)
	}
	if _, err := os.Stat(first.Location); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first %+v %v", report, err)
	}
	// Recreate first through admission, then share an inode across repositories
	// and an unmanaged external name. The report claims namespace observations.
	u, err := m.AcquireUse(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(first.Location, "data"), "reproducible")
	if err = u.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(second.Location, "data")); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(filepath.Join(first.Location, "data"), filepath.Join(second.Location, "data")); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "outside")
	if err = os.Link(filepath.Join(first.Location, "data"), external); err != nil {
		t.Fatal(err)
	}
	o.Scopes = append(o.Scopes, RepositoryScope{repo, filepath.Join(repo, ".git"), "github"})
	preview, err := PreviewReclamation(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	var localAllocated int64
	for _, r := range preview.Resources {
		if r.Before.AllocatedBytes != nil {
			localAllocated += *r.Before.AllocatedBytes
		}
	}
	if preview.Before.AllocatedBytes == nil || *preview.Before.AllocatedBytes >= localAllocated {
		t.Fatalf("shared inode counted twice: %+v local %d", preview, localAllocated)
	}
	o.Scopes[0], o.Scopes[1] = o.Scopes[1], o.Scopes[0]
	o.Scopes = append(o.Scopes, o.Scopes[0]) // canonical duplicate scope is deduplicated
	applied, err := ApplyReclamation(context.Background(), o)
	if err != nil {
		t.Fatalf("%+v %v", applied, err)
	}
	data, err := os.ReadFile(external)
	if err != nil || string(data) != "reproducible" {
		t.Fatal("external hardlink changed", err)
	}
	for _, r := range applied.Resources {
		if r.Outcome != ReclaimRemoved {
			t.Fatalf("%+v", applied)
		}
	}
}
func TestReclamationCancellationAfterRemovalAndIncompleteDiscovery(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		o, m, first := reclaimFixture(t, "agent-build")
		second := addReclaimResource(t, o, m, o.Scopes[0].Root, "validate-build")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		o.Retention = retentionFixture(func(_ context.Context, _ RepositoryScope, v func(RetentionEvidence) error) error {
			calls++
			err := v(RetentionEvidence{Complete: true, Stable: true})
			cancel()
			return err
		})
		report, err := ApplyReclamation(ctx, o)
		if !errors.Is(err, context.Canceled) || report.Complete || calls != 1 {
			t.Fatalf("%+v %v calls=%d", report, err, calls)
		}
		if _, err := os.Stat(first.Location); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("first removal missing", err)
		}
		if _, err := os.Stat(second.Location); err != nil {
			t.Fatal("scheduled after cancellation", err)
		}
		// Evidence and lock files remain usable on rerun.
		o.Retention = completeRetention()
		if _, err := ApplyReclamation(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("bounded", func(t *testing.T) {
		o, _, r := reclaimFixture(t, "agent-build")
		o.Limits = DefaultLimits()
		o.Limits.Entries = 1
		report, err := ApplyReclamation(context.Background(), o)
		if err == nil || report.Complete {
			t.Fatalf("%+v %v", report, err)
		}
		if _, err := os.Stat(r.Location); err != nil {
			t.Fatal("incomplete discovery authorized removal", err)
		}
	})
}
