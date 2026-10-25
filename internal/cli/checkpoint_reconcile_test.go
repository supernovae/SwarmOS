package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

type checkpointCLITracker struct{ tracker.Tracker }

func (checkpointCLITracker) BackendName() string { return "github-checkpoint-fixture" }

func TestCLICheckpointReconciliationAndStatus(t *testing.T) {
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	ctx := context.Background()
	number := strconv.Itoa(n)
	remote := filepath.Join(t.TempDir(), "remote.git")
	prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
	prGit(t, f.root, "remote", "add", "origin", remote)
	prGit(t, f.root, "push", "origin", "main")
	tr := checkpointCLITracker{f.tracker}
	cfg, err := os.ReadFile(filepath.Join(f.root, ".swarm/config.yaml"))
	if err != nil || len(cfg) == 0 {
		t.Fatal(err)
	}
	f.manager.SetStore(f.root, coordination.NewGitStore(f.root))
	if _, err = f.manager.ApproveTask(ctx, n, "alice"); err != nil {
		t.Fatal(err)
	}
	st, err := f.manager.Acquire(ctx, n, "alice", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.SaveClaimToken(f.root, n, st.Lease.ClaimID, st.BaseSHA); err != nil {
		t.Fatal(err)
	}
	path, branch, err := workspace.ProvisionAt(f.root, n, "test", st.BaseSHA, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "feature.txt"), []byte("feature"), 0644); err != nil {
		t.Fatal(err)
	}
	prGit(t, path, "add", "feature.txt")
	prGit(t, path, "commit", "-m", "A")
	a := prGit(t, path, "rev-parse", "HEAD")
	for _, art := range []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan", Producer: "mock", Summary: "plan"}, {Type: contract.ArtifactTestPlan, Body: "tests", Producer: "mock", Summary: "tests"}} {
		if _, err = f.manager.UpdateArtifact(ctx, n, "alice", art); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.manager.RecordHumanReview(ctx, n, "APPROVED", "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.RecordCheckpoint(ctx, n, "alice", branch, a); err != nil {
		t.Fatal(err)
	}
	prGit(t, path, "commit", "--amend", "-m", "B")
	b := prGit(t, path, "rev-parse", "HEAD")
	run := func(args ...string) (string, error) {
		opts := &RootOptions{RepoRoot: f.root, Repo: "test/repo", Backend: "github", trackerOverride: tr}
		cmd := newRootCmdWithOptions(opts)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(ctx)
		return out.String(), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("swarm %v: %v %s", args, err, out)
		}
		return out
	}
	raw := must("issue", "status", number, "--json")
	var status struct {
		Issues []struct {
			Checkpoint *workspace.CheckpointState `json:"checkpoint"`
		} `json:"issues"`
	}
	if err = json.Unmarshal([]byte(raw), &status); err != nil || len(status.Issues) != 1 || status.Issues[0].Checkpoint.Worktree.SHA != b || status.Issues[0].Checkpoint.Recorded.SHA != a {
		t.Fatalf("status omitted divergence: %s %v", raw, err)
	}
	text := must("issue", "status", number)
	if !strings.Contains(text, a) || !strings.Contains(text, b) {
		t.Fatal("text status omits exact heads")
	}
	args := []string{"issue", "reconcile", number, "--checkpoint", "--expected-head", a, "--candidate-head", b}
	refs := prGit(t, remote, "show-ref")
	must(append(append([]string{}, args...), "--dry-run", "--json")...)
	if prGit(t, remote, "show-ref") != refs {
		t.Fatal("dry run changed refs")
	}
	must(append(append([]string{}, args...), "--json")...)
	current, err := f.manager.GetCurrentState(ctx, n)
	if err != nil || current.HeadSHA != b {
		t.Fatalf("not reconciled: %+v %v", current, err)
	}
	if _, _, err = workspace.ProvisionAt(f.root, n, "test", st.BaseSHA, branch, b); err != nil {
		t.Fatal(err)
	}
	if prGit(t, remote, "rev-parse", coordination.CheckpointRef(n, a)) != a {
		t.Fatal("lost predecessor")
	}
	// Missing local worktrees do not hide authoritative status.
	prGit(t, f.root, "worktree", "remove", path)
	raw = must("issue", "status", number, "--json")
	if !strings.Contains(raw, `"missing":true`) || !strings.Contains(raw, b) {
		t.Fatal("missing worktree hides state")
	}
}

func TestCheckpointReconcileFlags(t *testing.T) {
	f := newCLIFixture(t)
	help := f.must(t, "issue", "reconcile", "--help")
	for _, flag := range []string{"--checkpoint", "--expected-head", "--candidate-head", "--dry-run"} {
		if !strings.Contains(help, flag) {
			t.Fatalf("missing flag %s", flag)
		}
	}
	for _, flags := range [][]string{{"--dry-run"}, {"--expected-head", strings.Repeat("a", 40)}, {"--checkpoint"}, {"--candidate-head", strings.Repeat("b", 40)}} {
		if _, err := f.run(append([]string{"issue", "reconcile", "1"}, flags...)...); err == nil {
			t.Fatalf("accepted invalid flags %v", flags)
		}
	}
}
