package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

type archiveCLITracker struct {
	tracker.Tracker
	denied bool
}

func (t *archiveCLITracker) BackendName() string                         { return "github" }
func (t *archiveCLITracker) UpstreamRepo() string                        { return "octo/swarm" }
func (t *archiveCLITracker) CurrentUser(context.Context) (string, error) { return "maintainer", nil }
func (t *archiveCLITracker) Authorize(context.Context, string) error {
	if t.denied {
		return fmt.Errorf("denied")
	}
	return nil
}
func (t *archiveCLITracker) GetIssue(_ context.Context, n int) (*tracker.Issue, error) {
	return &tracker.Issue{Number: n, State: "closed"}, nil
}
func (t *archiveCLITracker) ListIssues(context.Context, tracker.IssueFilter) ([]*tracker.Issue, error) {
	return nil, nil
}
func TestRepoArchivePreviewApplyRestore(t *testing.T) {
	for _, ns := range []string{"branch", "hidden"} {
		t.Run(ns, func(t *testing.T) {
			f := newLedgerCLIFixture(t, ns)
			setFixtureTracker(t, f.root, "github")
			layout, err := coordination.LayoutFor(ns)
			if err != nil {
				t.Fatal(err)
			}
			s := coordination.NewGitStoreWithLayout(f.root, "origin", layout)
			st := lease.IssueState{}
			st.Lease.IssueNumber = 363
			st.Lease.Status = "completed"
			data, _ := json.Marshal(st)
			tip, err := s.CompareAndSwap(context.Background(), 363, "", data)
			if err != nil {
				t.Fatal(err)
			}
			tr := &archiveCLITracker{}
			opts := RootOptions{RepoRoot: f.root, trackerOverride: tr}
			inventory := func(dir string) string { return f.git(dir, "for-each-ref", "--format=%(refname) %(objectname)") }
			before, local := inventory(f.project), inventory(f.root)
			out, err := f.run(opts, "repo", "archive", "363", "--json")
			if err != nil {
				t.Fatalf("preview %v %s", err, out)
			}
			var report lease.ArchiveReport
			if err = json.Unmarshal([]byte(out), &report); err != nil || report.Status != "preview" || len(report.Refs) != 1 || len(report.Checks) != 8 {
				t.Fatalf("preview JSON %s %v", out, err)
			}
			if inventory(f.project) != before || inventory(f.root) != local {
				t.Fatal("preview changed refs")
			}
			dest := filepath.Join(f.tmp, "archive.git")
			f.git(f.tmp, "init", "--bare", dest)
			if out, err = f.run(opts, "repo", "archive", "363", "--apply", "--to", "file://"+dest, "--json"); err != nil {
				t.Fatalf("apply %v %s", err, out)
			}
			if out, err = f.run(opts, "repo", "archive", "restore", "363", "--json"); err != nil {
				t.Fatalf("restore preview %v %s", err, out)
			}
			if out, err = f.run(opts, "repo", "archive", "restore", "363", "--apply", "--json"); err != nil {
				t.Fatalf("restore %v %s", err, out)
			}
			if got := f.git(f.project, "rev-parse", layout.StateRef(363)); got != tip {
				t.Fatal("restore changed state")
			}
		})
	}
}
func TestRepoArchiveRefusals(t *testing.T) {
	f := newLedgerCLIFixture(t, "branch")
	for _, restore := range []bool{false, true} {
		for _, apply := range []bool{false, true} {
			args := []string{"repo", "archive"}
			if restore {
				args = append(args, "restore")
			}
			args = append(args, "363", "--json")
			if apply {
				args = append(args, "--apply")
			}
			if out, err := f.run(RootOptions{RepoRoot: f.root, Fork: true}, args...); err == nil || !strings.Contains(err.Error(), "operator") {
				t.Fatalf("fork refusal %v %s", err, out)
			}
			setFixtureTracker(t, f.root, "local")
			if out, err := f.run(RootOptions{RepoRoot: f.root}, args...); err == nil {
				t.Fatalf("local accepted %s", out)
			}
			setFixtureTracker(t, f.root, "github")
			tr := &archiveCLITracker{denied: true}
			if out, err := f.run(RootOptions{RepoRoot: f.root, trackerOverride: tr}, args...); err == nil || !strings.Contains(err.Error(), "denied") {
				t.Fatalf("authorization %v %s", err, out)
			}
		}
	}
}
