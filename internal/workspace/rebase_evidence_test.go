package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAggregateRebasePatchIdentity(t *testing.T) {
	ctx := context.Background()
	root, _, base, head := prFixture(t)
	for _, tc := range []struct{ name, a, b, c, d, want string }{
		{"same", base, head, base, head, "unchanged"},
		{"empty", base, base, head, head, "unchanged"},
		{"changed", base, head, base, base, "changed"},
		{"missing", base, head, base, "0000000000000000000000000000000000000000", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RebasePatchIdentity(ctx, root, tc.a, tc.b, tc.c, tc.d)
			if got != tc.want || (err != nil) != (tc.want == "unavailable") {
				t.Fatalf("identity=%s err=%v", got, err)
			}
		})
	}
}

func TestNormalizeGitOutput(t *testing.T) {
	for _, input := range []string{"one\r\ntwo\rthree", "one\ntwo\nthree"} {
		if got := normalizeGitOutput(input); got != "one\ntwo\nthree" {
			t.Fatalf("output=%q", got)
		}
	}
}

// droppedFixtureCommit commits files on top of parent; an empty value deletes.
func droppedFixtureCommit(t *testing.T, repo, parent string, files map[string]string) string {
	t.Helper()
	runWorkspaceGit(t, repo, "checkout", "-q", "--detach", parent)
	for name, body := range files {
		p := filepath.Join(repo, name)
		if body == "" {
			runWorkspaceGit(t, repo, "rm", "-q", "--", name)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, repo, "add", "--", name)
	}
	runWorkspaceGit(t, repo, "commit", "-q", "--allow-empty", "-m", "fixture")
	return workspaceGitOutput(t, repo, "rev-parse", "HEAD")
}

func TestRebaseDroppedPaths(t *testing.T) {
	ctx := context.Background()
	repo, _, base, _ := prFixture(t)
	odd := "tab\tand\nnewline name.txt"
	oldHead := droppedFixtureCommit(t, repo, base, map[string]string{"a.txt": "pr\n", "b.txt": "pr\n", "c dir/space.txt": "pr\n", odd: "pr\n", "bin.dat": "\x00\x01pr"})
	renamed := droppedFixtureCommit(t, repo, base, map[string]string{"feature.txt": "", "moved.txt": "base\n"})
	newBase := droppedFixtureCommit(t, repo, base, map[string]string{"b.txt": "pr\n", "upstream.txt": "up\n"})
	keepAll := droppedFixtureCommit(t, repo, newBase, map[string]string{"a.txt": "pr\n", "b.txt": "different\n", "c dir/space.txt": "pr\n", odd: "pr\n", "bin.dat": "\x00\x01pr"})
	dropTwo := droppedFixtureCommit(t, repo, newBase, map[string]string{"c dir/space.txt": "pr\n", odd: "pr\n", "new.txt": "added by rebase\n"})
	addOnly := droppedFixtureCommit(t, repo, newBase, map[string]string{"a.txt": "pr\n", "b.txt": "x\n", "c dir/space.txt": "pr\n", odd: "pr\n", "bin.dat": "\x00\x01pr", "new.txt": "added\n"})
	missing := "0000000000000000000000000000000000000000"
	for _, tc := range []struct {
		name                string
		oldHead, newB, newH string
		want                []string
		wantErr             bool
	}{
		{"kept even with different content", oldHead, newBase, keepAll, []string{}, false},
		{"dropped and absorbed upstream are reported sorted", oldHead, newBase, dropTwo, []string{"a.txt", "b.txt", "bin.dat"}, false},
		{"newly changed path does not hide or count as dropped", oldHead, newBase, addOnly, []string{}, false},
		{"rename counts both endpoints", renamed, base, base, []string{"feature.txt", "moved.txt"}, false},
		{"literal names with spaces tabs and newlines", oldHead, base, base, []string{"a.txt", "b.txt", "bin.dat", "c dir/space.txt", odd}, false},
		{"missing commit is unavailable", oldHead, newBase, missing, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RebaseDroppedPaths(ctx, repo, base, tc.oldHead, tc.newB, tc.newH)
			if (err != nil) != tc.wantErr || !slices.Equal(got, tc.want) || (got == nil) != (tc.want == nil) {
				t.Fatalf("dropped=%q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}
