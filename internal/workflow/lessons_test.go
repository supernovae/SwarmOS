package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

func TestSanitizeLessons(t *testing.T) {
	long := strings.Repeat("é", 200)
	raw := "# Lessons\n\nSome prose that is not a bullet.\n- Run `make db-up` before   integration tests\n" +
		"* The `cfg` package caches   on first load\n- run `make db-up` before integration tests\n" +
		"<!-- - hidden bullet -->\n-\n- " + long + "\n"
	got := SanitizeLessons(raw)
	if len(got) != 3 || got[0] != "Run `make db-up` before integration tests" || got[1] != "The `cfg` package caches on first load" {
		t.Fatalf("unexpected lessons: %q", got)
	}
	if len(got[2]) > maxLessonBytes+len("…") || !strings.HasSuffix(got[2], "…") || !strings.HasPrefix(got[2], "é") {
		t.Fatalf("long lesson not trimmed on a rune boundary: %q", got[2])
	}
	var many strings.Builder
	for i := 0; i < 30; i++ {
		many.WriteString("- lesson number " + strings.Repeat("x", i+1) + "\n")
	}
	if n := len(SanitizeLessons(many.String())); n != maxLessons {
		t.Fatalf("kept %d lessons, want %d", n, maxLessons)
	}
}

func TestImplementationSummaryCarriesLessonsFirst(t *testing.T) {
	root, _, _ := setupProjectPlanTestRepo(t)
	active := filepath.Join(root, ".swarm", "active")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "LESSONS.md"), []byte("- Handler tests need SWARM_USER set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, body := SummarizeImplementationChanges(t.Context(), root, 7)
	lessonsAt, changesAt := strings.Index(body, lessonsHeading), strings.Index(body, "## Working Tree Changes")
	if lessonsAt < 0 || changesAt < lessonsAt {
		t.Fatalf("lessons section missing or after the change list:\n%s", body)
	}
	if got := ExtractLessons(body); !reflect.DeepEqual(got, []string{"Handler tests need SWARM_USER set"}) {
		t.Fatalf("round trip lost the lesson: %q", got)
	}
}

func TestReadActiveLessonsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("- host secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(root, ".swarm", "active")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(active, "LESSONS.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := ReadActiveLessons(root); got != nil {
		t.Fatalf("lessons were read through a symlink: %q", got)
	}
}

type prListTracker struct {
	tracker.Tracker
	prs []*tracker.PullRequest
}

func (p *prListTracker) ListPullRequests(_ context.Context, f tracker.PullRequestFilter) ([]*tracker.PullRequest, error) {
	var out []*tracker.PullRequest
	for _, pr := range p.prs {
		if f.State == "all" || pr.State == f.State {
			out = append(out, pr)
		}
	}
	return out, nil
}

func seedState(t *testing.T, root string, issue, pr int, lessons ...string) {
	t.Helper()
	st := lease.IssueState{PRNumber: pr}
	if len(lessons) > 0 {
		st.Artifacts = []contract.Artifact{{Type: contract.ArtifactImplementation, Body: "# Implementation Summary\n\n" + formatLessonsSection(lessons) + "\n## Working Tree Changes\n"}}
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordination.NewFileStore(root).CompareAndSwap(t.Context(), issue, "", data); err != nil {
		t.Fatal(err)
	}
}

func TestHarvestLessonsOnlyFromMergedAuthoritativelyLinkedPRs(t *testing.T) {
	root, engine, tr := setupProjectPlanTestRepo(t)
	seedState(t, root, 10, 100, "Handler tests need SWARM_USER set", "Already known lesson")
	seedState(t, root, 11, 101, "Lesson from unmerged work")
	// Issue 12's state records PR 102, but PR 199 claims to close it.
	seedState(t, root, 12, 102, "Lesson stolen by another PR")
	engine.Tracker = &prListTracker{Tracker: tr, prs: []*tracker.PullRequest{
		{Number: 100, State: "closed", Merged: true, Body: "Closes #10"},
		{Number: 101, State: "closed", Merged: false, Body: "Closes #11"},
		{Number: 199, State: "closed", Merged: true, Body: "Closes #12"},
		{Number: 103, State: "open", Body: "Closes #10"},
	}}
	rules := filepath.Join(root, LessonsRulePath)
	if err := os.WriteFile(rules, []byte("# Lessons\n\n- Already known lesson (learned in #9, PR #90)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := engine.HarvestLessons(t.Context(), root, 50, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []HarvestedLesson{{Text: "Handler tests need SWARM_USER set", IssueNumber: 10, PRNumber: 100}}
	if !reflect.DeepEqual(report.Proposed, want) || report.AlreadyKnow != 1 || report.MergedPRs != 2 || report.Applied {
		t.Fatalf("unexpected harvest: %+v", report)
	}
	if data, _ := os.ReadFile(rules); strings.Contains(string(data), "SWARM_USER") {
		t.Fatal("preview must not write the rule file")
	}

	if report, err = engine.HarvestLessons(t.Context(), root, 50, true); err != nil || !report.Applied {
		t.Fatalf("apply: %+v %v", report, err)
	}
	data, err := os.ReadFile(rules)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "- Handler tests need SWARM_USER set (learned in #10, PR #100)\n") || !strings.HasPrefix(string(data), "# Lessons\n") {
		t.Fatalf("rule file not appended correctly:\n%s", data)
	}
	if report, err = engine.HarvestLessons(t.Context(), root, 50, false); err != nil || len(report.Proposed) != 0 {
		t.Fatalf("second harvest should find nothing new: %+v %v", report, err)
	}
}

func TestScanTriagesOpenPullRequests(t *testing.T) {
	root, engine, tr := setupProjectPlanTestRepo(t)
	seedState(t, root, 20, 200)
	now := time.Now()
	old := now.Add(-200 * 24 * time.Hour)
	engine.Tracker = &prListTracker{Tracker: tr, prs: []*tracker.PullRequest{
		{Number: 200, State: "open", Draft: true, Body: "Closes #20", UpdatedAt: now},
		{Number: 201, State: "open", Body: "Fixes #20", UpdatedAt: now, Author: "someone"},
		{Number: 202, State: "open", Body: "drive-by docs fix", UpdatedAt: old},
		{Number: 203, State: "open", Body: "Closes #30", UpdatedAt: now},
		{Number: 204, State: "open", Draft: true, Body: "", UpdatedAt: now},
	}}
	report, err := engine.RunProjectPlan(t.Context(), ProjectPlanOptions{RepoRoot: root, ScanOpen: true, StaleDays: 90, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]PRSenseItem{}
	for _, it := range report.PullRequests {
		got[it.Number] = it
	}
	if got[200].Lane != "competing" || got[201].Lane != "competing" || got[202].Lane != "quiet" || got[203].Lane != "review" || got[204].Lane != "in-progress" {
		t.Fatalf("unexpected PR lanes: %+v", report.PullRequests)
	}
	if got[200].WorkUnit != 20 || got[201].WorkUnit != 0 || got[203].WorkUnit != 0 {
		t.Fatalf("WorkUnit links must come from coordination state only: %+v", report.PullRequests)
	}
	for _, want := range []string{"Open Pull Requests (5)", "Competing PRs", "WorkUnit `#20`", "no WorkUnit, mentions `#30`", "quiet for 200 days"} {
		if !strings.Contains(report.SummaryMarkdown, want) {
			t.Fatalf("scan markdown missing %q:\n%s", want, report.SummaryMarkdown)
		}
	}
}
