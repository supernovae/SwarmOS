package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

func TestHarnessCleanupPreservesUserInstructions(t *testing.T) {
	tmp := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatalf("InitRepository failed: %v", err)
	}
	userFiles := map[string]string{
		"AGENTS.md":                      "    indented instructions\n\n",
		"CLAUDE.md":                      "\n# Local instructions\n\nKeep this whitespace.\n\n",
		".claude/commands/personal.md":   "user-owned Claude command",
		".opencode/commands/personal.md": "user-owned OpenCode command",
	}
	for path, content := range userFiles {
		if err := os.WriteFile(filepath.Join(tmp, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.test"},
		{"add", "."},
		{"commit", "-m", "harness"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", tmp}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, out)
		}
	}

	base := workspaceGitOutput(t, tmp, "rev-parse", "HEAD")
	wtPath, _, err := ProvisionAt(tmp, 12, "[Swarm] Add Version Command", base, "", "")
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	// Git may check out CRLF on Windows. Preserve the actual user bytes in this
	// worktree, independently of Git's checkout conversion.
	for path := range userFiles {
		content, err := os.ReadFile(filepath.Join(wtPath, path))
		if err != nil {
			t.Fatal(err)
		}
		userFiles[path] = string(content)
	}

	wu := &contract.WorkUnit{
		ID:          "ISSUE-12",
		IssueNumber: 12,
		Title:       "Add Version Command",
		Objective:   "Add swarm version subcommand",
		Constraints: []string{"Stdlib only"},
		Acceptance:  []string{"swarm version --json outputs valid JSON"},
		Workflow:    "feature",
		Gate:        contract.GateAgent,
	}
	wf, err := harness.LoadWorkflow(tmp, "feature")
	if err != nil {
		t.Fatal(err)
	}

	if err := CompileHarnessBundleAt(wtPath, wu, wf, nil, "planner", base); err != nil {
		t.Fatalf("CompileHarnessBundle failed: %v", err)
	}

	// Replacing the active role must not accumulate markers or consume user text.
	if err := CompileHarnessBundleAt(wtPath, wu, wf, nil, "reviewer", base); err != nil {
		t.Fatal(err)
	}

	if err := CleanEphemeralHarnessMarkers(wtPath); err != nil {
		t.Fatalf("CleanEphemeralHarnessMarkers failed: %v", err)
	}
	for path, want := range userFiles {
		got, err := os.ReadFile(filepath.Join(wtPath, path))
		if err != nil || string(got) != want {
			t.Errorf("cleanup changed user-owned %s: got %q, want %q, error %v", path, got, want, err)
		}
	}
}

func TestProvisionAtPinsNonMainRemoteDefault(t *testing.T) {
	remote := t.TempDir()
	runWorkspaceGit(t, remote, "init", "--bare")
	repo := t.TempDir()
	runWorkspaceGit(t, repo, "init", "-b", "trunk")
	runWorkspaceGit(t, repo, "config", "user.name", "Test")
	runWorkspaceGit(t, repo, "config", "user.email", "test@example.test")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repo, "add", ".")
	runWorkspaceGit(t, repo, "commit", "-m", "base")
	runWorkspaceGit(t, repo, "remote", "add", "origin", remote)
	runWorkspaceGit(t, repo, "push", "-u", "origin", "trunk")
	runWorkspaceGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	base, err := ResolveBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repo, "checkout", "-b", "unrelated")
	wt, branch, err := ProvisionAt(repo, 13, "Pinned base", base, BranchName(13, "Pinned base"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := workspaceGitOutput(t, wt, "rev-parse", "HEAD"); got != base {
		t.Fatalf("worktree base = %s, want remote default %s", got, base)
	}
	if branch == "" {
		t.Fatal("expected branch")
	}
}

func TestCheckpointPreservesPrimaryDirtyCheckout(t *testing.T) {
	repo := t.TempDir()
	runWorkspaceGit(t, repo, "init", "-b", "main")
	runWorkspaceGit(t, repo, "config", "user.name", "Test")
	runWorkspaceGit(t, repo, "config", "user.email", "test@example.test")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repo, "add", ".")
	runWorkspaceGit(t, repo, "commit", "-m", "base")
	base := workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("primary dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, branch, err := ProvisionAt(repo, 14, "Checkpoint", base, BranchName(14, "Checkpoint"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "checkpoint.txt"), []byte("checkpoint"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Checkpoint(context.Background(), wt, branch); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "tracked.txt")); err != nil || string(got) != "primary dirty" {
		t.Fatalf("primary dirt was changed: %q %v", got, err)
	}
}

func runWorkspaceGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

func TestProvisionAtRestoresExactCheckpointInFreshClone(t *testing.T) {
	remote := t.TempDir()
	runWorkspaceGit(t, remote, "init", "--bare")
	first := t.TempDir()
	runWorkspaceGit(t, first, "init", "-b", "main")
	runWorkspaceGit(t, first, "config", "user.name", "Test")
	runWorkspaceGit(t, first, "config", "user.email", "test@example.test")
	if err := os.WriteFile(filepath.Join(first, "base.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, first, "add", ".")
	runWorkspaceGit(t, first, "commit", "-m", "base")
	runWorkspaceGit(t, first, "remote", "add", "origin", remote)
	runWorkspaceGit(t, first, "push", "-u", "origin", "main")
	runWorkspaceGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	base, err := ResolveBase(first)
	if err != nil {
		t.Fatal(err)
	}
	branch := BranchName(15, "transfer")
	wt, _, err := ProvisionAt(first, 15, "transfer", base, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "checkpoint.txt"), []byte("transferred"), 0o644); err != nil {
		t.Fatal(err)
	}
	head, err := Checkpoint(context.Background(), wt, branch)
	if err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, wt, "push", "origin", "HEAD:refs/heads/"+branch)

	secondParent := t.TempDir()
	second := filepath.Join(secondParent, "clone")
	runWorkspaceGit(t, secondParent, "clone", remote, second)
	resumed, _, err := ProvisionAt(second, 15, "transfer", base, branch, head)
	if err != nil {
		t.Fatal(err)
	}
	if got := workspaceGitOutput(t, resumed, "rev-parse", "HEAD"); got != head {
		t.Fatalf("resumed head = %s, want %s", got, head)
	}
	if _, err := os.Stat(filepath.Join(resumed, "checkpoint.txt")); err != nil {
		t.Fatalf("checkpoint content missing: %v", err)
	}
}

func workspaceGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestManagedInstructionsRejectSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("private instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := injectManagedBlock(link, ActiveMarkerStart+"\nmanaged\n"+ActiveMarkerEnd); err == nil {
		t.Fatal("symlinked instruction file accepted")
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "private instructions" {
		t.Fatalf("outside instructions changed: %q, %v", got, err)
	}
}
