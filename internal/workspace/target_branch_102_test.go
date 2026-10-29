package workspace

import (
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"path/filepath"
	"strings"
	"testing"
)

func TestIssue102ExactBranchFetch(t *testing.T) {
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "init", "-b", "trunk")
	git(root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "base")
	base := git(root, "rev-parse", "HEAD")
	git(root, "branch", "release/1.x")
	if got, err := ResolveBranchFrom(root, "origin", "release/1.x"); err != nil || got != base {
		t.Fatalf("local: %s %v", got, err)
	}
	if _, err := ResolveBranchFrom(root, "upstream", "release/1.x"); err == nil {
		t.Fatal("missing named upstream fell back")
	}
	remote := filepath.Join(t.TempDir(), "upstream.git")
	git(root, "init", "--bare", "--initial-branch=trunk", remote)
	git(root, "remote", "add", "origin", remote)
	git(root, "push", "origin", "trunk", "release/1.x")
	if name, err := BaseBranchName(root, "origin", ""); err != nil || name != "trunk" {
		t.Fatalf("default: %s %v", name, err)
	}
	if got, err := ResolveBranchFrom(root, "origin", "release/1.x"); err != nil || got != base {
		t.Fatalf("remote: %s %v", got, err)
	}
	if VerifyBaseBranch(root, "origin", "release/1.x", "trunk") == nil {
		t.Fatal("same-SHA retarget accepted")
	}
	git(remote, "update-ref", "-d", "refs/heads/release/1.x")
	if _, err := ResolveBranchFrom(root, "origin", "release/1.x"); err == nil || !strings.Contains(err.Error(), "release/1.x") {
		t.Fatalf("cached ref accepted: %v", err)
	}
	git(root, "tag", "tag-only")
	if _, err := ResolveBranchFrom(root, "origin", "tag-only"); err == nil {
		t.Fatal("tag accepted")
	}
}
