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
	open       bool
	deny       bool
	dependents []*tracker.Issue
	pr         *tracker.PullRequest
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
	state := "closed"
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
