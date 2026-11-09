package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
)

type archiveTracker struct {
	tracker.Tracker
	open        bool
	deny        bool
	dependents  []*tracker.Issue
	pr          *tracker.PullRequest
	failRecheck int
	calls       map[int]int
	onGet       func(int)
}

func (t *archiveTracker) BackendName() string                         { return "github" }
func (t *archiveTracker) UpstreamRepo() string                        { return "octo/swarm" }
func (t *archiveTracker) CurrentUser(context.Context) (string, error) { return "maintainer", nil }
func (t *archiveTracker) Authorize(context.Context, string) error {
	if t.deny {
		return errors.New("denied")
	}
	return nil
}
func (t *archiveTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	if t.onGet != nil {
		t.onGet(n)
	}
	state := "closed"
	if t.calls != nil {
		t.calls[n]++
		if n == t.failRecheck && t.calls[n] == 2 {
			state = "open"
		}
	}
	if t.open {
		state = "open"
	}
	return &tracker.Issue{Number: n, State: state}, nil
}
func (t *archiveTracker) ListIssues(context.Context, tracker.IssueFilter) ([]*tracker.Issue, error) {
	return t.dependents, nil
}
func (t *archiveTracker) GetPullRequest(context.Context, int) (*tracker.PullRequest, error) {
	return t.pr, nil
}
func archiveManagerFixture(t *testing.T) (*Manager, *archiveTracker, int, string, string) {
	t.Helper()
	m, tr, n, remote := mergedCleanupFixture(t)
	wrapped := &archiveTracker{Tracker: tr, pr: tr.pr}
	m.tracker = wrapped
	st, rev, err := m.load(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	st.Lease.Status = "completed"
	st.PRRevision = nil
	st.Cleanup = nil
	raw, _ := json.Marshal(st)
	if _, err = m.store.CompareAndSwap(context.Background(), n, rev, raw); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	run("remote", "set-url", "origin", "file://"+remote)
	run("push", "origin", ":refs/heads/"+st.Branch)
	dest := filepath.Join(t.TempDir(), "archive.git")
	run("init", "--bare", dest)
	return m, wrapped, n, remote, "file://" + dest
}
func TestArchiveEligibility(t *testing.T) {
	for _, blocker := range []string{"none", "closed-pr", "open-issue", "active", "handoff", "revision", "cleanup", "open-pr", "task-branch", "contributor-index", "dependent", "invalid-dependent", "authorization"} {
		t.Run(blocker, func(t *testing.T) {
			m, tr, n, remote, dest := archiveManagerFixture(t)
			ctx := context.Background()
			st, rev, err := m.load(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			run := func(args ...string) {
				t.Helper()
				if out, e := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput(); e != nil {
					t.Fatalf("git: %s %v", out, e)
				}
			}
			switch blocker {
			case "closed-pr":
				copy := *tr.pr
				copy.Merged = false
				copy.State = "closed"
				tr.pr = &copy
			case "open-issue":
				tr.open = true
			case "active", "handoff":
				st.Lease.Status = blocker
			case "revision":
				st.PRRevision = &PRRevision{}
			case "cleanup":
				st.Cleanup = &CleanupReservation{}
			case "open-pr":
				copy := *tr.pr
				copy.State = "open"
				copy.Merged = false
				tr.pr = &copy
			case "task-branch":
				run("push", "origin", st.HeadSHA+":refs/heads/"+st.Branch)
			case "contributor-index":
				path := filepath.Join(m.repoRoot, "index.json")
				if e := os.WriteFile(path, []byte(`{"version":1,"actor":"alice","claims":[{"issue_number":1,"claim_id":"test"}]}`), 0600); e != nil {
					t.Fatal(e)
				}
				run("add", "index.json")
				run("commit", "-m", "contributor fixture")
				run("push", "origin", "HEAD:"+m.LedgerLayout().ContributorRef("alice"))
			case "dependent", "invalid-dependent":
				body := contract.FormatIssueMarkdown(&contract.WorkUnit{Objective: "dependent", Constraints: []string{}, Acceptance: []string{"done"}, Workflow: "feature", DependsOn: []int{n}})
				if blocker == "invalid-dependent" {
					body = "invalid"
				}
				tr.dependents = []*tracker.Issue{{Number: 2, State: "open", Labels: []string{"swarm:ready"}, Body: body}}
			case "authorization":
				tr.deny = true
			}
			raw, _ := json.Marshal(st)
			if _, err = m.store.CompareAndSwap(ctx, n, rev, raw); err != nil {
				t.Fatal(err)
			}
			inventory := func() string {
				out, e := gitfixture.Command(t, "", "--git-dir="+remote, "for-each-ref", "--format=%(refname) %(objectname)").CombinedOutput()
				if e != nil {
					t.Fatal(e)
				}
				return string(out)
			}
			before := inventory()
			report, err := m.ArchiveIssue(ctx, n, dest, false)
			if blocker == "none" || blocker == "closed-pr" {
				if err != nil || len(report.Checks) != 8 {
					t.Fatalf("control: %+v %v", report, err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s: %+v", blocker, report)
			}
			if inventory() != before {
				t.Fatal("preview changed source")
			}
		})
	}
}
func TestArchivedManagerAdmission(t *testing.T) {
	m, tr, n, remote, dest := archiveManagerFixture(t)
	ctx := context.Background()
	if _, err := m.ArchiveIssue(ctx, n, dest, true); err != nil {
		t.Fatal(err)
	}
	tr.open = true // A reopened tracker issue must never become a new task.
	inventory := func() string {
		out, err := gitfixture.Command(t, "", "--git-dir="+remote, "for-each-ref", "--format=%(refname) %(objectname)").CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	before := inventory()
	var archived *coordination.ArchivedError
	if _, _, err := m.load(ctx, n); !errors.As(err, &archived) || !strings.Contains(err.Error(), "swarm repo archive restore") {
		t.Fatalf("load archived: %v", err)
	}
	if _, err := m.store.CompareAndSwap(ctx, n, "", []byte(`{}`)); !errors.As(err, &archived) {
		t.Fatalf("new state: %v", err)
	}
	checks := map[string]func() error{
		"checkpoint": func() error {
			_, e := m.RecordCheckpoint(ctx, n, "maintainer", fmt.Sprintf("swarm/issue-%d-test", n), strings.Repeat("a", 40))
			return e
		},
		"submit": func() error {
			_, e := m.RecordSubmission(ctx, n, "maintainer", 25, "https://github.com/octo/swarm/pull/25")
			return e
		},
		"approve": func() error { _, e := m.ApproveTask(ctx, n, "maintainer"); return e },
		"claim":   func() error { _, e := m.Acquire(ctx, n, "maintainer", "codex", "plan", time.Minute, 0); return e },
		"fork": func() error {
			_, e := m.AcquireFork(ctx, n, "maintainer", "codex", "plan", "maintainer/swarm", time.Minute, 0)
			return e
		},
		"pr": func() error {
			_, e := m.BeginPRRevision(ctx, n, "maintainer", 25, strings.Repeat("a", 40), strings.Repeat("b", 40), "revision")
			return e
		},
	}
	for name, check := range checks {
		if e := check(); !errors.As(e, &archived) || !strings.Contains(e.Error(), fmt.Sprintf("swarm repo archive restore %d", n)) {
			t.Errorf("%s: %v", name, e)
		}
	}
	if inventory() != before {
		t.Fatal("admission changed remote refs")
	}
	if _, err := m.RestoreIssueArchive(ctx, n, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.load(ctx, n); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveWorkerRefusal(t *testing.T) {
	m, _, n, _, dest := archiveManagerFixture(t)
	m.SetWorkerGateway(&refusingGateway{t: t})
	for _, apply := range []bool{false, true} {
		if _, err := m.ArchiveIssue(context.Background(), n, dest, apply); err == nil {
			t.Fatal("worker archived issue")
		}
		if _, err := m.RestoreIssueArchive(context.Background(), n, apply); err == nil {
			t.Fatal("worker restored issue")
		}
	}
}

func TestArchiveBulkLaterClockRetry(t *testing.T) {
	for _, layout := range []coordination.RefLayout{coordination.BranchLayout, coordination.HiddenLayout} {
		for _, failure := range []string{"recheck", "delete", "copy"} {
			t.Run(layout.Name()+"/"+failure, func(t *testing.T) {
				m, tr, n, remote, dest := archiveManagerFixture(t)
				ctx := context.Background()
				git := func(dir string, args ...string) string {
					t.Helper()
					out, e := gitfixture.Command(t, dir, args...).CombinedOutput()
					if e != nil {
						t.Fatalf("git %v: %s %v", args, out, e)
					}
					return strings.TrimSpace(string(out))
				}
				old := m.store.(*coordination.GitStore)
				if layout == coordination.HiddenLayout {
					refs, _, e := old.ArchiveSnapshot(ctx, n)
					if e != nil {
						t.Fatal(e)
					}
					for _, r := range refs {
						next := strings.Replace(r.Source, "refs/heads/swarm-state/", "refs/swarm/state/", 1)
						next = strings.Replace(next, "refs/heads/swarm-checkpoints/", "refs/swarm/checkpoints/", 1)
						git(remote, "update-ref", next, r.SHA)
						git(remote, "update-ref", "-d", r.Source)
					}
					indexTip := git(remote, "rev-parse", coordination.BranchLayout.ArchiveRef())
					git(remote, "update-ref", layout.ArchiveRef(), indexTip)
					git(remote, "update-ref", "-d", coordination.BranchLayout.ArchiveRef())
					m.SetStore(m.repoRoot, coordination.NewGitStoreWithLayout(m.repoRoot, "origin", layout))
				}
				store := m.store.(*coordination.GitStore)
				head := git(m.repoRoot, "rev-parse", "HEAD")
				git(remote, "update-ref", "refs/heads/unrelated", head)
				for _, id := range []int{n + 1, n + 2} {
					raw, _ := json.Marshal(IssueState{Lease: contract.Lease{IssueNumber: id, Status: "completed"}})
					if _, e := store.CompareAndSwapRetaining(ctx, id, "", raw, head); e != nil {
						t.Fatal(e)
					}
				}
				n++
				refs, tip, e := store.ArchiveSnapshot(ctx, n)
				if e != nil {
					t.Fatal(e)
				}
				stamp, e := store.ArchiveCommitTime(ctx, tip)
				if e != nil {
					t.Fatal(e)
				}
				now := stamp.Add(90 * 24 * time.Hour)
				m.SetNowFunc(func() time.Time { return now })
				days := 30
				policy := &contract.ArchiveConfig{Strategy: "repo", AfterDays: &days, Repo: "acme/archive"}
				tr.calls = map[int]int{}
				hook := ""
				if failure == "recheck" {
					tr.failRecheck = n
				} else {
					dir := remote
					condition := fmt.Sprintf(`[ "$ref" = "%s" ] && [ "$new" = "%s" ]`, layout.StateRef(n), strings.Repeat("0", 40))
					if failure == "copy" {
						dir = strings.TrimPrefix(dest, "file://")
						condition = fmt.Sprintf(`[ "$ref" = "refs/archive/issue-%d/state" ]`, n)
					}
					hook = filepath.Join(dir, "hooks", "pre-receive")
					body := "#!/bin/sh\nwhile read old new ref; do\nif " + condition + "; then exit 1; fi\ndone\n"
					if e = os.WriteFile(hook, []byte(body), 0700); e != nil {
						t.Fatal(e)
					}
				}
				r, e := m.ArchiveAllEligible(ctx, policy, dest, true)
				if e == nil || r.Totals.Archived != 2 || r.Totals.Failed != 1 {
					t.Fatalf("%+v %v", r, e)
				}
				for _, ref := range refs {
					if got := git(remote, "rev-parse", ref.Source); got != ref.SHA {
						t.Fatal("failed issue source changed")
					}
				}
				if _, e = store.ArchiveTombstone(ctx, n, true); !errors.Is(e, coordination.ErrNotFound) {
					t.Fatalf("failed issue has tombstone: %v", e)
				}
				destDir := strings.TrimPrefix(dest, "file://")
				manifestRef := fmt.Sprintf("refs/archive/issue-%d/manifest", n)
				var body, commit string
				if failure != "copy" {
					commit = git(destDir, "rev-parse", manifestRef)
					body = git(destDir, "show", commit+":manifest.json")
				}
				if hook != "" {
					if e = os.Remove(hook); e != nil {
						t.Fatal(e)
					}
				}
				tr.failRecheck = 0
				originalNow := now
				now = stamp.Add(10 * 24 * time.Hour)
				young, youngErr := m.ArchiveAllEligible(ctx, policy, dest, true)
				if youngErr != nil || young.Totals.Skipped != 1 || young.Totals.Archived != 0 {
					t.Fatalf("stricter current cutoff: %+v %v", young, youngErr)
				}
				now = originalNow.Add(24 * time.Hour)
				r, e = m.ArchiveAllEligible(ctx, policy, dest, true)
				if e != nil || r.Totals.Archived != 1 || r.Totals.Failed != 0 {
					t.Fatalf("retry %+v %v", r, e)
				}
				tomb, e := store.ArchiveTombstone(ctx, n, false)
				if e != nil {
					t.Fatal(e)
				}
				data, digest, e := contract.MarshalArchiveManifest(&tomb.Manifest)
				if e != nil {
					t.Fatal(e)
				}
				if digest != tomb.Digest {
					t.Fatal("tombstone digest differs")
				}
				if failure != "copy" {
					if string(data) != body || git(destDir, "rev-parse", manifestRef) != commit {
						t.Fatal("retry rewrote destination manifest")
					}
					if tomb.Manifest.Cutoff.Equal(*r.Cutoff) {
						t.Fatal("retained audit cutoff overwritten")
					}
				}
				if got := git(remote, "for-each-ref", "--format=%(refname)", layout.StateRef(n)); got != "" {
					t.Fatal("source state retained after success")
				}
				if got := git(remote, "rev-parse", "refs/heads/unrelated"); got == "" {
					t.Fatal("unrelated ref removed")
				}
			})
		}
	}
}

func TestArchiveBulkCurrentTipAgeRecheck(t *testing.T) {
	m, tr, n, remote, dest := archiveManagerFixture(t)
	ctx := context.Background()
	store := m.store.(*coordination.GitStore)
	refs, tip, e := store.ArchiveSnapshot(ctx, n)
	if e != nil {
		t.Fatal(e)
	}
	stamp, e := store.ArchiveCommitTime(ctx, tip)
	if e != nil {
		t.Fatal(e)
	}
	now := stamp.Add(90 * 24 * time.Hour)
	m.SetNowFunc(func() time.Time { return now })
	count := 0
	next := ""
	tr.onGet = func(id int) {
		if id != n {
			return
		}
		count++
		if count != 2 {
			return
		}
		tree := strings.TrimSpace(gitfixture.Run(t, m.repoRoot, "rev-parse", tip+"^{tree}"))
		cmd := gitfixture.Command(t, m.repoRoot, "commit-tree", tree, "-p", tip, "-m", "recent state")
		cmd.Env = append(cmd.Env, "GIT_COMMITTER_DATE="+now.Format(time.RFC3339))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v", out, err)
		}
		next = strings.TrimSpace(string(out))
		gitfixture.Run(t, m.repoRoot, "push", "origin", next+":"+store.Layout().StateRef(n))
	}
	days := 30
	r, e := m.ArchiveAllEligible(ctx, &contract.ArchiveConfig{Strategy: "repo", AfterDays: &days, Repo: "acme/archive"}, dest, true)
	if e == nil || r.Totals.Failed != 1 || r.Totals.Skipped != 0 || !strings.Contains(e.Error(), "age:") {
		t.Fatalf("%+v %v", r, e)
	}
	for _, ref := range refs {
		expected := ref.SHA
		if ref.Source == store.Layout().StateRef(n) {
			expected = next
		}
		got := strings.TrimSpace(gitfixture.Run(t, remote, "rev-parse", ref.Source))
		if got != expected {
			t.Fatal("source refs lost after age recheck")
		}
	}
	if _, e = store.ArchiveTombstone(ctx, n, true); !errors.Is(e, coordination.ErrNotFound) {
		t.Fatalf("failed recheck published tombstone: %v", e)
	}
}
