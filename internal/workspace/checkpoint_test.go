package workspace

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/supernovae/swarmos/internal/contract"
)

func checkpointFixture(t *testing.T) (string, string, string) {
	t.Helper()
	repo := t.TempDir()
	runWorkspaceGit(t, repo, "init", "-b", "main")
	runWorkspaceGit(t, repo, "config", "user.name", "Test")
	runWorkspaceGit(t, repo, "config", "user.email", "test@example.test")
	for name, content := range map[string]string{
		"staged.txt": "old\n", "deleted.txt": "remove me\n", "AGENTS.md": "User instructions\n",
	} {
		checkpointWrite(t, repo, name, content)
	}
	runWorkspaceGit(t, repo, "add", "-A")
	runWorkspaceGit(t, repo, "commit", "-m", "base")
	base := workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	branch := BranchName(47, "unicode")
	wt, _, err := ProvisionAt(repo, 47, "unicode", base, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	return wt, branch, base
}

func checkpointWrite(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointMessagesFromStagedChanges(t *testing.T) {
	t.Parallel()
	wt, branch, base := checkpointFixture(t)
	wu := &contract.WorkUnit{IssueNumber: 47, Title: "Improve multilingual checkpointing", Objective: "Make commits explain actual changes"}
	ctx := context.Background()
	checkpointWrite(t, wt, "staged.txt", "implementation\n")
	implementHead, err := Checkpoint(ctx, wt, branch, CheckpointOptions{WorkUnit: wu, Purpose: "implement"})
	if err != nil {
		t.Fatal(err)
	}
	if implementHead == base {
		t.Fatal("implementation did not commit")
	}
	msg := workspaceGitOutput(t, wt, "log", "-1", "--format=%B")
	if !strings.Contains(msg, wu.Title) || !strings.Contains(msg, "#47") || strings.Contains(msg, "Objective:") || strings.Contains(msg, "Staged changes") || strings.Contains(msg, "\n") {
		t.Fatalf("concise implementation message: %q", msg)
	}

	// The index, not a plan or a pre-cleanup status, must drive the detailed body.
	checkpointWrite(t, wt, "staged.txt", "staged repair\n")
	runWorkspaceGit(t, wt, "add", "staged.txt")
	checkpointWrite(t, wt, "new file.txt", "new\n")
	if runtime.GOOS != "windows" {
		checkpointWrite(t, wt, "界\nnotes.txt", "unicode path\n")
	}
	if err := os.Remove(filepath.Join(wt, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	checkpointWrite(t, wt, "AGENTS.md", "User instructions\n"+ActiveMarkerStart+"\ntransient instructions\n"+ActiveMarkerEnd+"\n")
	runWorkspaceGit(t, wt, "add", "AGENTS.md")
	repairHead, err := Checkpoint(ctx, wt, branch, CheckpointOptions{WorkUnit: wu, Purpose: "repair", Detail: "detailed"})
	if err != nil {
		t.Fatal(err)
	}
	if repairHead == implementHead {
		t.Fatal("repair did not commit")
	}
	msg = workspaceGitOutput(t, wt, "log", "-1", "--format=%B")
	for _, want := range []string{"Repair validation", "#47", "Objective: " + wu.Objective, "Staged changes", "modified \"staged.txt\"", "added \"new file.txt\"", "deleted \"deleted.txt\""} {
		if !strings.Contains(msg, want) {
			t.Errorf("repair message %q missing %q", msg, want)
		}
	}
	if runtime.GOOS != "windows" && !strings.Contains(msg, "added \"界\\nnotes.txt\"") {
		t.Fatalf("newline filename was not quoted accurately: %q", msg)
	}
	if strings.Contains(msg, "AGENTS.md") || strings.Contains(msg, "transient instructions") {
		t.Fatalf("ephemeral marker leaked into message: %q", msg)
	}
	if again, err := Checkpoint(ctx, wt, branch, CheckpointOptions{WorkUnit: wu, Purpose: "repair", Detail: "detailed"}); err != nil || again != repairHead {
		t.Fatalf("clean rerun head = %s, %v; want %s", again, err, repairHead)
	}
	if got := workspaceGitOutput(t, wt, "rev-list", "--count", base+"..HEAD"); got != "2" {
		t.Fatalf("unexpected checkpoint count: %s", got)
	}

	checkpointWrite(t, wt, "staged.txt", "finalized\n")
	_, err = Checkpoint(ctx, wt, branch, CheckpointOptions{WorkUnit: wu, Purpose: "submit", Detail: "concise"})
	if err != nil {
		t.Fatal(err)
	}
	msg = workspaceGitOutput(t, wt, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "Finalize") || !strings.Contains(msg, "#47") || strings.Contains(msg, "Repair validation") || strings.Contains(msg, "\n") {
		t.Fatalf("submission message: %q", msg)
	}
}

func TestCheckpointSubjectBoundariesAndInvalidOptions(t *testing.T) {
	t.Parallel()
	wt, branch, base := checkpointFixture(t)
	checkpointWrite(t, wt, "AGENTS.md", "User instructions\n"+ActiveMarkerStart+"\ntransient\n"+ActiveMarkerEnd+"\n")
	for _, opts := range []CheckpointOptions{
		{Purpose: "implement"},
		{WorkUnit: &contract.WorkUnit{IssueNumber: 47, Title: "Valid"}, Purpose: "unknown"},
		{WorkUnit: &contract.WorkUnit{IssueNumber: 47, Title: "Valid"}, Purpose: "implement", Detail: "verbose"},
	} {
		if _, err := Checkpoint(context.Background(), wt, branch, opts); err == nil {
			t.Fatalf("accepted invalid options: %+v", opts)
		}
		if got, err := os.ReadFile(filepath.Join(wt, "AGENTS.md")); err != nil || !strings.Contains(string(got), ActiveMarkerStart) {
			t.Fatalf("invalid options mutated markers: %q, %v", got, err)
		}
	}
	wu := &contract.WorkUnit{IssueNumber: 47, Title: "Résumé\n" + strings.Repeat("界", 45) + "\r\nspoofed", Objective: "unicode"}
	checkpointWrite(t, wt, "staged.txt", "updated\n")
	_, err := Checkpoint(context.Background(), wt, branch, CheckpointOptions{WorkUnit: wu, Purpose: "implement"})
	if err != nil {
		t.Fatal(err)
	}
	subject := workspaceGitOutput(t, wt, "log", "-1", "--format=%s")
	if len(subject) > 72 || !utf8.ValidString(subject) || strings.ContainsAny(subject, "\r\n") || !strings.HasSuffix(subject, " (#47)") || !strings.Contains(subject, "Résumé") {
		t.Fatalf("invalid subject: %q", subject)
	}
	if workspaceGitOutput(t, wt, "rev-parse", "HEAD") == base {
		t.Fatal("no checkpoint produced")
	}
}

func TestCheckpointDoesNotCommitCancelledIndexChanges(t *testing.T) {
	t.Parallel()
	wt, branch, base := checkpointFixture(t)
	checkpointWrite(t, wt, "staged.txt", "temporary\n")
	runWorkspaceGit(t, wt, "add", "staged.txt")
	checkpointWrite(t, wt, "staged.txt", "old\n")
	wu := &contract.WorkUnit{IssueNumber: 47, Title: "Cancelled changes"}
	head, err := Checkpoint(context.Background(), wt, branch, CheckpointOptions{WorkUnit: wu, Purpose: "implement", Detail: "detailed"})
	if err != nil || head != base {
		t.Fatalf("cancelled index changes committed: head %s, err %v; want %s", head, err, base)
	}
}
