package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

type bulkTask struct{ issue, pr int }

// bulkSubmit takes one task through approval, work and submission in the
// fixture's shared local tracker.
func bulkSubmit(t *testing.T, f *cliFixture) bulkTask {
	t.Helper()
	n := f.create(t, "agent")
	issue := strconv.Itoa(n)
	f.must(t, "issue", "approve", issue)
	f.must(t, "issue", "work", issue, "--agent", "mock", "--progress=false")
	f.must(t, "issue", "submit", issue)
	return bulkTask{n, f.state(t, n).PRNumber}
}

// bulkMerged submits and merges a task and adds its PR worktree.
func bulkMerged(t *testing.T, f *cliFixture) bulkTask {
	t.Helper()
	task := bulkSubmit(t, f)
	seedLocalCleanupMergedPR(t, f, task.pr)
	prGit(t, f.root, "worktree", "add", "-b", workspace.PRBranchName(task.pr), workspace.PRWorktreePath(f.root, task.pr), f.state(t, task.issue).HeadSHA)
	return task
}

func editLocalPR(t *testing.T, f *cliFixture, number int, edit func(*tracker.PullRequest)) {
	t.Helper()
	path := filepath.Join(f.root, ".swarm/local-tracker/state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	var prs map[string]*tracker.PullRequest
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(state["pull_requests"], &prs); err != nil {
		t.Fatal(err)
	}
	edit(prs[strconv.Itoa(number)])
	state["pull_requests"], _ = json.Marshal(prs)
	raw, _ = json.Marshal(state)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func runSplit(f *cliFixture, args ...string) (string, string, error) {
	cmd := NewRootCmd()
	var output, diagnostics bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
	err := cmd.Execute()
	return output.String(), diagnostics.String(), err
}

// decodeBulk requires stdout to hold exactly one JSON document.
func decodeBulk(t *testing.T, stdout string) bulkCleanupSummary {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var s bulkCleanupSummary
	if err := dec.Decode(&s); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
		t.Fatalf("stdout has more than one document: %v\n%s", err, stdout)
	}
	if s.Kind != "cleanup-bulk" {
		t.Fatalf("kind %q", s.Kind)
	}
	return s
}

func bulkIssue(t *testing.T, s bulkCleanupSummary, n int) bulkCleanupIssue {
	t.Helper()
	for _, issue := range s.Issues {
		if issue.IssueNumber == n {
			return issue
		}
	}
	t.Fatalf("issue #%d missing from summary: %+v", n, s.Issues)
	return bulkCleanupIssue{}
}

func bulkResource(t *testing.T, issue bulkCleanupIssue, kind, name string) bulkCleanupResource {
	t.Helper()
	for _, r := range issue.Resources {
		if r.Kind == kind && r.Name == name {
			return r
		}
	}
	t.Fatalf("issue #%d lacks %s %s: %+v", issue.IssueNumber, kind, name, issue.Resources)
	return bulkCleanupResource{}
}

type bulkMixed struct {
	f                    *cliFixture
	merged, dirty, open  bulkTask
	unsubmitted, orphan  int
	dirtyFile, orphanDir string
}

// bulkMixedFixture holds, in one local tracker: a clean merged task with a PR
// worktree, a merged task with a dirty worktree, an open PR, a claimed but
// unsubmitted task, and an orphan PR directory no issue records.
func bulkMixedFixture(t *testing.T) *bulkMixed {
	t.Helper()
	f := newCLIFixture(t)
	f.create(t, "agent") // PR and issue numbers deliberately differ.
	x := &bulkMixed{f: f}
	x.merged = bulkMerged(t, f)
	x.dirty = bulkSubmit(t, f)
	seedLocalCleanupMergedPR(t, f, x.dirty.pr)
	x.dirtyFile = filepath.Join(workspace.WorktreePath(f.root, x.dirty.issue), "scratch.txt")
	if err := os.WriteFile(x.dirtyFile, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	x.open = bulkSubmit(t, f)
	x.unsubmitted = f.create(t, "agent")
	f.must(t, "issue", "approve", strconv.Itoa(x.unsubmitted))
	f.must(t, "issue", "work", strconv.Itoa(x.unsubmitted), "--agent", "mock", "--progress=false")
	x.orphan = 999
	x.orphanDir = workspace.PRWorktreePath(f.root, x.orphan)
	if err := os.MkdirAll(x.orphanDir, 0700); err != nil {
		t.Fatal(err)
	}
	return x
}

func bulkFingerprint(t *testing.T, f *cliFixture, issues ...int) string {
	t.Helper()
	tracked, err := os.ReadFile(filepath.Join(f.root, ".swarm/local-tracker/state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.Write(tracked)
	b.WriteString(prGit(t, f.root, "for-each-ref"))
	b.WriteString(prGit(t, f.root, "worktree", "list", "--porcelain"))
	for _, n := range issues {
		raw, _ := json.Marshal(f.state(t, n))
		b.Write(raw)
	}
	_ = filepath.WalkDir(filepath.Join(f.root, ".swarm/worktrees"), func(p string, d os.DirEntry, e error) error {
		if e == nil {
			b.WriteString(p)
			if d.Type().IsRegular() {
				raw, _ := os.ReadFile(p)
				b.Write(raw)
			}
		}
		return nil
	})
	return b.String()
}

func TestCleanupBulkPreviewReadOnly(t *testing.T) {
	x := bulkMixedFixture(t)
	f := x.f
	issues := []int{x.merged.issue, x.dirty.issue, x.open.issue, x.unsubmitted}
	before := bulkFingerprint(t, f, issues...)
	text := f.must(t, "issue", "cleanup", "--all-merged")
	stdout, stderr, err := runSplit(f, "issue", "cleanup", "--all-merged", "--json")
	if err != nil {
		t.Fatalf("%v: %s %s", err, stdout, stderr)
	}
	if after := bulkFingerprint(t, f, issues...); after != before {
		t.Fatal("preview changed state, refs, registrations or worktree files")
	}
	s := decodeBulk(t, stdout)
	if s.Mode != "preview" || s.Status != "preview" || s.FailedIssues != 0 {
		t.Fatalf("summary: %+v", s)
	}
	var order []int
	for _, issue := range s.Issues {
		order = append(order, issue.IssueNumber)
	}
	if !reflect.DeepEqual(order, issues) {
		t.Fatalf("issue order %v, want %v", order, issues)
	}
	// Every discovered worktree appears exactly once with a reason.
	seen := map[string]int{}
	for _, issue := range s.Issues {
		for _, r := range issue.Resources {
			if r.Kind == "worktree" && !r.Absent {
				seen[r.Name]++
				if r.Reason == "" || (r.Disposition != "eligible" && r.Disposition != "retained") {
					t.Fatalf("worktree without disposition: %+v", r)
				}
			}
		}
	}
	for _, u := range s.Unresolved {
		seen[u.Path]++
	}
	discovered, err := workspace.DiscoverCleanupWorktrees(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != len(seen) {
		t.Fatalf("discovered %d worktrees, summary covers %v", len(discovered), seen)
	}
	for _, w := range discovered {
		if seen[w.Path] != 1 {
			t.Fatalf("%s reported %d times", w.Path, seen[w.Path])
		}
	}

	merged := bulkIssue(t, s, x.merged.issue)
	for _, r := range []bulkCleanupResource{
		bulkResource(t, merged, "worktree", workspace.WorktreePath(f.root, x.merged.issue)),
		bulkResource(t, merged, "worktree", workspace.PRWorktreePath(f.root, x.merged.pr)),
		bulkResource(t, merged, "local-code-branch", workspace.PRBranchName(x.merged.pr)),
	} {
		if r.Disposition != "eligible" {
			t.Fatalf("merged resource not eligible: %+v", r)
		}
	}
	if merged.Counts != (bulkCleanupCounts{Eligible: 4}) || merged.PRStatus != "merged" {
		t.Fatalf("merged counts: %+v", merged)
	}
	if r := merged.Resources[0]; r.Disposition != "not-applicable" {
		t.Fatalf("local remote branch counted: %+v", r)
	}
	dirty := bulkResource(t, bulkIssue(t, s, x.dirty.issue), "worktree", workspace.WorktreePath(f.root, x.dirty.issue))
	if dirty.Disposition != "retained" || !strings.Contains(dirty.Reason, "scratch.txt") {
		t.Fatalf("dirty worktree: %+v", dirty)
	}
	if open := bulkIssue(t, s, x.open.issue); open.Blocked == "" || open.Counts.Eligible != 0 || open.Counts.Retained == 0 {
		t.Fatalf("open PR: %+v", open)
	}
	if u := bulkIssue(t, s, x.unsubmitted); u.Blocked != "no recorded PR submission" || u.Counts.Eligible != 0 {
		t.Fatalf("unsubmitted: %+v", u)
	}
	if len(s.Unresolved) != 1 || s.Unresolved[0].Path != x.orphanDir || s.Unresolved[0].Reason != "no verified issue link" {
		t.Fatalf("orphan: %+v", s.Unresolved)
	}
	if d := bulkIssue(t, s, x.dirty.issue); d.Counts.Eligible != 0 || d.Counts.Retained != 2 {
		t.Fatalf("dirty counts: %+v", d)
	}
	if s.Totals.Eligible != 4 || s.Totals.Retired != 0 || s.Totals.Failed != 0 {
		t.Fatalf("totals: %+v", s.Totals)
	}
	for _, want := range []string{"advisory", "issue #" + strconv.Itoa(x.merged.issue), "eligible=4", "unresolved worktree " + x.orphanDir, "total: issues=4"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text output lacks %q:\n%s", want, text)
		}
	}
}

func TestCleanupBulkApplyMixed(t *testing.T) {
	x := bulkMixedFixture(t)
	f := x.f
	text := f.must(t, "issue", "cleanup", "--all-merged", "--apply")
	merged := f.state(t, x.merged.issue)
	for _, path := range []string{workspace.WorktreePath(f.root, x.merged.issue), workspace.PRWorktreePath(f.root, x.merged.pr)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("eligible worktree survived: %s %v", path, err)
		}
	}
	for _, branch := range []string{merged.Branch, workspace.PRBranchName(x.merged.pr)} {
		if out := prGit(t, f.root, "branch", "--list", branch); out != "" {
			t.Fatalf("eligible branch survived: %s", out)
		}
	}
	if raw, err := os.ReadFile(x.dirtyFile); err != nil || string(raw) != "keep me" {
		t.Fatalf("dirty worktree lost: %v", err)
	}
	for _, path := range []string{workspace.WorktreePath(f.root, x.open.issue), workspace.WorktreePath(f.root, x.unsubmitted), x.orphanDir} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("retained worktree removed: %s %v", path, err)
		}
	}
	if after := f.state(t, x.merged.issue); after.PRNumber != x.merged.pr || after.Cleanup != nil {
		t.Fatalf("authoritative state not retained: %+v", after)
	}
	if !strings.Contains(text, "issue #"+strconv.Itoa(x.merged.issue)+" (PR #"+strconv.Itoa(x.merged.pr)+" merged): eligible=0 retired=4 retained=0 failed=0") {
		t.Fatalf("apply text lacks per-issue counts:\n%s", text)
	}
	if !strings.Contains(text, "blocking path") {
		t.Fatalf("apply text lacks retention reasons:\n%s", text)
	}

	// A second run is safe and finds nothing left to retire.
	stdout, stderr, err := runSplit(f, "issue", "cleanup", "--all-merged", "--apply", "--json")
	if err != nil {
		t.Fatalf("%v: %s %s", err, stdout, stderr)
	}
	s := decodeBulk(t, stdout)
	if s.Status != "completed" || s.Totals.Retired != 0 || s.Totals.Failed != 0 {
		t.Fatalf("second run: %+v", s)
	}
	for _, issue := range s.Issues {
		if issue.IssueNumber == x.merged.issue {
			t.Fatalf("retired issue still discovered: %+v", issue)
		}
	}
}

func TestCleanupBulkContinuesAfterFailure(t *testing.T) {
	f := newCLIFixture(t)
	f.create(t, "agent")
	first, broken, last := bulkMerged(t, f), bulkMerged(t, f), bulkMerged(t, f)
	editLocalPR(t, f, broken.pr, func(pr *tracker.PullRequest) { pr.HTMLURL += "-forged" })
	stdout, stderr, err := runSplit(f, "issue", "cleanup", "--all-merged", "--apply", "--json")
	if err == nil || !strings.Contains(err.Error(), "issue #"+strconv.Itoa(broken.issue)) {
		t.Fatalf("failure not returned: %v\n%s", err, stderr)
	}
	s := decodeBulk(t, stdout)
	if s.Status != "failed" || s.FailedIssues != 1 || s.Error == "" {
		t.Fatalf("summary: %+v", s)
	}
	for _, task := range []bulkTask{first, last} {
		if issue := bulkIssue(t, s, task.issue); issue.Counts.Retired != 4 || issue.Error != "" {
			t.Fatalf("surrounding issue did not complete: %+v", issue)
		}
		if _, err := os.Lstat(workspace.WorktreePath(f.root, task.issue)); !os.IsNotExist(err) {
			t.Fatal("surrounding issue worktree survived")
		}
	}
	failed := bulkIssue(t, s, broken.issue)
	if !strings.Contains(failed.Error, "identity") || failed.Counts.Failed != 2 || failed.Counts.Retired != 0 {
		t.Fatalf("broken issue: %+v", failed)
	}
	for _, path := range []string{workspace.WorktreePath(f.root, broken.issue), workspace.PRWorktreePath(f.root, broken.pr)} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("failed issue resource removed: %v", err)
		}
	}
	if s.Totals.Retired != 8 || s.Totals.Failed != 2 {
		t.Fatalf("totals: %+v", s.Totals)
	}
}

func TestCleanupBulkApplyFailureAfterPartialRetirement(t *testing.T) {
	f := newCLIFixture(t)
	f.create(t, "agent")
	first, last := bulkMerged(t, f), bulkMerged(t, f)
	injected := errors.New("finish CAS lost")
	original := bulkApplyIssueCleanup
	t.Cleanup(func() { bulkApplyIssueCleanup = original })
	bulkApplyIssueCleanup = func(ctx context.Context, tr tracker.Tracker, m *lease.Manager, n int, p *lease.CleanupPreview) (bool, error) {
		started, err := original(ctx, tr, m, n, p)
		if n != first.issue || err != nil {
			return started, err
		}
		// Leave one resource unprocessed, as if the run stopped before it.
		p.Resources[len(p.Resources)-3].Retired = false
		p.Resources[len(p.Resources)-3].Eligible = true
		return true, injected
	}
	out, err := f.run("issue", "cleanup", "--all-merged", "--apply")
	if !errors.Is(err, injected) {
		t.Fatalf("injected failure lost identity: %v", err)
	}
	for _, want := range []string{
		"issue #" + strconv.Itoa(first.issue) + " (PR #" + strconv.Itoa(first.pr) + " merged): eligible=0 retired=3 retained=0 failed=1",
		"error: finish CAS lost",
		"issue #" + strconv.Itoa(last.issue) + " (PR #" + strconv.Itoa(last.pr) + " merged): eligible=0 retired=4 retained=0 failed=0",
		"failed_issues=1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Lstat(workspace.WorktreePath(f.root, last.issue)); !os.IsNotExist(err) {
		t.Fatal("later issue did not run")
	}
}

// Evidence that changes after the batched preview is caught by the fresh
// per-issue apply checks; unrelated issues still run.
func TestCleanupBulkFreshApplyEvidence(t *testing.T) {
	f := newCLIFixture(t)
	f.create(t, "agent")
	first, last := bulkMerged(t, f), bulkMerged(t, f)
	original := bulkApplyIssueCleanup
	t.Cleanup(func() { bulkApplyIssueCleanup = original })
	bulkApplyIssueCleanup = func(ctx context.Context, tr tracker.Tracker, m *lease.Manager, n int, p *lease.CleanupPreview) (bool, error) {
		if n == first.issue {
			// The PR is reopened after the fresh preview but before mutation.
			editLocalPR(t, f, first.pr, func(pr *tracker.PullRequest) { pr.State, pr.Merged, pr.MergedAt = "open", false, nil })
		}
		return original(ctx, tr, m, n, p)
	}
	stdout, _, err := runSplit(f, "issue", "cleanup", "--all-merged", "--apply", "--json")
	if err == nil {
		t.Fatal("changed evidence was not refused")
	}
	s := decodeBulk(t, stdout)
	if issue := bulkIssue(t, s, first.issue); issue.Error == "" || issue.Counts.Retired != 0 || issue.Counts.Failed != 4 {
		t.Fatalf("changed issue: %+v", issue)
	}
	for _, path := range []string{workspace.WorktreePath(f.root, first.issue), workspace.PRWorktreePath(f.root, first.pr)} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("refused issue lost %s: %v", path, err)
		}
	}
	if issue := bulkIssue(t, s, last.issue); issue.Counts.Retired != 4 || issue.Error != "" {
		t.Fatalf("unrelated issue: %+v", issue)
	}
	if f.state(t, first.issue).Cleanup != nil {
		t.Fatal("refused cleanup left a reservation")
	}
}

func TestCleanupBulkJSONEmptyAndDiagnostics(t *testing.T) {
	f := newCLIFixture(t)
	for _, args := range [][]string{{"--all-merged", "--json"}, {"--all-merged", "--apply", "--json"}} {
		stdout, stderr, err := runSplit(f, append([]string{"issue", "cleanup"}, args...)...)
		if err != nil || stderr != "" {
			t.Fatalf("%v: %s", err, stderr)
		}
		s := decodeBulk(t, stdout)
		if len(s.Issues) != 0 || len(s.Unresolved) != 0 || s.Totals != (bulkCleanupCounts{}) || s.Issues == nil || s.Unresolved == nil {
			t.Fatalf("empty inventory: %+v", s)
		}
	}
	// Argument failures still emit exactly one bulk document on stdout.
	stdout, stderr, err := runSplit(f, "issue", "cleanup", "--all-merged", "--recover-reservation", "--json")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("recovery accepted: %v %s", err, stderr)
	}
	if s := decodeBulk(t, stdout); s.Status != "failed" || !strings.Contains(s.Error, "mutually exclusive") {
		t.Fatalf("failure document: %+v", s)
	}
}

func TestCleanupBulkArguments(t *testing.T) {
	f := newCLIFixture(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--all-merged", "7"}, "does not take an issue number"},
		{[]string{"--all-merged", "--recover-reservation"}, "mutually exclusive"},
		{nil, "issue number or --all-merged"},
		{[]string{"1", "2"}, "accepts at most 1 arg"},
	} {
		_, err := f.run(append([]string{"issue", "cleanup"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: got %v, want %q", tc.args, err, tc.want)
		}
	}
}
