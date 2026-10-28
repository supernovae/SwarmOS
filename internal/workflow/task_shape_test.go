package workflow

import (
	"slices"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestAssessTaskDimensions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   DimensionInput
		want Dimensions
	}{
		{"routine typo", DimensionInput{Title: "Fix typo in README", Objective: "Correct the install command spelling", Acceptance: []string{"`make check` passes"}, Workflow: "bugfix", Touchpoints: []string{"README.md"}}, Dimensions{"small", "known", "standard"}},
		{"bugfix crash is urgent", DimensionInput{Title: "CLI panics on empty config", Objective: "swarm crashes when config.yaml is empty", Acceptance: []string{"Regression test fails before the fix"}, Workflow: "bugfix"}, Dimensions{"small", "known", "urgent"}},
		{"outage is urgent for any workflow", DimensionInput{Title: "Webhook outage", Objective: "Deliveries stopped", Workflow: "feature"}, Dimensions{"small", "known", "urgent"}},
		{"feature mentioning security is not urgent", DimensionInput{Title: "Add security docs", Objective: "Document the security model", Acceptance: []string{"docs/security.md exists"}, Workflow: "feature"}, Dimensions{"small", "known", "standard"}},
		{"feature adding regression tests is not urgent", DimensionInput{Title: "Add regression tests", Objective: "Cover the parser with regression tests", Acceptance: []string{"`go test ./...` passes"}, Workflow: "feature"}, Dimensions{"small", "known", "standard"}},
		{"investigate is exploratory", DimensionInput{Title: "Caching layer", Objective: "Investigate whether a cache helps scan latency", Acceptance: []string{"Findings written up"}, Workflow: "feature"}, Dimensions{"small", "unknown", "standard"}},
		{"question title is exploratory", DimensionInput{Title: "Support Windows paths?", Objective: "Users ask for Windows support", Acceptance: []string{"`go test ./...` passes"}, Workflow: "feature"}, Dimensions{"small", "unknown", "standard"}},
		{"all vague criteria is exploratory", DimensionInput{Title: "CLI output", Objective: "Make the CLI output nicer", Acceptance: []string{"Output is cleaner", "Errors are better"}, Workflow: "feature"}, Dimensions{"small", "unknown", "standard"}},
		{"wide touchpoints are involved", DimensionInput{Title: "Rename claim id", Objective: "Rename across packages", Acceptance: []string{"`make check` passes"}, Workflow: "refactor", Touchpoints: []string{"internal/lease/manager.go", "internal/cli/claim.go", "docs/WORKFLOWS.md"}}, Dimensions{"multi_area", "known", "standard"}},
		{"overlaps are involved", DimensionInput{Title: "Tweak handler", Objective: "Adjust the handler", Acceptance: []string{"handler returns 204"}, Workflow: "feature", Overlaps: 1}, Dimensions{"small", "known", "standard"}},
		{"protected paths are involved", DimensionInput{Title: "CI tweak", Objective: "Speed up CI", Acceptance: []string{"`make check` passes"}, Workflow: "feature", Protected: true}, Dimensions{"small", "known", "sensitive"}},
		{"urgent beats exploratory", DimensionInput{Title: "Investigate data loss", Objective: "Investigate data loss on resume", Workflow: "bugfix"}, Dimensions{"small", "known", "urgent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := AssessTaskDimensions(tc.in)
			if got.SuggestedDimensions != tc.want {
				t.Fatalf("dimensions = %s (signals %v), want %s", got.SuggestedDimensions, got.Signals, tc.want)
			}
			if got.SuggestedDimensions != (Dimensions{"small", "known", "standard"}) && len(got.Signals) == 0 {
				t.Fatalf("%s dimensions have no explaining signal", got.SuggestedDimensions)
			}
		})
	}
}

func TestAssessTaskDimensionsHints(t *testing.T) {
	t.Parallel()
	hasHint := func(a DimensionAssessment, sub string) bool {
		return slices.ContainsFunc(a.Hints, func(h string) bool { return strings.Contains(h, sub) })
	}
	noRepro := AssessTaskDimensions(DimensionInput{Title: "Wrong exit code", Objective: "swarm exits 0 on failure", Acceptance: []string{"exits 1 on failure"}, Workflow: "bugfix"})
	if !hasHint(noRepro, "reproduction") {
		t.Fatalf("bugfix without repro should get a hint: %v", noRepro.Hints)
	}
	unverifiable := AssessTaskDimensions(DimensionInput{Title: "Docs", Objective: "Document it", Acceptance: []string{"Feature is documented"}, Workflow: "feature"})
	if !hasHint(unverifiable, "observable result") {
		t.Fatalf("criteria with nothing checkable should get a hint: %v", unverifiable.Hints)
	}
	clean := AssessTaskDimensions(DimensionInput{Title: "Fix typo", Objective: "Fix typo; regression test added", Acceptance: []string{"`make check` passes"}, Workflow: "bugfix", Touchpoints: []string{"README.md"}})
	if len(clean.Hints) != 0 {
		t.Fatalf("well-formed routine issue should have no hints: %v", clean.Hints)
	}
}

func TestBacklogScanReportsDimensionsAndGatesUnknownSuggestions(t *testing.T) {
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
	if it := byNum[vague.Number]; it.SuggestedDimensions.Uncertainty != "unknown" || it.SuggestedUpdate == nil || it.SuggestedUpdate.Gate != contract.GateHuman || it.Gate != contract.GateHuman {
		t.Fatalf("exploratory raw issue should suggest a human gate: %+v", it)
	}
	if it := byNum[crash.Number]; it.SuggestedDimensions.Risk != "urgent" || it.SuggestedUpdate == nil || it.SuggestedUpdate.Gate == contract.GateNone {
		t.Fatalf("urgent raw issue should suggest at least an agent gate: %+v", it)
	}
	// The valid WorkUnit overlaps the crash report on handler.go, which adds a scope note without changing dimensions.
	if it := byNum[routine.Number]; it.SuggestedDimensions != (Dimensions{"small", "known", "standard"}) || it.Gate != contract.GateNone {
		t.Fatalf("valid WorkUnit dimensions/gate: %+v", it)
	}
	for _, format := range []string{"json", "yaml"} {
		raw, err := MarshalProjectPlanReport(report, format)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"suggested_dimensions", "signals", "unknown", "urgent"} {
			if !strings.Contains(string(raw), want) {
				t.Fatalf("%s export missing %s: %s", format, want, raw)
			}
		}
		if strings.Contains(string(raw), "shape") {
			t.Fatalf("obsolete export field: %s", raw)
		}
	}

	md := report.SummaryMarkdown
	for _, want := range []string{"| Suggested dimensions |", "Suggested Dimensions", "uncertainty=unknown", "start with a probe", "stabilize first"} {
		if !strings.Contains(md, want) {
			t.Fatalf("scan markdown missing %q:\n%s", want, md)
		}
	}
}

func TestWorkUnitTouchpoints(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
			got := AssessTouchpointOverlap(tc.candidate, tc.inFlight)
			if got.Score != tc.score || !slices.Equal(got.Issues, tc.issues) || !slices.Equal(got.Paths, tc.paths) {
				t.Fatalf("got %+v, want score %d issues %v paths %v", got, tc.score, tc.issues, tc.paths)
			}
		})
	}
}

func TestDimensionGatePrecedence(t *testing.T) {
	for _, in := range []DimensionInput{{Title: "Investigate outage"}, {Title: "Investigate cache"}} {
		a := AssessTaskDimensions(in)
		item := IssueSenseItem{SuggestedUpdate: &contract.WorkUnit{Gate: contract.GateNone}}
		applyDimensions(&item, a)
		want := contract.GateHuman
		if a.SuggestedDimensions.Risk == "urgent" {
			want = contract.GateAgent
		}
		if item.Gate != want {
			t.Fatalf("%+v: gate %s want %s", a, item.Gate, want)
		}
	}
	mixed := IssueSenseItem{SuggestedUpdate: &contract.WorkUnit{Gate: contract.GateNone}}
	applyDimensions(&mixed, DimensionAssessment{SuggestedDimensions: Dimensions{"small", "unknown", "urgent"}})
	if mixed.Gate != contract.GateAgent {
		t.Fatal("urgent risk must take precedence over unknown uncertainty", mixed.Gate)
	}

	// Scope notes explain coordination work, without changing dimensions.
	a := AssessTaskDimensions(DimensionInput{DependsOn: []int{1}, Overlaps: 1, Acceptance: make([]string, 6)})
	if a.SuggestedDimensions != (Dimensions{"small", "known", "standard"}) {
		t.Fatal(a)
	}
	for _, signal := range a.Signals {
		if !strings.HasPrefix(signal, "scope note:") {
			t.Fatal(signal)
		}
	}
}
