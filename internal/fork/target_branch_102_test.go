package fork

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type issue102Tracker struct {
	tracker.Tracker
	pr *tracker.PullRequest
}

func (t issue102Tracker) GetPullRequest(context.Context, int) (*tracker.PullRequest, error) {
	return t.pr, nil
}

func TestIssue102CoordinatorRetargetAndDiscovery(t *testing.T) {
	st := &lease.IssueState{ForkRepo: "alice/repo", Branch: "work"}
	pr := &tracker.PullRequest{Number: 7, State: "open", Base: "other", BaseRepo: "upstream/repo", HeadRepo: "alice/repo", Head: "alice:work"}
	c := &Coordinator{RepoRoot: t.TempDir(), UpstreamRepo: "upstream/repo", Tracker: issue102Tracker{pr: pr}, DefaultBaseFunc: func(string) (string, error) { return "release", nil }}
	if _, err := c.verifyForkPR(context.Background(), st, 7); err == nil || !strings.Contains(err.Error(), "release") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("retarget accepted: %v", err)
	}
	pr.Base = "release"
	if _, err := c.verifyForkPR(context.Background(), st, 7); err != nil {
		t.Fatal(err)
	}
	c.DefaultBaseFunc = func(string) (string, error) { return "", fmt.Errorf("discovery failed") }
	if _, err := c.verifyForkPR(context.Background(), st, 7); err == nil || !strings.Contains(err.Error(), "discovery failed") {
		t.Fatalf("discovery bypass: %v", err)
	}
}

func TestIssue102CoordinatorExplicitTargetIgnoresDefault(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=T", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-m", "base"}, {"branch", "release"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}

	st := &lease.IssueState{BaseBranch: "release", ForkRepo: "alice/repo", Branch: "work"}
	pr := &tracker.PullRequest{Number: 7, State: "open", Base: "release", BaseRepo: "upstream/repo", HeadRepo: "alice/repo", Head: "alice:work"}
	c := &Coordinator{RepoRoot: root, UpstreamRepo: "upstream/repo", Tracker: issue102Tracker{pr: pr}, DefaultBaseFunc: func(string) (string, error) { t.Fatal("explicit target rediscovered default"); return "", nil }}
	if _, err := c.verifyForkPR(context.Background(), st, 7); err != nil {
		t.Fatal(err)
	}
	pr.Base = "main"
	if _, err := c.verifyForkPR(context.Background(), st, 7); err == nil || !strings.Contains(err.Error(), "release") {
		t.Fatalf("retarget accepted: %v", err)
	}
}

func TestIssue102ForkFreshAcquireAndFence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "T")
	git("config", "user.email", "t@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "policy")
	git("checkout", "-b", "release")
	git("commit", "--allow-empty", "-m", "distinct release")
	target := git("rev-parse", "HEAD")
	git("checkout", "main")
	path := filepath.Join(root, ".swarm/config.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("base_branch: release\n"), b...), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "trusted selection")
	remote := filepath.Join(t.TempDir(), "upstream.git")
	git("init", "--bare", "--initial-branch=main", remote)
	git("remote", "add", "origin", remote)
	git("push", "origin", "main", "release")
	gh := newFakeGitHubState()
	gh.baseSHA = target
	gh.issues[42] = &tracker.Issue{Number: 42, Title: "release task", Body: "```yaml swarm-workunit\nid: ISSUE-42\nobjective: release task\nconstraints: [Keep policy]\nacceptance: [Correct branch]\nworkflow: feature\ngate: none\n```", State: "open", Author: "maintainer"}
	maint := gh.trackerForUser(t, "maintainer", "User")
	mgr := lease.NewManager(maint, contract.SecurityConfig{})
	if err := mgr.ConfigureStore(root); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return now })
	approved, err := mgr.ApproveTask(ctx, 42, "maintainer")
	if err != nil {
		t.Fatal(err)
	}
	if approved.BaseBranch != "release" || approved.BaseSHA != target {
		t.Fatalf("wrong approval: %+v", approved)
	}
	c := &Coordinator{RepoRoot: root, UpstreamRepo: "octo/swarm", Tracker: gh.trackerForUser(t, ActionsBotLogin, "Bot"), NowFn: func() time.Time { return now }}
	req := lease.ForkRequest{Version: 1, Operation: "acquire", IssueNumber: 42, ForkRepo: "alice/swarm", Agent: "claude", Phase: "all", Duration: "1h"}
	first, _, err := c.executeRequest(ctx, 1001, "alice", req)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := harness.LoadConfigAt(root, target)
	if err != nil {
		t.Fatal(err)
	}
	wu, err := contract.ParseWorkUnitFromIssue(42, gh.issues[42].Title, gh.issues[42].Body)
	if err != nil {
		t.Fatal(err)
	}
	if first.BaseBranch != "release" || first.BaseSHA != target || !lease.TaskApprovalCurrent(first, wu, cfg) {
		t.Fatalf("fork did not retain target approval: %+v", first)
	}
	now = now.Add(2 * time.Hour)
	second, _, err := c.executeRequest(ctx, 1002, "alice", req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Lease.ClaimID == first.Lease.ClaimID {
		t.Fatal("expired claim reused")
	}
	before, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.executeRequest(ctx, 1003, "alice", lease.ForkRequest{Version: 1, Operation: "update-artifact", IssueNumber: 42, ClaimID: first.Lease.ClaimID, Artifact: &contract.Artifact{Type: contract.ArtifactDesign, Body: "stale"}})
	if err == nil || !strings.Contains(err.Error(), lease.ErrClaimFence.Error()) {
		t.Fatalf("stale worker accepted: %v", err)
	}
	after, err := mgr.GetCurrentState(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(before) {
		t.Fatal("stale worker changed successor state")
	}
}
