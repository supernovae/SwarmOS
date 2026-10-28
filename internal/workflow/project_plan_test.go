package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
)

func setupProjectPlanTestRepo(t *testing.T) (string, *Engine, *tracker.LocalTracker) {
	t.Helper()
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	root := t.TempDir()
	t.Setenv("SWARM_USER", "maintainer")
	git := func(args ...string) {
		t.Helper()
		if out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Plan Test")
	git("config", "user.email", "test@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "api", "handler.go"), []byte("package api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "initial commit")

	tr := tracker.NewLocalTracker(root)
	eng, err := NewEngine(root, tr)
	if err != nil {
		t.Fatal(err)
	}
	return root, eng, tr
}

func TestProjectPlannerReportsIsolationFailure(t *testing.T) {
	root, engine, _ := setupProjectPlanTestRepo(t)
	t.Setenv("SWARM_EXECUTION_MODE", "")
	entry := engine.Config.Agents["claude"]
	entry.Binary = "swarm-test-missing-claude"
	engine.Config.Agents["claude"] = entry
	source := filepath.Join(root, "roadmap.md")
	if err := os.WriteFile(source, []byte("# Roadmap\n- Deliver a tested feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := engine.parseOrSynthesizeRoadmap(t.Context(), root, source, "roadmap.md", "claude", nil, engine.Config)
	if err == nil || !strings.Contains(err.Error(), "is unavailable") {
		t.Fatalf("explicit agent request silently fell back after isolation failure: %v", err)
	}
}

type flakyLinkTracker struct {
	tracker.Tracker
	failChildNum int
	failedOnce   bool
}

func (f *flakyLinkTracker) LinkSubIssue(ctx context.Context, parentNumber, childNumber int) error {
	if childNumber == f.failChildNum && !f.failedOnce {
		f.failedOnce = true
		return os.ErrInvalid
	}
	return f.Tracker.LinkSubIssue(ctx, parentNumber, childNumber)
}

func TestRunProjectPlanFromMarkdownRoadmapAndScanBacklog(t *testing.T) {
	root, eng, tr := setupProjectPlanTestRepo(t)
	ctx := context.Background()

	roadmapPath := filepath.Join(root, "ROADMAP.md")
	roadmap := `# Q4 Roadmap

## Add rate limiter middleware in internal/api/handler.go
### Objective
Implement token-bucket rate limiting for API endpoints in internal/api/handler.go.
### Acceptance Criteria
- [ ] Rate limiter middleware blocks burst traffic
- [ ] Unit tests cover rate limit headers

## Add audit logging to internal/api/handler.go
### Objective
Emit structured audit events from internal/api/handler.go after each request.
### Acceptance Criteria
- [ ] Audit log entries include status code and latency
depends on: 1
`
	if err := os.WriteFile(roadmapPath, []byte(roadmap), 0o644); err != nil {
		t.Fatal(err)
	}

	// 0. Preview mode must keep IssueNumber=0, use Step ordinals, and not write to tracker
	preview, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{
		RepoRoot: root,
		FromFile: roadmapPath,
	})
	if err != nil {
		t.Fatalf("RunProjectPlan preview: %v", err)
	}
	if preview.ProposedUnits[0].IssueNumber != 0 || preview.ProposedUnits[1].IssueNumber != 0 {
		t.Fatalf("preview units must keep IssueNumber=0, got %+v", preview.ProposedUnits)
	}
	if !strings.Contains(preview.SummaryMarkdown, "`Step 1`") || !strings.Contains(preview.SummaryMarkdown, "`Step 2`") || strings.Contains(preview.SummaryMarkdown, "`#1`") {
		t.Fatalf("preview markdown should display Step ordinals, not fake #N:\n%s", preview.SummaryMarkdown)
	}
	if all, _ := tr.ListIssues(ctx, tracker.IssueFilter{State: "all"}); len(all) != 0 {
		t.Fatalf("preview must not create tracker issues, got %d", len(all))
	}

	// 1. Decompose roadmap and apply + approve
	fromReport, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{
		RepoRoot: root,
		FromFile: roadmapPath,
		Apply:    true,
		Ready:    true,
	})
	if err != nil {
		t.Fatalf("RunProjectPlan --from --apply: %v", err)
	}
	if fromReport.EpicIssue == nil || !fromReport.EpicCreated || !fromReport.EpicIssue.IsEpic() {
		t.Fatalf("expected created Epic issue, got epic=%#v created=%t", fromReport.EpicIssue, fromReport.EpicCreated)
	}
	if len(fromReport.EpicIssue.Labels) != 0 {
		t.Fatalf("expected Epic issue to have no readiness labels, got %v", fromReport.EpicIssue.Labels)
	}
	if !strings.Contains(fromReport.EpicIssue.Body, "# Q4 Roadmap") {
		t.Fatalf("expected Epic body to contain full source file, got:\n%s", fromReport.EpicIssue.Body)
	}
	if len(fromReport.CreatedIssues) != 2 {
		t.Fatalf("expected 2 created child issues, got %d", len(fromReport.CreatedIssues))
	}
	child1 := fromReport.CreatedIssues[0]
	child2 := fromReport.CreatedIssues[1]
	if !slices.Contains(child1.Labels, "swarm:ready") {
		t.Fatalf("expected first child #%d to have swarm:ready, got %v", child1.Number, child1.Labels)
	}
	if len(fromReport.ProposedUnits[1].DependsOn) != 1 || fromReport.ProposedUnits[1].DependsOn[0] != child1.Number {
		t.Fatalf("expected second child #%d to depend on #%d, got %v", child2.Number, child1.Number, fromReport.ProposedUnits[1].DependsOn)
	}
	for _, ch := range fromReport.CreatedIssues {
		p, err := tr.ParentIssueNumber(ctx, ch.Number)
		if err != nil || p != fromReport.EpicIssue.Number {
			t.Fatalf("expected child #%d native parent #%d, got (%d, %v)", ch.Number, fromReport.EpicIssue.Number, p, err)
		}
	}

	// 2. Create one unstructured raw issue and one issue touching a protected path
	rawIss, err := tr.CreateIssue(ctx, "Fix panic on empty request body", "When an empty POST body hits internal/api/handler.go it panics.\n- Add nil check and test", []string{"swarm:community"})
	if err != nil {
		t.Fatal(err)
	}
	riskyIss, err := tr.CreateIssue(ctx, "Modify CI workflow", "### Objective\n\nUpdate .github/workflows/swarm-validate.yml\n\n### Constraints\n\n- Keep fast\n\n### Acceptance Criteria\n\n- [ ] Passes\n\n### Workflow\n\nfeature\n\n### Plan Gate\n\nagent\n", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Scan without normalize: Epic (#1) must be skipped, leaving 4 non-Epic open issues
	scanPreview, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{
		RepoRoot: root,
		ScanOpen: true,
	})
	if err != nil {
		t.Fatalf("RunProjectPlan --scan preview: %v", err)
	}
	if len(scanPreview.BacklogItems) != 4 {
		t.Fatalf("expected 4 non-Epic backlog items, got %d", len(scanPreview.BacklogItems))
	}
	var foundRaw, foundRisky bool
	for _, it := range scanPreview.BacklogItems {
		if it.IssueNumber == fromReport.EpicIssue.Number {
			t.Fatalf("scan must skip Epic issue #%d", fromReport.EpicIssue.Number)
		}
		if it.IssueNumber == rawIss.Number {
			foundRaw = true
			if it.ValidWorkUnit || it.RecommendedLabel != "swarm:needs-scope" {
				t.Fatalf("expected raw community issue to be invalid with swarm:needs-scope, got %+v", it)
			}
			if !slices.Contains(it.OverlapsWith, child1.Number) {
				t.Fatalf("expected raw issue touching internal/api/handler.go to overlap with #%d, got %v", child1.Number, it.OverlapsWith)
			}
		}
		if it.IssueNumber == riskyIss.Number {
			foundRisky = true
			if it.GoalAlignment != "RISK_FLAGGED" || it.RecommendedLabel != "swarm:blocked" {
				t.Fatalf("expected protected-path issue to be RISK_FLAGGED / swarm:blocked, got %+v", it)
			}
		}
	}
	if !foundRaw || !foundRisky {
		t.Fatalf("missing expected backlog items: raw=%t risky=%t", foundRaw, foundRisky)
	}

	// Apply with --normalize: raw issue becomes swarm:draft, Epic remains untouched
	scanApplied, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{
		RepoRoot:  root,
		ScanOpen:  true,
		Apply:     true,
		Normalize: true,
	})
	if err != nil {
		t.Fatalf("RunProjectPlan --scan --apply --normalize: %v", err)
	}
	epicAfterScan, err := tr.GetIssue(ctx, fromReport.EpicIssue.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(epicAfterScan.Labels) != 0 || epicAfterScan.Body != fromReport.EpicIssue.Body {
		t.Fatalf("scan modified Epic issue #%d: labels=%v", epicAfterScan.Number, epicAfterScan.Labels)
	}
	updatedRaw, err := tr.GetIssue(ctx, rawIss.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updatedRaw.Body, "### Objective") || !slices.Contains(updatedRaw.Labels, "swarm:draft") {
		t.Fatalf("expected normalized raw issue to have WorkUnit sections and swarm:draft, got labels=%v body=%s", updatedRaw.Labels, updatedRaw.Body)
	}
	if len(scanApplied.ExecutionWaves) < 2 {
		t.Fatalf("expected overlapping issues to be separated into at least 2 execution waves, got %v", scanApplied.ExecutionWaves)
	}
}

func TestRunProjectPlanValidationIdempotencyConflictsAndRetry(t *testing.T) {
	root, eng, tr := setupProjectPlanTestRepo(t)
	ctx := context.Background()

	// 1. Validation failures before any tracker writes:
	//    - missing ## heading
	//    - missing acceptance criteria
	//    - duplicate titles / colliding slugs
	//    - malformed, duplicate, self, forward, and missing external depends_on
	badCases := map[string]string{
		"no-h2":           "# Only Top Heading\nSome text\n- [ ] Check\n",
		"no-acceptance":   "# Plan\n## Task One\n### Objective\nImplement feature one properly\n",
		"dup-title":       "# Plan\n## Same Title\n### Objective\nImplement first item\n### Acceptance Criteria\n- [ ] First check\n## same title\n### Objective\nImplement second item\n### Acceptance Criteria\n- [ ] Second check\n",
		"slug-collision":  "# Plan\n## Feature: Alpha!\n### Objective\nImplement alpha one\n### Acceptance Criteria\n- [ ] Check alpha\n## Feature - Alpha?\n### Objective\nImplement alpha two\n### Acceptance Criteria\n- [ ] Check alpha two\n",
		"malformed-dep":   "# Plan\n## Task One\n### Objective\nImplement task one\n### Acceptance Criteria\n- [ ] Check one\ndepends on: invalid\n",
		"dup-dep":         "# Plan\n## Task One\n### Objective\nImplement task one\n### Acceptance Criteria\n- [ ] Check one\n## Task Two\n### Objective\nImplement task two\n### Acceptance Criteria\n- [ ] Check two\ndepends on: 1, 1\n",
		"self-dep":        "# Plan\n## Task One\n### Objective\nImplement task one\n### Acceptance Criteria\n- [ ] Check one\ndepends on: 1\n",
		"forward-dep":     "# Plan\n## Task One\n### Objective\nImplement task one\n### Acceptance Criteria\n- [ ] Check one\ndepends on: 2\n## Task Two\n### Objective\nImplement task two\n### Acceptance Criteria\n- [ ] Check two\n",
		"missing-ext-dep": "# Plan\n## Task One\n### Objective\nImplement task one\n### Acceptance Criteria\n- [ ] Check one\ndepends on: 999\n",
	}
	for name, content := range badCases {
		p := filepath.Join(root, name+".md")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: p, Apply: true}); err == nil {
			t.Fatalf("case %s: expected validation/preflight error, got nil", name)
		}
	}
	if issues, _ := tr.ListIssues(ctx, tracker.IssueFilter{State: "all"}); len(issues) != 0 {
		t.Fatalf("expected 0 issues after validation failures, got %d", len(issues))
	}

	// 2. Outside-repo file: preview succeeds, --apply is rejected before writes
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "OUTSIDE.md")
	singleTaskMD := "# Single Task Plan\n\n## Ship single feature\n### Objective\nDeliver a single well-scoped feature.\n### Acceptance Criteria\n- [ ] Feature test passes\n"
	if err := os.WriteFile(outsideFile, []byte(singleTaskMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: outsideFile, Apply: false}); err != nil {
		t.Fatalf("outside-repo preview should succeed, got %v", err)
	}
	if _, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: outsideFile, Apply: true}); err == nil {
		t.Fatal("outside-repo --apply must fail")
	}

	// 3. Non-maintainer COLLABORATOR cannot run --from --apply, but can run preview and --scan
	inRepoSingle := filepath.Join(root, "SINGLE.md")
	if err := os.WriteFile(inRepoSingle, []byte(singleTaskMD), 0o644); err != nil {
		t.Fatal(err)
	}
	collabEng, err := NewEngine(root, tr.WithUser("contrib", "COLLABORATOR"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collabEng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: inRepoSingle, Apply: false}); err != nil {
		t.Fatalf("collaborator preview failed: %v", err)
	}
	if _, err := collabEng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: inRepoSingle, Apply: true}); err == nil {
		t.Fatal("collaborator --from --apply must be rejected before writes")
	}
	if _, err := collabEng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, ScanOpen: true}); err != nil {
		t.Fatalf("collaborator --scan failed: %v", err)
	}
	if issues, _ := tr.ListIssues(ctx, tracker.IssueFilter{State: "all"}); len(issues) != 0 {
		t.Fatalf("expected 0 issues after unauthorized --apply, got %d", len(issues))
	}

	// 4. Two independent tasks have NO automatic dependency chaining
	indepPath := filepath.Join(root, "INDEPENDENT.md")
	indepMD := `# Independent Roadmap

## Build alpha command
### Objective
Add alpha CLI command.
### Acceptance Criteria
- [ ] Alpha command runs

## Build beta command
### Objective
Add beta CLI command.
### Acceptance Criteria
- [ ] Beta command runs
`
	if err := os.WriteFile(indepPath, []byte(indepMD), 0o644); err != nil {
		t.Fatal(err)
	}
	// Test partial link failure on second child followed by retry
	flakyEng, err := NewEngine(root, &flakyLinkTracker{Tracker: tr, failChildNum: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flakyEng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: indepPath, Apply: true}); err == nil {
		t.Fatal("expected partial link failure on first attempt")
	}
	// Retry with normal engine: must reuse Epic #1 and Child #2, link already-created Child #3, and create 0 new issues!
	retryReport, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: indepPath, Apply: true})
	if err != nil {
		t.Fatalf("retry RunProjectPlan after partial link failure: %v", err)
	}
	if retryReport.EpicCreated || retryReport.EpicIssue.Number != 1 {
		t.Fatalf("expected retry to reuse Epic #1, got epic=%#v created=%t", retryReport.EpicIssue, retryReport.EpicCreated)
	}
	if len(retryReport.CreatedIssues) != 0 || len(retryReport.ExistingIssues) != 2 {
		t.Fatalf("expected retry to reuse 2 existing children and create 0, got created=%d existing=%d", len(retryReport.CreatedIssues), len(retryReport.ExistingIssues))
	}
	if len(retryReport.ProposedUnits[1].DependsOn) != 0 {
		t.Fatalf("expected independent second task to have empty DependsOn, got %v", retryReport.ProposedUnits[1].DependsOn)
	}
	if p, err := tr.ParentIssueNumber(ctx, 3); err != nil || p != 1 {
		t.Fatalf("expected retry to complete native link for child #3 -> Epic #1, got (%d, %v)", p, err)
	}

	// 5. Repeat apply with no changes (and even when a child is closed) creates no new issues
	//    and --ready on repeat apply does NOT approve reused children
	repeatReport, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: indepPath, Apply: true, Ready: true})
	if err != nil {
		t.Fatalf("repeat apply failed: %v", err)
	}
	if repeatReport.EpicCreated || len(repeatReport.CreatedIssues) != 0 || len(repeatReport.ExistingIssues) != 2 {
		t.Fatalf("unexpected repeat report: %+v", repeatReport)
	}
	reusedChild2, _ := tr.GetIssue(ctx, 2)
	if slices.Contains(reusedChild2.Labels, "swarm:ready") {
		t.Fatalf("--ready on repeat apply must not approve reused child #2: labels=%v", reusedChild2.Labels)
	}

	// 6. Add a 3rd task and remove the 2nd task: Epic body refreshes, 1st task reused, 3rd created, 2nd reported as leftover
	evolvedMD := `# Independent Roadmap

## Build alpha command
### Objective
Add alpha CLI command.
### Acceptance Criteria
- [ ] Alpha command runs

## Build gamma command
### Objective
Add gamma CLI command depending on alpha.
### Acceptance Criteria
- [ ] Gamma command runs
depends on: 1
`
	if err := os.WriteFile(indepPath, []byte(evolvedMD), 0o644); err != nil {
		t.Fatal(err)
	}
	evolvedReport, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: indepPath, Apply: true})
	if err != nil {
		t.Fatalf("evolved apply failed: %v", err)
	}
	if evolvedReport.EpicCreated || !strings.Contains(evolvedReport.EpicIssue.Body, "Build gamma command") {
		t.Fatalf("expected Epic #1 body to be refreshed with gamma command, got:\n%s", evolvedReport.EpicIssue.Body)
	}
	if len(evolvedReport.CreatedIssues) != 1 || evolvedReport.CreatedIssues[0].Number != 4 {
		t.Fatalf("expected only gamma (#4) to be created, got %+v", evolvedReport.CreatedIssues)
	}
	if len(evolvedReport.ExistingIssues) != 1 || evolvedReport.ExistingIssues[0].Number != 2 {
		t.Fatalf("expected alpha (#2) to be reused, got %+v", evolvedReport.ExistingIssues)
	}
	if len(evolvedReport.LeftoverIssues) != 1 || evolvedReport.LeftoverIssues[0].Number != 3 || evolvedReport.LeftoverIssues[0].State != "open" {
		t.Fatalf("expected beta (#3) to be reported as open leftover, got %+v", evolvedReport.LeftoverIssues)
	}
	if len(evolvedReport.ProposedUnits[1].DependsOn) != 1 || evolvedReport.ProposedUnits[1].DependsOn[0] != 2 {
		t.Fatalf("expected gamma to depend on reused alpha (#2), got %v", evolvedReport.ProposedUnits[1].DependsOn)
	}

	// 7. Modifying an existing task's acceptance criterion fails preflight with issue number and leaves issue count at 4
	conflictMD := strings.Replace(evolvedMD, "- [ ] Alpha command runs", "- [ ] Alpha command runs with flags", 1)
	if err := os.WriteFile(indepPath, []byte(conflictMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: indepPath, Apply: true}); err == nil || !strings.Contains(err.Error(), "#2") {
		t.Fatalf("expected conflict error mentioning issue #2, got %v", err)
	}
	if all, _ := tr.ListIssues(ctx, tracker.IssueFilter{State: "all"}); len(all) != 4 {
		t.Fatalf("expected issue count to remain 4 after conflict, got %d", len(all))
	}
}

func TestRunProjectPlanClosedReuseAndTrackerEditConflicts(t *testing.T) {
	root, eng, tr := setupProjectPlanTestRepo(t)
	ctx := context.Background()

	planPath := filepath.Join(root, "CLOSED_REUSE.md")
	planMD := `# Closed Reuse Plan

## First closed task
### Objective
Complete the first closed task.
### Acceptance Criteria
- [ ] First closed task verified
`
	if err := os.WriteFile(planPath, []byte(planMD), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: planPath, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	epicNum := first.EpicIssue.Number
	childNum := first.CreatedIssues[0].Number

	// Mark both Epic and child as closed in local tracker state, then re-run --apply: must reuse both!
	statePath := filepath.Join(root, ".swarm", "local-tracker", "state.json")
	rawBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	closedJSON := strings.ReplaceAll(string(rawBytes), `"state": "open"`, `"state": "closed"`)
	if err := os.WriteFile(statePath, []byte(closedJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	reusedClosed, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: planPath, Apply: true})
	if err != nil {
		t.Fatalf("expected closed Epic and child to be reused, got %v", err)
	}
	if reusedClosed.EpicCreated || reusedClosed.EpicIssue.Number != epicNum || len(reusedClosed.CreatedIssues) != 0 || len(reusedClosed.ExistingIssues) != 1 || reusedClosed.ExistingIssues[0].Number != childNum {
		t.Fatalf("expected closed reuse of epic #%d and child #%d, got %+v", epicNum, childNum, reusedClosed)
	}

	// Foreign title collision: create an unrelated issue whose title matches a new task in the plan
	foreignIss, err := tr.CreateIssue(ctx, "Colliding Task Title", "unrelated manual issue", nil)
	if err != nil {
		t.Fatal(err)
	}
	withCollision := planMD + "\n## colliding task title\n### Objective\nTry to collide with manual issue.\n### Acceptance Criteria\n- [ ] Check\n"
	if err := os.WriteFile(planPath, []byte(withCollision), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: planPath, Apply: true}); err == nil || !strings.Contains(err.Error(), "#3") {
		t.Fatalf("expected foreign title collision mentioning #%d, got %v", foreignIss.Number, err)
	}

	// Restore valid plan, then manually edit child body in tracker -> must reject with child issue number
	if err := os.WriteFile(planPath, []byte(planMD), 0o644); err != nil {
		t.Fatal(err)
	}
	origChild, _ := tr.GetIssue(ctx, childNum)
	if _, err := tr.UpdateIssueBody(ctx, childNum, "tampered body\n"+origChild.Body); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: planPath, Apply: true}); err == nil || !strings.Contains(err.Error(), "#2") {
		t.Fatalf("expected manual child edit conflict mentioning #%d, got %v", childNum, err)
	}
	// Restore child body, then manually edit Epic body in tracker -> must reject with Epic issue number
	_, _ = tr.UpdateIssueBody(ctx, childNum, origChild.Body)
	origEpic, _ := tr.GetIssue(ctx, epicNum)
	_, _ = tr.UpdateIssueBody(ctx, epicNum, "tampered epic\n"+origEpic.Body)
	if _, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, FromFile: planPath, Apply: true}); err == nil || !strings.Contains(err.Error(), "#1") {
		t.Fatalf("expected manual Epic edit conflict mentioning #%d, got %v", epicNum, err)
	}
}

func TestProjectPlanExportFormat(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"snapshot.yaml", "yaml"}, {"snapshot.yml", "yaml"}, {"snapshot.JSON", "json"},
		{"snapshot.YaMl", "yaml"}, {"snapshot.YML", "yaml"}, {"snapshot", ""}, {"snapshot.txt", ""}, {"", ""},
	} {
		got, err := ProjectPlanExportFormat(tc.path)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatalf("%q: %q, %v", tc.path, got, err)
		}
	}
}

func TestMarshalProjectPlanReport(t *testing.T) {
	report := &ProjectPlanReport{
		Mode: "from-plan", BaseSHA: "abc", PolicyDigest: "policy", GoalsDigest: "goals",
		EpicIssue:      &tracker.Issue{Number: 42, Title: "Release", Labels: []string{}},
		ProposedUnits:  []contract.WorkUnit{{ID: "task", Title: "Unicode café", Objective: "First\nSecond", Constraints: []string{"Keep order"}, Acceptance: []string{"Works"}, DependsOn: []int{3, 1}, Subtasks: []contract.WorkUnit{{ID: "nested", Constraints: []string{}, Acceptance: []string{}}}}},
		BacklogItems:   []IssueSenseItem{{IssueNumber: 7, RecommendedLabel: "swarm:draft", Touchpoints: []string{"b.go", "a.go"}}},
		ExecutionWaves: [][]int{{3, 1}, {7}}, SummaryMarkdown: "# Release\n\nReady.\n",
	}
	before, _ := json.Marshal(report)
	for _, format := range []string{"yaml", "json"} {
		for _, input := range []*ProjectPlanReport{report, {}} {
			data, err := MarshalProjectPlanReport(input, format)
			if err != nil {
				t.Fatal(err)
			}
			again, err := MarshalProjectPlanReport(input, format)
			if err != nil || !bytes.Equal(data, again) {
				t.Fatal("unstable serialization", err)
			}
			if !bytes.HasSuffix(data, []byte("\n")) || bytes.HasSuffix(data, []byte("\n\n")) {
				t.Fatalf("newline: %q", data)
			}
			var decoded ProjectPlanReport
			if format == "json" {
				err = json.Unmarshal(data, &decoded)
			} else {
				err = yaml.Unmarshal(data, &decoded)
			}
			if err != nil || !reflect.DeepEqual(input, &decoded) {
				t.Fatalf("%s roundtrip: %v\n%#v\n%#v", format, err, input, &decoded)
			}
			indent := "  base_sha:"
			if format == "json" {
				indent = "  \"base_sha\":"
			} else {
				indent = "  - id:"
			}
			if input == report && !bytes.Contains(data, []byte(indent)) {
				t.Fatalf("missing indentation %q: %s", indent, data)
			}
		}
	}
	after, _ := json.Marshal(report)
	if !bytes.Equal(before, after) {
		t.Fatal("input changed")
	}
	for _, tc := range []struct {
		r *ProjectPlanReport
		f string
	}{{nil, "yaml"}, {report, "xml"}} {
		if _, err := MarshalProjectPlanReport(tc.r, tc.f); err == nil {
			t.Fatal("expected error")
		}
	}
}
