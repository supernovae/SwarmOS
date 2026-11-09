package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			setArchiveCLIOptIn(t, f)
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

func setArchiveCLIOptIn(t *testing.T, f *ledgerCLIFixture) {
	t.Helper()
	path := filepath.Join(f.root, ".swarm", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\narchive:\n  strategy: repo\n  after_days: 30\n  repo: acme/private-swarm-archive\n")...)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.git(f.root, "add", ".swarm/config.yaml")
	f.git(f.root, "commit", "-m", "archive opt in")
	f.git(f.root, "push", "origin", "main")
}
func TestArchiveBulkCLI(t *testing.T) {
	for _, ns := range []string{"branch", "hidden"} {
		t.Run(ns, func(t *testing.T) {
			f := newLedgerCLIFixture(t, ns)
			setArchiveCLIOptIn(t, f)
			layout, _ := coordination.LayoutFor(ns)
			// Build state commits with distinct author and committer dates.
			dates := []string{"2026-01-01T00:00:00Z", "2026-03-01T12:00:00Z", "2026-02-28T12:00:00Z", "2027-01-01T00:00:00Z"}
			for i, date := range dates {
				n := i + 1
				raw, _ := json.Marshal(lease.IssueState{Lease: contract.Lease{IssueNumber: n, Status: "completed"}})
				path := filepath.Join(f.root, "state.json")
				if e := os.WriteFile(path, raw, 0600); e != nil {
					t.Fatal(e)
				}
				f.git(f.root, "add", "state.json")
				cmd := gitfixture.Command(t, f.root, "commit", "-m", fmt.Sprint(n))
				cmd.Env = append(cmd.Env, "GIT_COMMITTER_DATE="+date, "GIT_AUTHOR_DATE=2020-01-01T00:00:00Z")
				if out, e := cmd.CombinedOutput(); e != nil {
					t.Fatalf("%s %v", out, e)
				}
				// State store expects state.json at the commit root.
				f.git(f.root, "push", "origin", "HEAD:"+layout.StateRef(n))
			}
			opts := RootOptions{RepoRoot: f.root, trackerOverride: &archiveCLITracker{}}
			dest := filepath.Join(f.tmp, "archive.git")
			f.git(f.tmp, "init", "--bare", dest)
			inv := func(dir string) string { return f.git(dir, "for-each-ref", "--format=%(refname) %(objectname)") }
			before, local, db := inv(f.project), inv(f.root), inv(dest)
			out, err := f.run(opts, "repo", "archive", "--all-eligible", "--json", "--to", "file://"+dest)
			var r lease.BulkArchiveReport
			if err != nil || json.Unmarshal([]byte(out), &r) != nil {
				t.Fatalf("%s %v", out, err)
			}
			if r.Totals.Candidates != 4 || r.Totals.Previewed != 1 || r.Totals.Skipped != 3 {
				t.Fatalf("%s", out)
			}
			cutoff := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
			human, humanErr := f.run(opts, "repo", "archive", "--all-eligible", "--to", "file://"+dest)
			if humanErr != nil {
				t.Fatal(humanErr)
			}
			if len(r.Issues) != len(dates) {
				t.Fatalf("missing candidates: %s", out)
			}
			for i, issue := range r.Issues {
				stamp, _ := time.Parse(time.RFC3339, dates[i])
				eligible := i == 0
				status := "skipped"
				if eligible {
					status = "preview"
				}
				if issue.Issue != i+1 || issue.Eligible != eligible || issue.Status != status || issue.CommitterTime == nil || !issue.CommitterTime.Equal(stamp) || issue.Cutoff == nil || !issue.Cutoff.Equal(cutoff) || issue.AfterDays != 30 || issue.StateTip != f.git(f.project, "rev-parse", layout.StateRef(i+1)) {
					t.Fatalf("candidate %d age evidence: %+v", i+1, issue)
				}
				if !strings.Contains(human, fmt.Sprintf("#%d: %s eligible=%t tip=%s", issue.Issue, issue.Status, issue.Eligible, issue.StateTip)) {
					t.Fatalf("human/JSON mismatch: %s", human)
				}
			}
			if !strings.Contains(human, fmt.Sprintf("totals: %+v", r.Totals)) {
				t.Fatalf("human totals: %s", human)
			}
			if before != inv(f.project) || local != inv(f.root) || db != inv(dest) {
				t.Fatal("preview changed refs")
			}
			out, err = f.run(opts, "repo", "archive", "--all-eligible", "--apply", "--json", "--to", "file://"+dest)
			if err != nil || json.Unmarshal([]byte(out), &r) != nil || r.Totals.Archived != 1 {
				t.Fatalf("%s %v", out, err)
			}
			store := coordination.NewGitStoreWithLayout(f.root, "origin", layout)
			tomb, err := store.ArchiveTombstone(context.Background(), 1, true)
			if err != nil || tomb == nil || tomb.Manifest.Cutoff == nil || !tomb.Manifest.Cutoff.Equal(cutoff) {
				t.Fatalf("tombstone cutoff: %v %v", tomb, err)
			}
			manifest, err := contract.ParseArchiveManifest([]byte(f.git(dest, "show", "refs/archive/issue-1/manifest:manifest.json")))
			if err != nil {
				t.Fatal(err)
			}
			_, digest, err := contract.MarshalArchiveManifest(manifest)
			if err != nil || digest != tomb.Digest || manifest.StateTip != tomb.Manifest.StateTip || manifest.Cutoff == nil || !manifest.Cutoff.Equal(cutoff) {
				t.Fatal("destination/tombstone identity mismatch")
			}
			for _, ref := range manifest.Refs {
				if f.git(dest, "rev-parse", ref.Destination) != ref.SHA {
					t.Fatal("destination SHA mismatch")
				}
			}

		})
	}
}

func TestArchiveKeepTrustedAndGrammar(t *testing.T) {
	for _, ns := range []string{"branch", "hidden"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", ns, explicit), func(t *testing.T) {
				f := newLedgerCLIFixture(t, ns)
				path := filepath.Join(f.root, ".swarm", "config.yaml")
				data, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				if explicit {
					data = append(data, []byte("\narchive:\n  strategy: keep\n")...)
					if e = os.WriteFile(path, data, 0600); e != nil {
						t.Fatal(e)
					}
					f.git(f.root, "commit", "-am", "explicit keep")
					f.git(f.root, "push", "origin", "main")
				}
				layout, _ := coordination.LayoutFor(ns)
				store := coordination.NewGitStoreWithLayout(f.root, "origin", layout)
				raw, _ := json.Marshal(lease.IssueState{Lease: contract.Lease{IssueNumber: 1, Status: "completed"}})
				if _, e = store.CompareAndSwapRetaining(context.Background(), 1, "", raw, f.git(f.root, "rev-parse", "HEAD")); e != nil {
					t.Fatal(e)
				}
				dest := filepath.Join(f.tmp, "archive.git")
				f.git(f.tmp, "init", "--bare", dest)
				// A candidate-only opt-in must not authorize deletion.
				text := strings.ReplaceAll(string(data), "archive:\n  strategy: keep\n", "")
				text += "\narchive:\n  strategy: repo\n  after_days: 30\n  repo: acme/archive\n"
				if e = os.WriteFile(path, []byte(text), 0600); e != nil {
					t.Fatal(e)
				}
				for _, phase := range []string{"worktree", "candidate"} {
					if phase == "candidate" {
						f.git(f.root, "commit", "-am", "untrusted opt in")
					}
					inv := func(dir string) string { return f.git(dir, "for-each-ref", "--format=%(refname) %(objectname)") }
					before, local, db := inv(f.project), inv(f.root), inv(dest)
					opts := RootOptions{RepoRoot: f.root, trackerOverride: &archiveCLITracker{}}
					for _, args := range [][]string{
						{"repo", "archive", "1", "--apply", "--to", "file://" + dest, "--json"},
						{"repo", "archive", "--all-eligible", "--apply", "--to", "file://" + dest, "--json"},
						{"repo", "archive", "1", "--apply", "--json"},
						{"repo", "archive", "--all-eligible", "--apply", "--json"},
						{"repo", "archive", "--json"}, {"repo", "archive", "1", "--all-eligible", "--json"},
					} {
						out, err := f.run(opts, args...)
						if err == nil {
							t.Fatalf("accepted %v", args)
						}
						var doc map[string]any
						if e = json.Unmarshal([]byte(out), &doc); e != nil {
							t.Fatalf("not one JSON: %s %v", out, e)
						}
					}
					out, e := f.run(opts, "repo", "archive", "--all-eligible", "--json")
					var report lease.BulkArchiveReport
					if e != nil || json.Unmarshal([]byte(out), &report) != nil || report.Totals.Skipped != 1 || !strings.Contains(out, "strategy: keep") {
						t.Fatalf("%s %v", out, e)
					}
					if before != inv(f.project) || local != inv(f.root) || db != inv(dest) {
						t.Fatal("keep changed refs")
					}
				}
			})
		}
	}
}

// Run with a caller-owned clock (ledgerCLIFixture.run uses its own fixed clock).
func runArchiveClock(f *ledgerCLIFixture, opts RootOptions, now time.Time, args ...string) (string, error) {
	opts.nowFn = func() time.Time { return now }
	cmd := newRootCmdWithOptions(&opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestArchiveBulkJSONPartialFailureRetry(t *testing.T) {
	for _, ns := range []string{"branch", "hidden"} {
		for _, failure := range []string{"copy", "delete"} {
			t.Run(ns+"/"+failure, func(t *testing.T) {
				f := newLedgerCLIFixture(t, ns)
				setArchiveCLIOptIn(t, f)
				layout, _ := coordination.LayoutFor(ns)
				store := coordination.NewGitStoreWithLayout(f.root, "origin", layout)
				ctx := context.Background()
				var tip string
				head := f.git(f.root, "rev-parse", "HEAD")
				for n := 1; n <= 3; n++ {
					raw, _ := json.Marshal(lease.IssueState{Lease: contract.Lease{IssueNumber: n, Status: "completed"}})
					var e error
					tip, e = store.CompareAndSwapRetaining(ctx, n, "", raw, head)
					if e != nil {
						t.Fatal(e)
					}
				}
				inventory := func(n int) string {
					return f.git(f.project, "for-each-ref", "--format=%(refname) %(objectname)", layout.StateRef(n), fmt.Sprintf("%sissue-%d/", layout.CheckpointPrefix(), n))
				}
				before := map[int]string{}
				for n := 1; n <= 3; n++ {
					before[n] = inventory(n)
				}
				main := f.git(f.project, "rev-parse", "refs/heads/main")
				stamp, e := store.ArchiveCommitTime(ctx, tip)
				if e != nil {
					t.Fatal(e)
				}
				now := stamp.Add(90 * 24 * time.Hour)
				dest := filepath.Join(f.tmp, "archive.git")
				f.git(f.tmp, "init", "--bare", dest)
				dir, ref := dest, "refs/archive/issue-2/state"
				condition := fmt.Sprintf(`[ "$ref" = "%s" ]`, ref)
				if failure == "delete" {
					dir = f.project
					condition = fmt.Sprintf(`[ "$ref" = "%s" ] && [ "$new" = "%s" ]`, layout.StateRef(2), strings.Repeat("0", 40))
				}
				hook := filepath.Join(dir, "hooks", "pre-receive")
				if e = os.MkdirAll(filepath.Dir(hook), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(hook, []byte("#!/bin/sh\nwhile read old new ref; do\nif "+condition+"; then exit 1; fi\ndone\n"), 0700); e != nil {
					t.Fatal(e)
				}
				opts := RootOptions{RepoRoot: f.root, trackerOverride: &archiveCLITracker{}}
				args := []string{"repo", "archive", "--all-eligible", "--apply", "--json", "--to", "file://" + dest}
				out, e := runArchiveClock(f, opts, now, args...)
				var r lease.BulkArchiveReport
				if e == nil || json.Unmarshal([]byte(out), &r) != nil || r.Totals.Archived != 2 || r.Totals.Failed != 1 || len(r.Issues) != 3 || r.Issues[1].Status != "failed" {
					t.Fatalf("%s %v", out, e)
				}
				if got := inventory(2); got != before[2] {
					t.Fatalf("failed issue refs changed: %s != %s", got, before[2])
				}
				for n := 1; n <= 3; n++ {
					tomb, err := store.ArchiveTombstone(ctx, n, true)
					if n == 2 {
						if !errors.Is(err, coordination.ErrNotFound) || tomb != nil {
							t.Fatalf("failed issue tombstone: %v %v", tomb, err)
						}
						continue
					}
					if err != nil || tomb == nil || inventory(n) != "" {
						t.Fatalf("successful issue %d not atomically archived: %v %v", n, tomb, err)
					}
					for _, ref := range tomb.Manifest.Refs {
						if got := f.git(dest, "rev-parse", ref.Destination); got != ref.SHA {
							t.Fatalf("copy mismatch: %s", ref.Destination)
						}
					}
				}
				if got := f.git(f.project, "rev-parse", "refs/heads/main"); got != main {
					t.Fatal("unrelated main changed")
				}
				var original string
				if failure == "delete" {
					original = f.git(dest, "show", "refs/archive/issue-2/manifest:manifest.json")
				}
				if e = os.Remove(hook); e != nil {
					t.Fatal(e)
				}
				out, e = runArchiveClock(f, opts, now.Add(24*time.Hour), args...)
				if e != nil || json.Unmarshal([]byte(out), &r) != nil || r.Totals.Archived != 1 || r.Totals.Failed != 0 || len(r.Issues) != 1 || r.Issues[0].Issue != 2 {
					t.Fatalf("%s %v", out, e)
				}
				emptyOut, emptyErr := runArchiveClock(f, opts, now.Add(48*time.Hour), args...)
				var empty lease.BulkArchiveReport
				if emptyErr != nil || json.Unmarshal([]byte(emptyOut), &empty) != nil || empty.Totals != (lease.BulkArchiveTotals{}) {
					t.Fatalf("empty rerun: %s %v", emptyOut, emptyErr)
				}
				if original != "" && f.git(dest, "show", "refs/archive/issue-2/manifest:manifest.json") != original {
					t.Fatal("manifest changed")
				}
			})
		}
	}
}
