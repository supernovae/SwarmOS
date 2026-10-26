package workspace

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseCleanupWorktreeName(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		n    int
	}{
		{"issue-12", "issue", 12},
		{"pr-7", "pr", 7},
		{"pr-007", "", 0},
		{"issue-0", "", 0},
		{"issue--3", "", 0},
		{"issue-+3", "", 0},
		{"issue-3x", "", 0},
		{"issue-", "", 0},
		{"pull-3", "", 0},
		{"issue-99999999999999999999", "", 0},
	} {
		kind, n := parseCleanupWorktreeName(tc.name)
		if kind != tc.kind || n != tc.n {
			t.Errorf("%s: got %q %d, want %q %d", tc.name, kind, n, tc.kind, tc.n)
		}
	}
}

func TestDiscoverCleanupWorktreesInventory(t *testing.T) {
	root, issue1, _, git := cleanupWorkspaceFixture(t)
	ctx := context.Background()
	managed := filepath.Dir(issue1)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	locked := WorktreePath(root, 2)
	git("worktree", "add", "-b", "swarm/issue-2-test", locked)
	git("worktree", "lock", locked)
	missing := PRWorktreePath(root, 4)
	git("worktree", "add", "-b", PRBranchName(4), missing)
	must(os.RemoveAll(missing))
	unregistered := PRWorktreePath(root, 5)
	must(os.MkdirAll(unregistered, 0700))
	odd := filepath.Join(managed, "pr-007")
	must(os.MkdirAll(odd, 0700))
	link := WorktreePath(root, 9)
	must(os.Symlink(t.TempDir(), link))
	elsewhere := filepath.Join(t.TempDir(), "outside")
	git("worktree", "add", "-b", "outside", elsewhere)
	before := git("worktree", "list", "--porcelain")

	got, err := DiscoverCleanupWorktrees(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	canonical := func(path string) string {
		_, p, err := cleanupPaths(root, path)
		must(err)
		return p
	}
	want := []CleanupWorktree{
		{Path: canonical(issue1), Kind: "issue", Number: 1, Present: true, Registered: true},
		{Path: canonical(locked), Kind: "issue", Number: 2, Present: true, Registered: true},
		{Path: canonical(link), Kind: "issue", Number: 9, Present: true},
		{Path: canonical(odd), Present: true},
		{Path: canonical(missing), Kind: "pr", Number: 4, Registered: true},
		{Path: canonical(unregistered), Kind: "pr", Number: 5, Present: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory:\n got %+v\nwant %+v", got, want)
	}
	if after := git("worktree", "list", "--porcelain"); after != before {
		t.Fatalf("discovery changed registrations:\n%s\n%s", before, after)
	}
	// Discovery reports symlinks; inspection keeps them.
	if i := InspectCleanupWorktree(ctx, root, link, "swarm/issue-9-test", "x"); i.Eligible || i.Absent {
		t.Fatalf("symlinked worktree not retained: %+v", i)
	}
}

func TestDiscoverCleanupWorktreesEmptyAndUnsafe(t *testing.T) {
	root, path, _, git := cleanupWorkspaceFixture(t)
	git("worktree", "remove", path)
	if err := os.Remove(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverCleanupWorktrees(context.Background(), root)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty inventory: %+v %v", got, err)
	}
	if err = os.Symlink(t.TempDir(), filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if _, err = DiscoverCleanupWorktrees(context.Background(), root); err == nil {
		t.Fatal("symlinked managed directory accepted")
	}
}
