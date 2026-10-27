package workflow

import (
	"slices"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestAssessTaskShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   ShapeInput
		want TaskShape
	}{
		{"routine typo", ShapeInput{Title: "Fix typo in README", Objective: "Correct the install command spelling", Acceptance: []string{"`make check` passes"}, Workflow: "bugfix", Touchpoints: []string{"README.md"}}, ShapeRoutine},
		{"bugfix crash is urgent", ShapeInput{Title: "CLI panics on empty config", Objective: "swarm crashes when config.yaml is empty", Acceptance: []string{"Regression test fails before the fix"}, Workflow: "bugfix"}, ShapeUrgent},
		{"outage is urgent for any workflow", ShapeInput{Title: "Webhook outage", Objective: "Deliveries stopped", Workflow: "feature"}, ShapeUrgent},
		{"feature mentioning security is not urgent", ShapeInput{Title: "Add security docs", Objective: "Document the security model", Acceptance: []string{"docs/security.md exists"}, Workflow: "feature"}, ShapeRoutine},
		{"feature adding regression tests is not urgent", ShapeInput{Title: "Add regression tests", Objective: "Cover the parser with regression tests", Acceptance: []string{"`go test ./...` passes"}, Workflow: "feature"}, ShapeRoutine},
		{"investigate is exploratory", ShapeInput{Title: "Caching layer", Objective: "Investigate whether a cache helps scan latency", Acceptance: []string{"Findings written up"}, Workflow: "feature"}, ShapeExploratory},
		{"question title is exploratory", ShapeInput{Title: "Support Windows paths?", Objective: "Users ask for Windows support", Acceptance: []string{"`go test ./...` passes"}, Workflow: "feature"}, ShapeExploratory},
		{"all vague criteria is exploratory", ShapeInput{Title: "CLI output", Objective: "Make the CLI output nicer", Acceptance: []string{"Output is cleaner", "Errors are better"}, Workflow: "feature"}, ShapeExploratory},
		{"wide touchpoints are involved", ShapeInput{Title: "Rename claim id", Objective: "Rename across packages", Acceptance: []string{"`make check` passes"}, Workflow: "refactor", Touchpoints: []string{"internal/lease/manager.go", "internal/cli/claim.go", "docs/WORKFLOWS.md"}}, ShapeInvolved},
		{"overlaps are involved", ShapeInput{Title: "Tweak handler", Objective: "Adjust the handler", Acceptance: []string{"handler returns 204"}, Workflow: "feature", Overlaps: 1}, ShapeInvolved},
		{"protected paths are involved", ShapeInput{Title: "CI tweak", Objective: "Speed up CI", Acceptance: []string{"`make check` passes"}, Workflow: "feature", Protected: true}, ShapeInvolved},
		{"urgent beats exploratory", ShapeInput{Title: "Investigate data loss", Objective: "Investigate data loss on resume", Workflow: "bugfix"}, ShapeUrgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AssessTaskShape(tc.in)
			if got.Shape != tc.want {
				t.Fatalf("shape = %s (signals %v), want %s", got.Shape, got.Signals, tc.want)
			}
			if got.Shape != ShapeRoutine && len(got.Signals) == 0 {
				t.Fatalf("%s shape has no explaining signal", got.Shape)
			}
		})
	}
}

func TestAssessTaskShapeHints(t *testing.T) {
	hasHint := func(a ShapeAssessment, sub string) bool {
		return slices.ContainsFunc(a.Hints, func(h string) bool { return strings.Contains(h, sub) })
	}
	noRepro := AssessTaskShape(ShapeInput{Title: "Wrong exit code", Objective: "swarm exits 0 on failure", Acceptance: []string{"exits 1 on failure"}, Workflow: "bugfix"})
	if !hasHint(noRepro, "reproduction") {
		t.Fatalf("bugfix without repro should get a hint: %v", noRepro.Hints)
	}
	unverifiable := AssessTaskShape(ShapeInput{Title: "Docs", Objective: "Document it", Acceptance: []string{"Feature is documented"}, Workflow: "feature"})
	if !hasHint(unverifiable, "observable result") {
		t.Fatalf("criteria with nothing checkable should get a hint: %v", unverifiable.Hints)
	}
	clean := AssessTaskShape(ShapeInput{Title: "Fix typo", Objective: "Fix typo; regression test added", Acceptance: []string{"`make check` passes"}, Workflow: "bugfix", Touchpoints: []string{"README.md"}})
	if len(clean.Hints) != 0 {
		t.Fatalf("well-formed routine issue should have no hints: %v", clean.Hints)
	}
}

func TestBacklogScanReportsShapeAndGatesExploratorySuggestions(t *testing.T) {
	root, engine, tr := setupProjectPlanTestRepo(t)
	ctx := t.Context()
	vague, err := tr.CreateIssue(ctx, "Should we cache scan results?", "Investigate whether caching helps large backlogs.", nil)
	if err != nil {
		t.Fatal(err)
	}
	crash, err := tr.CreateIssue(ctx, "Fix crash on empty body", "When an empty POST body hits internal/api/handler.go it crashes.\n- Add nil check and test", nil)
	if err != nil {
		t.Fatal(err)
	}
	routine, err := tr.CreateIssue(ctx, "Tidy handler doc", "### Objective\n\nTidy the doc comment in internal/api/handler.go\n\n### Constraints\n\n- Docs only\n\n### Acceptance Criteria\n\n- [ ] `go vet ./...` passes\n\n### Workflow\n\nfeature\n\n### Plan Gate\n\nnone\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := engine.RunProjectPlan(ctx, ProjectPlanOptions{RepoRoot: root, ScanOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	byNum := map[int]IssueSenseItem{}
	for _, it := range report.BacklogItems {
		byNum[it.IssueNumber] = it
	}
	if it := byNum[vague.Number]; it.Shape != ShapeExploratory || it.SuggestedUpdate == nil || it.SuggestedUpdate.Gate != contract.GateHuman || it.Gate != contract.GateHuman {
		t.Fatalf("exploratory raw issue should suggest a human gate: %+v", it)
	}
	if it := byNum[crash.Number]; it.Shape != ShapeUrgent || it.SuggestedUpdate == nil || it.SuggestedUpdate.Gate == contract.GateNone {
		t.Fatalf("urgent raw issue should suggest at least an agent gate: %+v", it)
	}
	// The valid WorkUnit overlaps the crash report on handler.go, which makes it Involved.
	if it := byNum[routine.Number]; it.Shape != ShapeInvolved || it.Gate != contract.GateNone {
		t.Fatalf("valid WorkUnit shape/gate: %+v", it)
	}
	md := report.SummaryMarkdown
	for _, want := range []string{"| Shape |", "Suggested Shapes", "Exploratory", "start with a probe", "stabilize first"} {
		if !strings.Contains(md, want) {
			t.Fatalf("scan markdown missing %q:\n%s", want, md)
		}
	}
}

func TestWorkUnitTouchpoints(t *testing.T) {
	wu := &contract.WorkUnit{
		Title:      "Tune internal/lease/manager.go",
		Objective:  "Adjust internal/cli/selection.go and ignore /etc/passwd and ../outside/x.go",
		Acceptance: []string{"`go test ./internal/lease` passes", "docs/WORKFLOWS.md mentions it"},
	}
	got := WorkUnitTouchpoints(t.TempDir(), wu)
	for _, want := range []string{"internal/lease/manager.go", "internal/cli/selection.go", "docs/WORKFLOWS.md"} {
		if !slices.Contains(got, want) {
			t.Fatalf("touchpoints %v missing %s", got, want)
		}
	}
	for _, tp := range got {
		if strings.HasPrefix(tp, "/") || strings.HasPrefix(tp, "..") {
			t.Fatalf("touchpoints %v include %s", got, tp)
		}
	}
	if got := WorkUnitTouchpoints(t.TempDir(), &contract.WorkUnit{Title: "Polish", Objective: "No paths here"}); len(got) != 0 {
		t.Fatalf("unknown touchpoints = %v", got)
	}
	if got := WorkUnitTouchpoints(t.TempDir(), nil); got != nil {
		t.Fatalf("nil WorkUnit touchpoints = %v", got)
	}
}

func TestTouchpointOverlap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate []string
		inFlight  map[int][]string
		score     int
		issues    []int
		paths     []string
	}{
		{"same file", []string{"internal/lease/a.go"}, map[int][]string{7: {"internal/lease/a.go"}}, 1, []int{7}, []string{"internal/lease/a.go"}},
		{"same go package", []string{"internal/lease/b.go"}, map[int][]string{7: {"internal/lease/a.go"}}, 1, []int{7}, []string{"internal/lease/b.go"}},
		{"parent directory", []string{"internal/lease"}, map[int][]string{7: {"internal/lease/sub/a.go"}}, 1, []int{7}, []string{"internal/lease"}},
		{"child of directory", []string{"docs/guide/intro.md"}, map[int][]string{7: {"docs"}}, 1, []int{7}, []string{"docs/guide/intro.md"}},
		{"sibling directories", []string{"internal/lease/a.go"}, map[int][]string{7: {"internal/leasex/a.go", "internal/cli/a.go"}}, 0, nil, nil},
		{"string prefix is not ancestry", []string{"internal/lease"}, map[int][]string{7: {"internal/leases"}}, 0, nil, nil},
		{"same directory non-go files", []string{"docs/a.md"}, map[int][]string{7: {"docs/b.md"}}, 0, nil, nil},
		{"subpackage is a different package", []string{"internal/lease/a.go"}, map[int][]string{7: {"internal/lease/sub/b.go"}}, 0, nil, nil},
		{"duplicate paths count one issue", []string{"internal/lease/a.go", "internal/lease/a.go", "internal/lease/b.go"}, map[int][]string{7: {"internal/lease/a.go", "internal/lease/c.go"}}, 1, []int{7}, []string{"internal/lease/a.go", "internal/lease/b.go"}},
		{"multiple issues", []string{"internal/lease/a.go"}, map[int][]string{9: {"internal/lease"}, 7: {"internal/lease/z.go"}, 8: {"docs/x.md"}}, 2, []int{7, 9}, []string{"internal/lease/a.go"}},
		{"normalized separators", []string{"internal\\lease\\a.go"}, map[int][]string{7: {"internal/lease/./b.go"}}, 1, []int{7}, []string{"internal/lease/a.go"}},
		{"empty candidate", nil, map[int][]string{7: {"internal/lease/a.go"}}, 0, nil, nil},
		{"no in-flight work", []string{"internal/lease/a.go"}, nil, 0, nil, nil},
		{"absolute and traversal rejected", []string{"/internal/lease/a.go", "../internal/lease/a.go", "."}, map[int][]string{7: {"internal/lease/a.go", "/internal/lease/b.go", ".."}}, 0, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AssessTouchpointOverlap(tc.candidate, tc.inFlight)
			if got.Score != tc.score || !slices.Equal(got.Issues, tc.issues) || !slices.Equal(got.Paths, tc.paths) {
				t.Fatalf("got %+v, want score %d issues %v paths %v", got, tc.score, tc.issues, tc.paths)
			}
		})
	}
}
