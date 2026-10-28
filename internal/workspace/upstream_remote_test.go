package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// upstreamRemoteFixture is a checkout whose origin is a personal fork with a
// different default branch and whose upstream is the project.
type upstreamRemoteFixture struct {
	root, project, fork string
	base, forkBase      string
}

func newUpstreamRemoteFixture(t *testing.T) upstreamRemoteFixture {
	t.Helper()
	dir := t.TempDir()
	f := upstreamRemoteFixture{project: filepath.Join(dir, "octo", "swarm.git"), fork: filepath.Join(dir, "alice", "swarm.git"), root: filepath.Join(dir, "clone")}
	seed := filepath.Join(dir, "seed")
	runWorkspaceGit(t, dir, "init", "--bare", "--initial-branch=main", f.project)
	runWorkspaceGit(t, dir, "init", "-b", "main", seed)
	runWorkspaceGit(t, seed, "config", "user.name", "Test")
	runWorkspaceGit(t, seed, "config", "user.email", "test@example.test")
	runWorkspaceGit(t, seed, "commit", "--allow-empty", "-m", "project base")
	runWorkspaceGit(t, seed, "push", f.project, "main")
	f.base = workspaceGitOutput(t, seed, "rev-parse", "HEAD")
	runWorkspaceGit(t, dir, "clone", "--bare", f.project, f.fork)
	runWorkspaceGit(t, seed, "commit", "--allow-empty", "-m", "fork-only")
	runWorkspaceGit(t, seed, "push", f.fork, "HEAD:refs/heads/fork-default")
	runWorkspaceGit(t, f.fork, "symbolic-ref", "HEAD", "refs/heads/fork-default")
	f.forkBase = workspaceGitOutput(t, seed, "rev-parse", "HEAD")
	runWorkspaceGit(t, dir, "clone", f.fork, f.root)
	runWorkspaceGit(t, f.root, "config", "user.name", "Test")
	runWorkspaceGit(t, f.root, "config", "user.email", "test@example.test")
	runWorkspaceGit(t, f.root, "remote", "add", "upstream", f.project)
	return f
}

func TestResolveBaseFromNamedUpstream(t *testing.T) {
	t.Parallel()
	f := newUpstreamRemoteFixture(t)
	if got, err := ResolveBaseFrom(f.root, "upstream"); err != nil || got != f.base {
		t.Fatalf("ResolveBaseFrom(upstream) = %s, %v; want project base %s", got, err, f.base)
	}
	if got, err := ResolveBase(f.root); err != nil || got != f.forkBase {
		t.Fatalf("ResolveBase (origin) = %s, %v; want %s", got, err, f.forkBase)
	}
	// A missing named upstream never falls back to local HEAD.
	if _, err := ResolveBaseFrom(f.root, "project"); err == nil || !strings.Contains(err.Error(), `upstream remote "project" is not configured`) || !strings.Contains(err.Error(), "git remote add project <project-url>") {
		t.Fatalf("missing upstream: %v", err)
	}
	if _, err := ResolveBaseFrom(f.root, "--upload-pack=x"); err == nil {
		t.Fatal("option-like remote name accepted")
	}
}

func TestValidateRemoteName(t *testing.T) {
	t.Parallel()
	for name, ok := range map[string]bool{"origin": true, "upstream": true, "my-fork.2": true, "Fork_1": true,
		"": false, "-x": false, ".hidden": false, "a/b": false, "a b": false, "a..b": false, "x.lock": false, "a:b": false, "trailing.": false} {
		if err := ValidateRemoteName(name); (err == nil) != ok {
			t.Errorf("ValidateRemoteName(%q) = %v, want ok=%v", name, err, ok)
		}
	}
}

func TestResolveForkRemoteSlugRelativeToUpstream(t *testing.T) {
	t.Parallel()
	f := newUpstreamRemoteFixture(t)
	if slug, err := ResolveForkRemoteSlug(f.root, "upstream", "origin"); err != nil || slug != "alice/swarm" {
		t.Fatalf("origin as fork = %s, %v", slug, err)
	}
	if slug, err := ResolveForkRemoteSlug(f.root, "upstream", "upstream"); err != nil || slug != "octo/swarm" {
		t.Fatalf("upstream itself = %s, %v", slug, err)
	}
	runWorkspaceGit(t, f.root, "remote", "add", "mirror", f.project)
	if _, err := ResolveForkRemoteSlug(f.root, "upstream", "mirror"); err == nil || !strings.Contains(err.Error(), "same URL as upstream upstream") {
		t.Fatalf("fork at upstream URL: %v", err)
	}
	// The legacy wrapper still compares against origin.
	if _, err := ResolveRemoteGitHubSlug(f.root, "origin"); err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]string{"https://github.com/octo/swarm.git/": "octo/swarm", "git@github.com:Octo/Swarm.git": "Octo/Swarm", "/srv/octo/swarm.git/": "octo/swarm"} {
		if got, err := parseGitRemoteSlug(raw); err != nil || got != want {
			t.Errorf("parseGitRemoteSlug(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestClaimCheckpointFrom(t *testing.T) {
	t.Parallel()
	if remote, branch := ClaimCheckpointFrom("upstream", false, "origin", 3, "Task", "c1", ""); remote != "upstream" || branch != BranchName(3, "Task") {
		t.Fatalf("same repository = %s %s", remote, branch)
	}
	if remote, branch := ClaimCheckpointFrom("upstream", true, "origin", 3, "Task", "c1", ""); remote != "origin" || branch != ForkBranchName(3, "c1") {
		t.Fatalf("fork = %s %s", remote, branch)
	}
	if remote, _ := ClaimCheckpoint(false, "fork", 3, "Task", "c1", ""); remote != "origin" {
		t.Fatalf("legacy = %s", remote)
	}
}

func TestPushForkCheckpointFromAllowsOriginOnlyWhenNotUpstream(t *testing.T) {
	t.Parallel()
	f := newUpstreamRemoteFixture(t)
	ctx := context.Background()
	branch := ForkBranchName(4, "claim")
	runWorkspaceGit(t, f.root, "fetch", "upstream", f.base)
	if err := PushForkCheckpointFrom(ctx, f.root, "upstream", "upstream", branch, f.base); err == nil || !strings.Contains(err.Error(), "other than upstream upstream") {
		t.Fatalf("push to upstream: %v", err)
	}
	if err := PushForkCheckpoint(ctx, f.root, "origin", branch, f.base); err == nil || !strings.Contains(err.Error(), "non-origin") {
		t.Fatalf("legacy push to origin: %v", err)
	}
	if err := PushForkCheckpointFrom(ctx, f.root, "upstream", "origin", branch, f.base); err != nil {
		t.Fatal(err)
	}
	if got := workspaceGitOutput(t, f.fork, "rev-parse", "refs/heads/"+branch); got != f.base {
		t.Fatalf("fork branch = %s", got)
	}
	if out := workspaceGitOutput(t, f.project, "for-each-ref", "refs/heads/swarm"); out != "" {
		t.Fatalf("project received the fork checkpoint: %s", out)
	}
}

func TestProvisionWithContextNamedUpstream(t *testing.T) {
	t.Parallel()
	f := newUpstreamRemoteFixture(t)
	// A newer project base exists only upstream; provisioning fetches it there.
	maintainer := filepath.Join(t.TempDir(), "maintainer")
	runWorkspaceGit(t, filepath.Dir(maintainer), "clone", f.project, maintainer)
	runWorkspaceGit(t, maintainer, "-c", "user.name=M", "-c", "user.email=m@example.test", "commit", "--allow-empty", "-m", "upstream only")
	runWorkspaceGit(t, maintainer, "push", "origin", "main")
	newBase := workspaceGitOutput(t, maintainer, "rev-parse", "HEAD")
	if _, err := resolveCommit(f.root, newBase); err == nil {
		t.Fatal("fixture base is already local")
	}
	pc := ProvisionContext{UpstreamRemote: "upstream"}
	branch := ForkBranchName(5, "claim")
	if _, _, err := ProvisionFromRemoteAt(f.root, "origin", 5, "Task", newBase, branch, ""); err == nil {
		t.Fatal("origin-based provisioning found an upstream-only base")
	}
	wt, got, err := ProvisionWithContext(f.root, "origin", 5, "Task", newBase, branch, "", pc)
	if err != nil || got != branch {
		t.Fatalf("provision = %s %s %v", wt, got, err)
	}
	if head := workspaceGitOutput(t, wt, "rev-parse", "HEAD"); head != newBase {
		t.Fatalf("worktree head = %s, want %s", head, newBase)
	}
	// An origin checkpoint is a fork checkpoint here, never same-repository
	// dropped-checkpoint recovery.
	pc.Dropped = &DroppedCheckpoint{Branch: BranchName(6, "x"), HeadSHA: f.base}
	if _, _, err := ProvisionWithContext(f.root, "origin", 6, "x", f.base, "", "", pc); err == nil || !strings.Contains(err.Error(), "invalid checkpoint recovery context") {
		t.Fatalf("origin treated as same-repository: %v", err)
	}
	if _, _, err := ProvisionWithContext(f.root, "origin", 7, "x", f.base, "", "", ProvisionContext{UpstreamRemote: "bad name"}); err == nil {
		t.Fatal("invalid upstream name accepted")
	}
	if _, err := os.Stat(WorktreePath(f.root, 7)); !os.IsNotExist(err) {
		t.Fatalf("rejected provision created a worktree: %v", err)
	}
}
