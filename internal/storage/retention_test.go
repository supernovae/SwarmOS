package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/safefs"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

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
		if class == "sandbox-scratch" {
			path = filepath.Join(o.ScratchRoot, "swarm-worker-fixture")
		}
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
			snapshot := reclamationSnapshot(t, filepath.Dir(o.ControlRoot))
			before, _ := os.Stat(o.ControlRoot)
			records, _ := os.ReadDir(o.ControlRoot)
			preview, err := PreviewReclamation(context.Background(), o)
			if err != nil || len(preview.Resources) != 1 || preview.Resources[0].Outcome != ReclaimEligible {
				t.Fatalf("%+v %v", preview, err)
			}
			if !reflect.DeepEqual(snapshot, reclamationSnapshot(t, filepath.Dir(o.ControlRoot))) {
				t.Fatal("preview changed content, metadata or paths")
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
		waiting, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		if _, err := m.AcquireUse(waiting, r); !errors.Is(err, context.DeadlineExceeded) {
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

// Snapshot all fixture roots, including Git/workflow files and durable records.
func reclamationSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		data := []byte(nil)
		if info.Mode().IsRegular() {
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		out[path] = fmt.Sprintf("%v %d %d %x", info.Mode(), info.Size(), info.ModTime().UnixNano(), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReclamationLinkedWorktreeScopes(t *testing.T) {
	o, _, _ := reclaimFixture(t, "agent-build")
	repo := o.Scopes[0].Root
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
	}
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-qm", "base")
	linked := filepath.Join(filepath.Dir(repo), "linked")
	git("worktree", "add", "-qb", "linked", linked)
	o.Scopes = append(o.Scopes, RepositoryScope{Root: linked, CommonDirectory: o.Scopes[0].CommonDirectory, Backend: "local"})
	snapshot := reclamationSnapshot(t, linked)
	report, err := ApplyReclamation(context.Background(), o)
	if err != nil || len(report.Resources) != 1 || report.Resources[0].Outcome != ReclaimRemoved {
		t.Fatalf("%+v %v", report, err)
	}
	if !reflect.DeepEqual(snapshot, reclamationSnapshot(t, linked)) {
		t.Fatal("linked worktree changed")
	}
}

func TestReclamationInjectedFailures(t *testing.T) {
	for _, phase := range []string{"remove", "after-stat"} {
		t.Run(phase, func(t *testing.T) {
			o, m, first := reclaimFixture(t, "agent-build")
			second := addReclaimResource(t, o, m, o.Scopes[0].Root, "validate-build")
			injected := errors.New("injected " + phase)
			ops := reclamationIO{
				afterStat: func(p *safefs.Root, name string) (os.FileInfo, error) { return p.Lstat(name) },
			}
			calls := 0
			// Candidate order is agent-build before validate-build.
			ops.remove = func(p *safefs.Root, ctx context.Context, name string, info os.FileInfo, visit func() error) error {
				calls++
				if phase == "remove" && calls == 1 {
					return injected
				}
				return p.RemoveAllCheckedContext(ctx, name, info, visit)
			}
			stats := 0
			ops.afterStat = func(p *safefs.Root, name string) (os.FileInfo, error) {
				stats++
				waiting, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
				defer cancel()
				resource := first
				if stats == 2 {
					resource = second
				}
				if _, err := m.AcquireUse(waiting, resource); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("admission during after observation: %v", err)
				}
				if phase == "after-stat" && stats == 1 {
					return nil, injected
				}
				return p.Lstat(name)
			}
			report, err := reclaimWithIO(context.Background(), o, true, ops)
			if !errors.Is(err, injected) || report.Complete || report.Resources[0].Outcome != ReclaimPartial {
				t.Fatalf("%+v %v", report, err)
			}
			if _, err := os.Stat(second.Location); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("independent resource not removed", err)
			}
			if phase == "after-stat" && report.Resources[0].ObservedLogicalDelta != nil {
				t.Fatal("failed after-stat claimed savings")
			}
			u, err := m.AcquireUse(context.Background(), first)
			if err != nil {
				t.Fatal("exclusion leaked", err)
			}
			if err = u.Finish(context.Background()); err != nil {
				t.Fatal(err)
			}
			again, err := ApplyReclamation(context.Background(), o)
			if err != nil {
				t.Fatalf("rerun %+v %v", again, err)
			}
		})
	}
}

type backendPRFixture struct {
	pr    *tracker.PullRequest
	visit func()
	sha   func() string
}

func (f backendPRFixture) UpstreamRepo() string { return "fixture/repo" }

func (f backendPRFixture) VisitStateRefs(_ context.Context, _ tracker.InventoryBudget, v func(int, string) error) error {
	if f.sha == nil {
		return errors.New("state refs unavailable")
	}
	return v(1, f.sha())
}
func (f backendPRFixture) VisitPullRequests(_ context.Context, _ tracker.InventoryBudget, v func(*tracker.PullRequest) error) error {
	if f.visit != nil {
		f.visit()
	}
	return v(f.pr)
}

// backendReclaimFixture uses actual local authority or a bare Git remote. Only
// GitHub's PR API is replaced; state enumeration and object fetches run Git.
func backendReclaimFixture(t *testing.T, backend, class string) (ReclaimOptions, *storageuse.Manager, storageuse.Resource, func(string), string) {
	t.Helper()
	o, m, r := reclaimFixture(t, class)
	repo := o.Scopes[0].Root
	o.Scopes[0].Backend = backend
	state := func(status string) string {
		return fmt.Sprintf(`{"lease":{"version":1,"issue_number":1,"status":%q},"branch":"swarm/issue-1-test","submitted_head_sha":"abc","pr_number":2}`, status)
	}
	now := time.Now()
	pr := &tracker.PullRequest{Number: 2, State: "closed", Merged: true, MergedAt: &now, MergeCommitSHA: "def", HeadSHA: "abc", Head: "swarm/issue-1-test", HeadRepo: "fixture/repo", BaseRepo: "fixture/repo"}
	update := func(status string) {
		put(t, filepath.Join(repo, ".swarm", "coordination", "issue-1.json"), state(status))
	}
	remote := ""
	pinned := ""
	if backend == "github" {
		seed := t.TempDir()
		remote = filepath.Join(t.TempDir(), "remote.git")
		git := func(root string, args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %s %v", args, out, err)
			}
			return strings.TrimSpace(string(out))
		}
		git(seed, "init", "-q")
		git(seed, "init", "--bare", "-q", remote)
		git(seed, "remote", "add", "origin", remote)
		git(repo, "remote", "add", "origin", remote)
		update = func(status string) {
			put(t, filepath.Join(seed, "state.json"), state(status))
			git(seed, "add", "state.json")
			git(seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", status)
			git(seed, "push", "-q", "origin", "HEAD:refs/heads/swarm-state/issue-1")
			pinned = git(seed, "rev-parse", "HEAD")
		}
	}
	update("completed")
	o.Retention = BackendRetention{Apply: true, Tracker: func(RepositoryScope) (tracker.PullRequestVisitor, error) {
		return backendPRFixture{pr: pr, sha: func() string { return pinned }}, nil
	}}
	return o, m, r, update, remote
}
func TestReclamationBackendSnapshotsAndEligibleRemoval(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		for _, class := range []string{"agent-build", "validate-build", "agent-deps", "validate-deps", "native-scratch", "sandbox-scratch"} {
			t.Run(backend+"/"+class, func(t *testing.T) {
				o, _, r, _, _ := backendReclaimFixture(t, backend, class)
				source := o.Retention.(BackendRetention)
				source.Apply = false
				o.Retention = source
				preview, err := PreviewReclamation(context.Background(), o)
				if err != nil {
					t.Fatal(err)
				}
				want := ReclaimEligible
				if backend == "github" {
					want = ReclaimRetained
				}
				if len(preview.Resources) != 1 || preview.Resources[0].Outcome != want {
					t.Fatalf("preview %+v", preview)
				}
				if _, err = os.Stat(r.Location); err != nil {
					t.Fatal(err)
				}
				source.Apply = true
				o.Retention = source
				applied, err := ApplyReclamation(context.Background(), o)
				if err != nil || applied.Resources[0].Outcome != ReclaimRemoved {
					t.Fatalf("apply %+v %v", applied, err)
				}
				again, err := ApplyReclamation(context.Background(), o)
				foundAbsent := false
				for _, item := range again.Resources {
					if item.Resource.Location == r.Location && item.Outcome == ReclaimAbsent {
						foundAbsent = true
					}
				}
				if err != nil || !foundAbsent {
					t.Fatalf("rerun %+v %v", again, err)
				}
			})
		}
	}
}
func TestReclamationBackendFreshnessAndFetchFailure(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		for _, scenario := range []string{"advance", "unreadable", "active-use", "maintenance-wins"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				o, m, r, update, remote := backendReclaimFixture(t, backend, "native-scratch")
				// Establish pinned preview objects, then change authority before apply.
				b := Budget{Limits: DefaultLimits()}
				if _, err := ReadRetentionSnapshot(context.Background(), o.Scopes[0], "", true, &b); err != nil {
					t.Fatal(err)
				}
				src := o.Retention.(BackendRetention)
				src.Apply = false
				o.Retention = src
				if _, err := PreviewReclamation(context.Background(), o); err != nil {
					t.Fatal(err)
				}
				src.Apply = true
				o.Retention = src
				switch scenario {
				case "advance":
					update("active")
				case "unreadable":
					if backend == "github" {
						if err := os.Rename(remote, remote+"-gone"); err != nil {
							t.Fatal(err)
						}
					} else {
						put(t, filepath.Join(o.Scopes[0].Root, ".swarm", "coordination", "issue-1.json"), "invalid")
					}
				case "active-use":
					u, err := m.AcquireUse(context.Background(), r)
					if err != nil {
						t.Fatal(err)
					}
					defer u.Finish(context.Background())
				case "maintenance-wins":
					// A contender attempts admission while the real backend source visits.
					// Cancellation provides a deterministic barrier without timing sleeps.
					original := src.Tracker
					src.Tracker = func(scope RepositoryScope) (tracker.PullRequestVisitor, error) {
						tr, err := original(scope)
						if err != nil {
							return nil, err
						}
						f := tr.(backendPRFixture)
						f.visit = func() {
							ctx, cancel := context.WithCancel(context.Background())
							started := make(chan struct{})
							done := make(chan error, 1)
							go func() {
								close(started)
								u, e := m.AcquireUse(ctx, r)
								if u != nil {
									u.Finish(context.Background())
								}
								done <- e
							}()
							<-started
							cancel()
							if e := <-done; !errors.Is(e, context.Canceled) {
								t.Errorf("admission crossed maintenance: %v", e)
							}
						}
						return f, nil
					}
					o.Retention = src
				}
				out, err := ApplyReclamation(context.Background(), o)
				if err != nil {
					t.Fatal(err)
				}
				want := ReclaimRetained
				if scenario == "maintenance-wins" {
					want = ReclaimRemoved
				}
				if len(out.Resources) != 1 || out.Resources[0].Outcome != want {
					t.Fatalf("%+v", out)
				}
				if want == ReclaimRetained {
					if _, err := os.Stat(r.Location); err != nil {
						t.Fatal("resource lost", err)
					}
				}
			})
		}
	}
}

func TestReclamationMissingRootsRemainNonCreating(t *testing.T) {
	o, _, r := reclaimFixture(t, "native-scratch")
	o.ControlRoot = filepath.Join(t.TempDir(), "missing-control")
	o.CacheRoot = filepath.Join(t.TempDir(), "missing-cache")
	o.Retention = nil
	for _, apply := range []bool{false, true} {
		var report ReclamationReport
		var err error
		if apply {
			report, err = ApplyReclamation(context.Background(), o)
		} else {
			report, err = PreviewReclamation(context.Background(), o)
		}
		if err != nil || len(report.Resources) != 1 || report.Resources[0].Outcome != ReclaimRetained {
			t.Fatalf("%+v %v", report, err)
		}
		for _, p := range []string{o.CacheRoot, o.ControlRoot} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatal("created missing root", p, err)
			}
		}
		if _, err := os.Stat(r.Location); err != nil {
			t.Fatal("unregistered scratch removed", err)
		}
	}
}

func TestReclamationBackendMultipleRepositories(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) {
			o, m, first, _, _ := backendReclaimFixture(t, backend, "agent-build")
			other, _, outside, _, _ := backendReclaimFixture(t, backend, "validate-build")
			o.Scopes = append(o.Scopes, other.Scopes[0])
			second := addReclaimResource(t, o, m, other.Scopes[0].Root, "validate-deps")
			expected := map[string]bool{first.Location: true, second.Location: true}
			for _, scope := range o.Scopes {
				for _, class := range []string{"agent-build", "validate-build", "agent-deps", "validate-deps"} {
					item := addReclaimResource(t, o, m, scope.Root, class)
					expected[item.Location] = true
				}
				hash := sha256.Sum256([]byte(scope.CommonDirectory))
				key := hex.EncodeToString(hash[:16])
				for _, kind := range []struct{ class, prefix string }{{"native-scratch", "swarm-native-"}, {"sandbox-scratch", "swarm-worker-"}} {
					path := filepath.Join(o.ScratchRoot, kind.prefix+key)
					item := storageuse.ResourceFor(key, kind.class, path)
					use, err := m.AcquireUse(context.Background(), item)
					if err != nil {
						t.Fatal(err)
					}
					put(t, filepath.Join(path, "data"), "scratch")
					if err := use.Finish(context.Background()); err != nil {
						t.Fatal(err)
					}
					expected[path] = true
				}
			}
			outsideLink := filepath.Join(t.TempDir(), "external-hardlink")
			if err := os.Link(filepath.Join(first.Location, "data"), outsideLink); err != nil {
				t.Fatal(err)
			}
			source := o.Retention.(BackendRetention)
			source.Budget = &Budget{Limits: DefaultLimits()}
			o.Retention = source
			put(t, filepath.Join(o.CacheRoot, "go-build", "legacy", "data"), "keep legacy")
			report, err := ApplyReclamation(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			outcomes := map[string]ReclamationOutcome{}
			for _, item := range report.Resources {
				outcomes[item.Resource.Location] = item.Outcome
			}
			for path := range expected {
				if outcomes[path] != ReclaimRemoved {
					t.Fatalf("eligible leaf %s not removed: %+v", path, report)
				}
			}
			if raw, err := os.ReadFile(outsideLink); err != nil || string(raw) != "reproducible" {
				t.Fatal("external hardlink lost", err)
			}
			if _, err := os.Stat(outside.Location); err != nil {
				t.Fatal("cross-root deletion", err)
			}
			if _, err := os.Stat(filepath.Join(o.CacheRoot, "go-build", "legacy", "data")); err != nil {
				t.Fatal("legacy deletion", err)
			}
		})
	}
}
func TestReclamationBackendCacheUseWins(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) {
			o, m, r, _, _ := backendReclaimFixture(t, backend, "agent-build")
			use, err := m.AcquireUse(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			report, err := ApplyReclamation(context.Background(), o)
			if err != nil || report.Resources[0].Outcome != ReclaimRetained {
				t.Fatalf("%+v %v", report, err)
			}
			if _, err := os.Stat(r.Location); err != nil {
				t.Fatal(err)
			}
			if err := use.Finish(context.Background()); err != nil {
				t.Fatal(err)
			}
			report, err = ApplyReclamation(context.Background(), o)
			if err != nil || report.Resources[0].Outcome != ReclaimRemoved {
				t.Fatalf("idle %+v %v", report, err)
			}
		})
	}
}

// The fixture writes each ledger through a file, never one dataset-sized buffer.
func inventoryScaleObjects(t *testing.T, root string, count int, size func(int) int64) (map[int]string, []*tracker.PullRequest, int64, int64) {
	t.Helper()
	git := func(input string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	refs := map[int]string{}
	prs := []*tracker.PullRequest{}
	var total, largest int64
	chunk := strings.Repeat("x", 8192)
	for n := 1; n <= count; n++ {
		path := filepath.Join(t.TempDir(), "state.json")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		prefix := fmt.Sprintf(`{"lease":{"version":1,"issue_number":%d,"status":"completed","phase":"submitted"},"branch":"swarm/issue-%d-test","submitted_head_sha":"abc","pr_number":%d,"pr_url":"https://example.test/pr/%d","artifacts":[{"body":"`, n, n, 1000+n, 1000+n)
		suffix := `"}],"role_runs":[{"history":[true,null,{"ignored":"escaped\nvalue"}]}]}`
		want := size(n)
		padding := want - int64(len(prefix)+len(suffix))
		if padding < 0 {
			t.Fatal("invalid fixture size")
		}
		if _, err = io.WriteString(f, prefix); err != nil {
			t.Fatal(err)
		}
		for padding > 0 {
			part := int64(len(chunk))
			if padding < part {
				part = padding
			}
			if _, err = io.WriteString(f, chunk[:part]); err != nil {
				t.Fatal(err)
			}
			padding -= part
		}
		if _, err = io.WriteString(f, suffix); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		blob := git("", "hash-object", "-w", path)
		tree := git("100644 blob "+blob+"\tstate.json\n", "mktree")
		sha := git("", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit-tree", tree, "-m", "inventory")
		git("", "update-ref", fmt.Sprintf("refs/heads/swarm-state/issue-%d", n), sha)
		refs[n] = sha
		total += want
		if want > largest {
			largest = want
		}
		now := time.Now()
		prs = append(prs, &tracker.PullRequest{Number: 1000 + n, HTMLURL: fmt.Sprintf("https://example.test/pr/%d", 1000+n), State: "closed", Merged: true, MergedAt: &now, MergeCommitSHA: "def", HeadSHA: "abc", Head: fmt.Sprintf("swarm/issue-%d-test", n), HeadRepo: "fixture/repo", BaseRepo: "fixture/repo"})
	}
	return refs, prs, total, largest
}

type inventoryScaleVisitor struct {
	refs map[int]string
	prs  []*tracker.PullRequest
}

func (v inventoryScaleVisitor) UpstreamRepo() string { return "fixture/repo" }
func (v inventoryScaleVisitor) VisitStateRefs(ctx context.Context, b tracker.InventoryBudget, visit func(int, string) error) error {
	for n, sha := range v.refs {
		if err := visit(n, sha); err != nil {
			return err
		}
	}
	return nil
}
func (v inventoryScaleVisitor) VisitPullRequests(ctx context.Context, b tracker.InventoryBudget, visit func(*tracker.PullRequest) error) error {
	for _, pr := range v.prs {
		if err := visit(pr); err != nil {
			return err
		}
	}
	return nil
}

func TestReclamationInventoryRealLedgerScale(t *testing.T) {
	o, _, resource := reclaimFixture(t, "agent-build")
	o.Scopes[0].Backend = "github"
	var baseline int64
	for _, extra := range []int64{0, 10000} {
		refs, prs, total, largest := inventoryScaleObjects(t, o.Scopes[0].Root, 166, func(n int) int64 {
			if n == 1 {
				return 4533441 + extra
			}
			if n == 166 {
				return 648470 + extra
			}
			return 648464 + extra
		})
		if extra == 0 && (total < 110000000 || total > 115000000 || largest < 4533441) {
			t.Fatal(total, largest)
		}
		beforeRefs := prunedInventoryRefs(t, o.Scopes[0].Root)
		b := &Budget{Limits: DefaultLimits()}
		source := BackendRetention{Budget: b, Tracker: func(RepositoryScope) (tracker.PullRequestVisitor, error) {
			return inventoryScaleVisitor{refs, prs}, nil
		}}
		o.Retention = source
		report, err := PreviewReclamation(context.Background(), o)
		if err != nil || !report.Complete || len(report.Resources) != 1 || report.Resources[0].Outcome != ReclaimEligible {
			t.Fatalf("%+v %v", report, err)
		}
		if b.Counters.Bytes >= DefaultLimits().Bytes {
			t.Fatal(b.Counters)
		}
		if extra == 0 {
			baseline = b.Counters.Bytes
		} else if baseline != b.Counters.Bytes {
			t.Fatalf("input-scaled charge %d %d", baseline, b.Counters.Bytes)
		}
		if _, err = os.Stat(resource.Location); err != nil {
			t.Fatal(err)
		}
		if beforeRefs != prunedInventoryRefs(t, o.Scopes[0].Root) {
			t.Fatal("preview mutated state refs")
		}
		if _, err := os.Stat(filepath.Join(o.Scopes[0].Root, ".git", "FETCH_HEAD")); !os.IsNotExist(err) {
			t.Fatal("preview fetched", err)
		}
		t.Logf("refs=%d total=%d largest=%d budget=%d", len(refs), total, largest, b.Counters.Bytes)
	}
	// A bounded allocation observation catches whole-object buffering separately
	// from retained-memory accounting. Dataset setup is outside measurement.
	body := strings.NewReader(`{"lease":{"version":1,"issue_number":1},"ignored":"` + strings.Repeat("x", 4533441) + `"}`)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := lease.ProjectInventory(context.Background(), body, func(int64) error { return nil })
	runtime.ReadMemStats(&after)
	if err != nil || after.TotalAlloc-before.TotalAlloc > 1<<20 {
		t.Fatalf("allocation=%d err=%v", after.TotalAlloc-before.TotalAlloc, err)
	}
	t.Logf("parser allocation bytes=%d", after.TotalAlloc-before.TotalAlloc)
}

func TestReclamationInventoryOverLimitRetains(t *testing.T) {
	for _, backend := range []string{"github", "local"} {
		t.Run(backend, func(t *testing.T) {
			o, _, resource := reclaimFixture(t, "agent-build")
			o.Scopes[0].Backend = backend
			refs, prs, _, _ := inventoryScaleObjects(t, o.Scopes[0].Root, 1, func(int) int64 { return coordination.InventoryInputLimit + 1 })
			if backend == "github" {
				if out, err := exec.Command("git", "-C", o.Scopes[0].Root, "remote", "add", "origin", o.Scopes[0].Root).CombinedOutput(); err != nil {
					t.Fatal(string(out), err)
				}
			}
			if backend == "local" {
				data, err := exec.Command("git", "-C", o.Scopes[0].Root, "show", refs[1]+":state.json").Output()
				if err != nil {
					t.Fatal(err)
				}
				put(t, filepath.Join(o.Scopes[0].Root, ".swarm", "coordination", "issue-1.json"), string(data))
			}
			for _, missing := range []bool{false, true} {
				if missing {
					if backend == "github" {
						refs[1] = strings.Repeat("a", 40)
					} else {
						put(t, filepath.Join(o.Scopes[0].Root, ".swarm", "coordination", "issue-1.json"), "invalid")
					}
				}
				failures := map[string]error{}
				o.Retention = BackendRetention{Apply: backend == "github" && !missing, Failures: failures, Tracker: func(RepositoryScope) (tracker.PullRequestVisitor, error) {
					return inventoryScaleVisitor{refs, prs}, nil
				}}
				report, err := ApplyReclamation(context.Background(), o)
				if err != nil || report.Complete || len(report.Resources) != 1 || report.Resources[0].Outcome != ReclaimRetained || len(failures) != 1 {
					t.Fatalf("%+v failures=%v err=%v", report, failures, err)
				}
				reason := report.Resources[0].Reason
				if strings.Contains(reason, "signal: killed") || !strings.Contains(reason, "issue 1") {
					t.Fatal(reason)
				}
				if _, err = os.Stat(resource.Location); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func prunedInventoryRefs(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "show-ref").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
