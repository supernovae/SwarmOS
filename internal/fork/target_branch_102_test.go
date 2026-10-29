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

// Counts publication/projection writes and injects retargets on a selected observation.
type issue102ObservedTracker struct {
	tracker.Tracker
	reads, retargetAt, writes int
	prs                       map[int]*tracker.PullRequest
}

func (tr *issue102ObservedTracker) GetPullRequest(ctx context.Context, n int) (*tracker.PullRequest, error) {
	var pr *tracker.PullRequest
	var err error
	if tr.prs == nil {
		pr, err = tr.Tracker.GetPullRequest(ctx, n)
	}
	if tr.prs != nil {
		cp := *tr.prs[n]
		cp.State = "open"
		pr, err = &cp, nil
	}
	tr.reads++
	if err == nil && tr.reads == tr.retargetAt {
		cp := *pr
		cp.Base = "main"
		return &cp, nil
	}
	return pr, err
}
func (tr *issue102ObservedTracker) UpdateDraftPR(ctx context.Context, n int, title, body string) (*tracker.PullRequest, error) {
	tr.writes++
	return tr.Tracker.UpdateDraftPR(ctx, n, title, body)
}
func (tr *issue102ObservedTracker) CreateComment(ctx context.Context, n int, body string) (*tracker.Comment, error) {
	tr.writes++
	return tr.Tracker.CreateComment(ctx, n, body)
}
func (tr *issue102ObservedTracker) UpdateComment(ctx context.Context, n int64, body string) (*tracker.Comment, error) {
	tr.writes++
	return tr.Tracker.UpdateComment(ctx, n, body)
}
func TestIssue102ForkExplicitReleaseLifecycle(t *testing.T) {
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
	// The contributor's default branch contains a conflicting target selector.
	forkRemote := filepath.Join(t.TempDir(), "fork.git")
	git("clone", "--bare", remote, forkRemote)
	git("checkout", "-b", "fork-default", "main")
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), "base_branch: release", "base_branch: main", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	git("commit", "-am", "conflicting fork selector")
	git("push", forkRemote, "HEAD:refs/heads/main")
	git("checkout", "main")
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

	observed := &issue102ObservedTracker{Tracker: c.Tracker, prs: gh.prs}
	verifier := c.Tracker.(*tracker.GitHubTracker)
	c.VerifyForkFunc = func(ctx context.Context, repo, owner string) (string, error) {
		return verifier.VerifyForkRepository(ctx, repo, owner, false)
	}
	c.Tracker = observed
	c.DefaultBaseFunc = func(string) (string, error) { t.Fatal("explicit release consulted default"); return "", nil }
	c.ForkGitURLFunc = func(string) string { return forkRemote }
	id := int64(1100)
	apply := func(req lease.ForkRequest) (*lease.IssueState, error) {
		id++
		req.Version = 1
		req.IssueNumber = 42
		if req.Operation != "acquire-pr" {
			req.ClaimID = first.Lease.ClaimID
		}
		st, _, err := c.executeRequest(ctx, id, "alice", req)
		return st, err
	}
	must := func(req lease.ForkRequest) *lease.IssueState {
		t.Helper()
		st, err := apply(req)
		if err != nil {
			t.Fatalf("%s: %v", req.Operation, err)
		}
		return st
	}
	branch := fmt.Sprintf("swarm/issue-42-%s", first.Lease.ClaimID)
	git("checkout", "-b", branch, target)
	git("commit", "--allow-empty", "-m", "release work")
	head := git("rev-parse", "HEAD")
	git("push", forkRemote, "HEAD:refs/heads/"+branch)
	must(lease.ForkRequest{Operation: "record-checkpoint", Branch: branch, HeadSHA: head})
	created, err := observed.Tracker.CreateDraftPR(ctx, tracker.DraftPRRequest{IssueNumber: 42, Title: "Release work", Head: "alice:" + branch, Base: "release"})
	if err != nil {
		t.Fatal(err)
	}
	gh.prs[created.Number].HeadSHA = head
	if created.Base != "release" || gh.prs[created.Number].Base != "release" || git("--git-dir="+forkRemote, "symbolic-ref", "HEAD") != "refs/heads/main" || git("--git-dir="+remote, "symbolic-ref", "HEAD") != "refs/heads/main" {
		t.Fatal("publication followed default")
	}
	// Every refusal compares durable state, remote refs and tracker write counters.
	refuse := func(req lease.ForkRequest, at int, expectedWrites int) {
		t.Helper()
		before, err := mgr.GetCurrentState(ctx, 42)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(before)
		refs := git("ls-remote", remote)
		forkRefs := git("ls-remote", forkRemote)
		observed.reads = 0
		observed.retargetAt = at
		writes := observed.writes
		_, err = apply(req)
		if err == nil || !strings.Contains(err.Error(), "release") {
			t.Fatalf("%s retarget accepted: %v", req.Operation, err)
		}
		after, e := mgr.GetCurrentState(ctx, 42)
		if e != nil {
			t.Fatal(e)
		}
		got, _ := json.Marshal(after)
		if string(raw) != string(got) || refs != git("ls-remote", remote) || forkRefs != git("ls-remote", forkRemote) || observed.writes-writes != expectedWrites {
			t.Fatalf("%s refusal mutated state/refs or unexpected projection writes: %d", req.Operation, observed.writes-writes)
		}
		if observed.reads < at {
			t.Fatalf("late read not reached: %d", observed.reads)
		}
		observed.retargetAt = 0
	}

	approvePlan := func() {
		t.Helper()
		for _, typ := range []contract.ArtifactType{contract.ArtifactDesign, contract.ArtifactTestPlan} {
			art := contract.Artifact{Type: typ, Producer: "claude:planner", Summary: "Release plan", Body: "release plan " + string(typ)}
			must(lease.ForkRequest{Operation: "update-artifact", Artifact: &art})
		}
		if _, err := mgr.RecordHumanReview(ctx, 42, "APPROVED", "Reviewed release plan"); err != nil {
			t.Fatal(err)
		}
	}
	approvePlan()
	submission := lease.ForkRequest{Operation: "record-submission", PRNumber: created.Number}
	refuse(submission, 1, 0)
	submitted := must(submission)
	if submitted.BaseBranch != "release" || submitted.BaseSHA != target || submitted.Lease.Status != "completed" {
		t.Fatalf("wrong submission: %+v", submitted)
	}
	acquire := lease.ForkRequest{Operation: "acquire-pr", PRNumber: created.Number, Agent: "claude"}
	refuse(acquire, 1, 0)
	first = must(acquire)
	begin := lease.ForkRequest{Operation: "begin-pr-revision", PRNumber: created.Number, Branch: fmt.Sprintf("swarm/pr-%d", created.Number), HeadSHA: head, ExpectedHeadSHA: head}
	refuse(begin, 1, 0)
	must(begin)
	git("commit", "--allow-empty", "-m", "revision")
	revised := git("rev-parse", "HEAD")
	record := lease.ForkRequest{Operation: "record-pr-head", PRNumber: created.Number, Branch: begin.Branch, HeadSHA: revised}
	refuse(record, 1, 0)
	must(record)
	complete := lease.ForkRequest{Operation: "complete-pr-revision", PRNumber: created.Number, ExpectedHeadSHA: head, HeadSHA: revised}
	// A local revision cannot complete before publication.
	if _, err := apply(complete); err == nil || !strings.Contains(err.Error(), "not the validated revision") {
		t.Fatalf("unpublished completion: %v", err)
	}
	git("push", forkRemote, "HEAD:refs/heads/"+branch)
	gh.prs[created.Number].HeadSHA = revised
	approvePlan()
	validation := contract.Artifact{Type: contract.ArtifactValidation, Producer: "swarm:validator", Verdict: "PASS", Summary: "Release validation", Body: "release validation", Metadata: map[string]string{"head_sha": revised}}
	must(lease.ForkRequest{Operation: "update-artifact", Artifact: &validation})
	refuse(complete, 1, 0)
	// The third observation follows the projection read and write, before CAS completion.
	refuse(complete, 3, 1)
	done := must(complete)
	if done.PRRevision != nil || done.Lease.Status != "completed" || done.HeadSHA != revised || done.BaseBranch != "release" {
		t.Fatalf("wrong completion: %+v", done)
	}
}
