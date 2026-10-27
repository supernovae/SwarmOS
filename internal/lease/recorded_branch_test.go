package lease

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestAcquireRecordedBranchSurvivesTitleEdit(t *testing.T) {
	ctx := context.Background()
	m, lt, issue := testManager(t)
	first, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	original := workspace.BranchName(issue.Number, issue.Title)
	if first.Branch != original || first.HeadSHA != "" {
		t.Fatalf("first claim reserved %q head %q, want %q", first.Branch, first.HeadSHA, original)
	}
	if _, err = m.Release(ctx, issue.Number, "local-dev", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = lt.UpdateIssueTitle(ctx, issue.Number, "A completely different title"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err == nil {
		t.Fatal("title edit must require reapproval")
	}
	if _, err = m.ApproveTask(ctx, issue.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	again, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if again.Branch != original {
		t.Fatalf("reclaim after title edit used %q, want %q", again.Branch, original)
	}
	_, branch := workspace.ClaimCheckpointFrom("", false, "", issue.Number, "A completely different title", again.Lease.ClaimID, again.Branch)
	if branch != original {
		t.Fatalf("provisioning selects %q, want %q", branch, original)
	}
}

func TestAcquireRecordedBranchNotSavedOnCASFailure(t *testing.T) {
	ctx := context.Background()
	m, _, issue := testManager(t)
	m.store = &claimRaceStore{Store: m.store}
	if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err == nil {
		t.Fatal("expected lost race")
	}
	got, _, err := m.load(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "" || got.Lease.Status == "active" {
		t.Fatalf("failed claim saved reservation %q", got.Branch)
	}
}

func TestForkAcquireAfterRecordedBranchUsesForkBranchName(t *testing.T) {
	ctx := context.Background()
	m, _, issue := testManager(t)
	same, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if same.Branch == "" {
		t.Fatal("same-repository claim did not reserve a branch")
	}
	if _, err = m.Release(ctx, issue.Number, "local-dev", "test"); err != nil {
		t.Fatal(err)
	}
	fork, err := m.AcquireFork(ctx, issue.Number, "local-dev", "mock", "plan", "local-dev/swarm", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fork.Branch != "" {
		t.Fatalf("fork claim kept same-repository reservation %q", fork.Branch)
	}
	_, branch := workspace.ClaimCheckpointFrom("", true, "fork", issue.Number, issue.Title, fork.Lease.ClaimID, fork.Branch)
	if want := workspace.ForkBranchName(issue.Number, fork.Lease.ClaimID); branch != want {
		t.Fatalf("fork provisioning selects %q, want %q", branch, want)
	}
}

// A checkpoint contained in a reapproved base keeps its recovery branch even
// when the title changed in between; a title-derived name would fail publication.
func TestAcquireRecordedBranchUsesDroppedCheckpoint(t *testing.T) {
	ctx := context.Background()
	m, lt, issue := testManager(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = m.repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	git("init", "--bare", "--initial-branch=main", remote)
	git("remote", "add", "origin", remote)
	main := git("symbolic-ref", "--short", "HEAD")
	git("--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/"+main)
	git("push", "origin", main)
	m.SetStore(m.repoRoot, coordination.NewGitStore(m.repoRoot))
	if _, err := m.ApproveTask(ctx, issue.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	st, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	old := st.BaseSHA
	_, branch, err := workspace.ProvisionAt(m.repoRoot, issue.Number, issue.Title, old, st.Branch, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.RecordCheckpoint(ctx, issue.Number, "local-dev", branch, old); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Release(ctx, issue.Number, "local-dev", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = lt.UpdateIssueTitle(ctx, issue.Number, "Renamed task"); err != nil {
		t.Fatal(err)
	}
	git("-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "new base")
	base := git("rev-parse", "HEAD")
	git("push", "origin", main)
	approved, err := m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if approved.DroppedCheckpoint == nil || approved.DroppedCheckpoint.Branch != branch || approved.Branch != "" {
		t.Fatalf("approval did not retain recovery identity: %+v", approved.DroppedCheckpoint)
	}
	claimed, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Branch != branch || claimed.DroppedCheckpoint == nil {
		t.Fatalf("claim reserved %q, want recovery branch %q", claimed.Branch, branch)
	}
	if claimed.Branch == workspace.BranchName(issue.Number, "Renamed task") {
		t.Fatal("claim reserved a title-derived branch")
	}
	result, err := m.RecordCheckpoint(ctx, issue.Number, "local-dev", claimed.Branch, base)
	if err != nil {
		t.Fatal(err)
	}
	if result.DroppedCheckpoint != nil || result.HeadSHA != base || result.Branch != branch {
		t.Fatal("recovery not consumed on the original branch")
	}
}
