package workspace

import (
	"context"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompilePRHarnessBundlePreservesUnmanagedInstructions(t *testing.T) {
	repo := t.TempDir()
	runWorkspaceGit(t, repo, "init", "-b", "main")
	runWorkspaceGit(t, repo, "config", "user.name", "Test")
	runWorkspaceGit(t, repo, "config", "user.email", "test@example.test")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: repo}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("Unmanaged instruction retained.\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	runWorkspaceGit(t, repo, "add", ".")
	runWorkspaceGit(t, repo, "commit", "-m", "base")
	base := workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	runWorkspaceGit(t, repo, "branch", "feature")
	path, _, err := PreparePR(context.Background(), repo, PRWorkspaceOptions{Number: 31, HeadBranch: "feature", HeadSHA: base, BaseSHA: base})
	if err != nil {
		t.Fatal(err)
	}
	wf, err := harness.LoadWorkflow(repo, "feature")
	if err != nil {
		t.Fatal(err)
	}
	wu := &contract.WorkUnit{ID: "ISSUE-7", IssueNumber: 7, Title: "Repair", Objective: "Fix PR"}
	if err := CompilePRHarnessBundleAt(path, wu, wf, nil, "developer", base, 31); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		content, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		if !strings.Contains(text, "Unmanaged instruction retained.") || !strings.Contains(text, "swarm pr work 31") || !strings.Contains(text, "swarm issue review-plan 7 --approve") || strings.Contains(text, "swarm issue submit 7") {
			t.Fatalf("%s has incorrect PR pointer: %s", name, text)
		}
	}
}

func prFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	remote := t.TempDir()
	runWorkspaceGit(t, remote, "init", "--bare")
	repo := t.TempDir()
	runWorkspaceGit(t, repo, "init", "-b", "main")
	runWorkspaceGit(t, repo, "config", "user.name", "Test")
	runWorkspaceGit(t, repo, "config", "user.email", "test@example.test")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repo, "add", "feature.txt")
	runWorkspaceGit(t, repo, "commit", "-m", "base")
	base := workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	runWorkspaceGit(t, repo, "remote", "add", "origin", remote)
	runWorkspaceGit(t, repo, "push", "origin", "main")
	runWorkspaceGit(t, repo, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repo, "add", "feature.txt")
	runWorkspaceGit(t, repo, "commit", "-m", "feature")
	head := workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	runWorkspaceGit(t, repo, "push", "origin", "feature")
	runWorkspaceGit(t, repo, "checkout", "main")
	return repo, remote, base, head
}

func TestPRPublishExactHeadLeaseAndPreservesPrimaryCheckout(t *testing.T) {
	ctx := context.Background()
	repo, remote, base, old := prFixture(t)
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	path, branch, err := PreparePR(ctx, repo, PRWorkspaceOptions{Number: 42, Remote: "origin", HeadBranch: "feature", HeadSHA: old, BaseSHA: base})
	if err != nil {
		t.Fatal(err)
	}
	if path != PRWorktreePath(repo, 42) || branch != PRBranchName(42) {
		t.Fatalf("unexpected PR worktree %s %s", path, branch)
	}
	if err := os.WriteFile(filepath.Join(path, "new.txt"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, path, "add", "new.txt")
	runWorkspaceGit(t, path, "commit", "-m", "new")
	next := workspaceGitOutput(t, path, "rev-parse", "HEAD")
	if err := PublishPR(ctx, repo, "origin", "feature", old, next); err != nil {
		t.Fatal(err)
	}
	if got := workspaceGitOutput(t, remote, "rev-parse", "refs/heads/feature"); got != next {
		t.Fatalf("remote head %s != %s", got, next)
	}
	if err := os.WriteFile(filepath.Join(path, "more.txt"), []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, path, "add", "more.txt")
	runWorkspaceGit(t, path, "commit", "-m", "more")
	later := workspaceGitOutput(t, path, "rev-parse", "HEAD")
	runWorkspaceGit(t, repo, "checkout", "feature")
	if err := os.WriteFile(filepath.Join(repo, "competitor.txt"), []byte("other writer"), 0600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repo, "add", "competitor.txt")
	runWorkspaceGit(t, repo, "commit", "-m", "competing writer")
	winner := workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	runWorkspaceGit(t, repo, "push", "--force-with-lease=refs/heads/feature:"+next, "origin", "feature:feature")
	if err := PublishPR(ctx, repo, "origin", "feature", next, later); err == nil {
		t.Fatal("competing remote writer was overwritten")
	}
	if got := workspaceGitOutput(t, remote, "rev-parse", "refs/heads/feature"); got != winner {
		t.Fatalf("race overwrote competing writer: %s", got)
	}
	if err := PublishPR(ctx, repo, "origin", "feature", old, base); err == nil {
		t.Fatal("stale head was accepted")
	}
	if err := PublishPR(ctx, repo, "origin", "missing", next, base); err == nil {
		t.Fatal("missing remote branch was created")
	}
	if _, _, err := PreparePR(ctx, repo, PRWorkspaceOptions{Number: 42, Remote: "origin", HeadBranch: "feature", HeadSHA: old, BaseSHA: base}); err == nil {
		t.Fatal("stale upstream head accepted")
	}
	if got, err := os.ReadFile(filepath.Join(repo, "unrelated.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("primary checkout changed: %q %v", got, err)
	}
}

func TestPRRebaseConflictPreservesFilesContinueAndAbort(t *testing.T) {
	ctx := context.Background()
	repo, _, base, old := prFixture(t)
	path, _, err := PreparePR(ctx, repo, PRWorkspaceOptions{Number: 43, HeadBranch: "feature", HeadSHA: old, BaseSHA: base})
	if err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repo, "add", "feature.txt")
	runWorkspaceGit(t, repo, "commit", "-m", "main change")
	newer := workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	if _, _, err := PreparePR(ctx, repo, PRWorkspaceOptions{Number: 43, HeadBranch: "feature", HeadSHA: old, LocalHeadSHA: old, BaseSHA: newer, RequireBaseAncestor: true}); err == nil {
		t.Fatal("validation accepted a candidate missing the approved baseline")
	}
	if _, err := RebasePR(ctx, path, newer, false, false); err == nil {
		t.Fatal("expected conflict")
	}
	_, dirty, rebasing, err := PRHead(ctx, path)
	if err != nil || !dirty || !rebasing {
		t.Fatalf("conflict status dirty=%t rebasing=%t error=%v", dirty, rebasing, err)
	}
	contents, err := os.ReadFile(filepath.Join(path, "feature.txt"))
	if err != nil || !strings.Contains(string(contents), "<<<<<<<") {
		t.Fatalf("conflict lost: %q %v", contents, err)
	}
	if _, _, err := PreparePR(ctx, repo, PRWorkspaceOptions{Number: 43, HeadBranch: "feature", HeadSHA: old, BaseSHA: base}); err != nil {
		t.Fatalf("managed conflicting rebase must remain accessible: %v", err)
	}
	if _, err := RebasePR(ctx, path, newer, true, false); err == nil {
		t.Fatal("unresolved conflict continued")
	}
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("resolved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, path, "add", "feature.txt")
	if _, err := RebasePR(ctx, path, base, true, false); err == nil {
		t.Fatal("continued onto a target different from the approved baseline")
	}
	if _, _, rebasing, err := PRHead(ctx, path); err != nil || !rebasing {
		t.Fatalf("rejected continuation lost the in-progress rebase: %t %v", rebasing, err)
	}
	result, err := RebasePR(ctx, path, newer, true, false)
	if err != nil || result == old {
		t.Fatalf("continue result %s %v", result, err)
	}
	if _, dirty, rebasing, err := PRHead(ctx, path); err != nil || dirty || rebasing {
		t.Fatalf("continued status dirty=%t rebasing=%t error=%v", dirty, rebasing, err)
	}
	if _, _, err := PreparePR(ctx, repo, PRWorkspaceOptions{Number: 43, HeadBranch: "feature", HeadSHA: old, LocalHeadSHA: result, BaseSHA: base}); err != nil {
		t.Fatalf("recorded rebased PR branch must remain accessible: %v", err)
	}
	if _, _, err := PreparePR(ctx, repo, PRWorkspaceOptions{Number: 43, HeadBranch: "feature", HeadSHA: old, LocalHeadSHA: result, BaseSHA: newer, RequireBaseAncestor: true}); err != nil {
		t.Fatalf("validation rejected the rebased candidate: %v", err)
	}
	path2, _, err := PreparePR(ctx, repo, PRWorkspaceOptions{Number: 44, HeadBranch: "feature", HeadSHA: old, BaseSHA: base})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RebasePR(ctx, path2, newer, false, false); err == nil {
		t.Fatal("expected second conflict")
	}
	after, err := RebasePR(ctx, path2, newer, false, true)
	if err != nil || after != old {
		t.Fatalf("abort changed original head %s %v", after, err)
	}
}

func TestPRManualWorktreeVerificationPreservesDirtyFiles(t *testing.T) {
	ctx := context.Background()
	repo, _, base, head := prFixture(t)
	path, _, err := PreparePR(ctx, repo, PRWorkspaceOptions{Number: 45, HeadBranch: "feature", HeadSHA: head, BaseSHA: base})
	if err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(path, "draft.txt")
	if err := os.WriteFile(draft, []byte("uncommitted implementation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPRWorktree(ctx, repo, 45, head); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(draft); err != nil || string(got) != "uncommitted implementation\n" {
		t.Fatalf("manual changes were not preserved: %q %v", got, err)
	}
	if _, err := VerifyPRWorktree(ctx, repo, 45, base); err == nil {
		t.Fatal("stale checkpoint accepted")
	}
	runWorkspaceGit(t, path, "checkout", "--detach")
	if _, err := VerifyPRWorktree(ctx, repo, 45, head); err == nil {
		t.Fatal("unmanaged detached checkout accepted")
	}
	runWorkspaceGit(t, path, "checkout", PRBranchName(45))
	moved := path + "-saved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := VerifyPRWorktree(ctx, repo, 45, head); err == nil {
		t.Fatal("symlinked PR worktree accepted")
	}
}

func TestPreparePRRecreatesHandRemovedWorktree(t *testing.T) {
	ctx := context.Background()
	repo, _, base, head := prFixture(t)
	opts := PRWorkspaceOptions{Number: 7, Remote: "origin", HeadBranch: "feature", HeadSHA: head, BaseSHA: base}
	path, _, err := PreparePR(ctx, repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	again, branch, err := PreparePR(ctx, repo, opts)
	if err != nil {
		t.Fatalf("re-prepare after removing the PR worktree directory: %v", err)
	}
	if again != path || workspaceGitOutput(t, again, "rev-parse", "HEAD") != head || branch != PRBranchName(7) {
		t.Fatalf("recreated PR worktree %s on %s", again, branch)
	}
}
