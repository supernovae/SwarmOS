package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
)

type projectionTracker struct {
	tracker.Tracker
	links          [][2]int
	orders         [][]int
	failProjection bool
	failHierarchy  bool
}

func (p *projectionTracker) LinkBlockedBy(_ context.Context, n, dep int) error {
	p.links = append(p.links, [2]int{n, dep})
	if p.failProjection {
		return errors.New("blocked-by denied")
	}
	return nil
}
func (p *projectionTracker) OrderSubIssues(_ context.Context, parent int, children []int) error {
	p.orders = append(p.orders, append([]int{parent}, children...))
	if p.failProjection {
		return errors.New("priority denied")
	}
	return nil
}
func (p *projectionTracker) LinkSubIssue(ctx context.Context, parent, child int) error {
	if p.failHierarchy {
		return errors.New("hierarchy denied")
	}
	return p.Tracker.LinkSubIssue(ctx, parent, child)
}

func TestNativeProjectionDecomposition(t *testing.T) {
	for _, sequential := range []bool{false, true} {
		t.Run(map[bool]string{false: "parallel", true: "sequential"}[sequential], func(t *testing.T) {
			root, eng, local := setupProjectPlanTestRepo(t)
			wu := &contract.WorkUnit{ID: "parent", Objective: "Deliver parent feature", Acceptance: []string{"First part works", "Second part works"}, Constraints: []string{}, Workflow: "feature", Gate: "none"}
			parent, err := local.CreateIssue(t.Context(), "parent", contract.FormatIssueMarkdown(wu), nil)
			if err != nil {
				t.Fatal(err)
			}
			p := &projectionTracker{Tracker: local, failProjection: true}
			eng.Tracker = p
			subs := []contract.WorkUnit{{Title: "one", Objective: "Deliver first part", Acceptance: []string{"First works"}, DependsOn: []int{77, 77}}, {Title: "two", Objective: "Deliver second part", Acceptance: []string{"Second works"}, DependsOn: []int{78}}}
			var warnings []string
			children, err := eng.DecomposeIssue(t.Context(), root, parent.Number, subs, sequential, func(s string) { warnings = append(warnings, s) })
			if err != nil || len(children) != 2 {
				t.Fatalf("children=%v err=%v", children, err)
			}
			want := [][2]int{{children[0].Number, 77}, {children[1].Number, 78}}
			if sequential {
				want = append(want, [2]int{children[1].Number, children[0].Number})
			}
			if !reflect.DeepEqual(p.links, want) || len(warnings) != len(want)+1 {
				t.Fatalf("links=%v warnings=%v", p.links, warnings)
			}
			if !reflect.DeepEqual(p.orders, [][]int{{parent.Number, children[0].Number, children[1].Number}}) {
				t.Fatal(p.orders)
			}
			stored, _ := local.GetIssue(t.Context(), children[0].Number)
			parsed, err := contract.ParseWorkUnitFromIssue(stored.Number, stored.Title, stored.Body)
			if err != nil || !reflect.DeepEqual(parsed.DependsOn, []int{77}) || stored.Body != children[0].Body {
				t.Fatalf("body changed: %v %v", parsed, err)
			}
			p.failHierarchy = true
			before := len(p.orders)
			count := len(p.links)
			if _, err := eng.DecomposeIssue(t.Context(), root, parent.Number, subs, sequential); err == nil || !strings.Contains(err.Error(), "link decomposed child issue") {
				t.Fatalf("hierarchy failure: %v", err)
			}
			if before != len(p.orders) || count != len(p.links) {
				t.Fatal("projected after hierarchy failure")
			}
		})
	}
}

const projectionRoadmap = "# Plan\n\n## First feature\n### Objective\nDeliver first feature\n### Acceptance Criteria\n- [ ] First feature works\n\n## Second feature\n### Objective\nDeliver second feature\n### Acceptance Criteria\n- [ ] Second feature works\ndepends on: 1\n"

func TestNativeProjectionRoadmapRetry(t *testing.T) {
	root, eng, local := setupProjectPlanTestRepo(t)
	path := filepath.Join(root, "roadmap.md")
	if err := os.WriteFile(path, []byte(projectionRoadmap), 0600); err != nil {
		t.Fatal(err)
	}
	p := &projectionTracker{Tracker: local, failProjection: true}
	eng.Tracker = p
	opts := ProjectPlanOptions{RepoRoot: root, FromFile: path, Apply: true}
	report, err := eng.RunProjectPlan(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.CreatedIssues) != 2 || len(report.Warnings) != 2 || len(p.links) != 1 || len(p.orders) != 1 {
		t.Fatalf("report=%+v links=%v orders=%v", report, p.links, p.orders)
	}
	first := report.CreatedIssues[0].Number
	second := report.CreatedIssues[1].Number
	if !reflect.DeepEqual(p.links, [][2]int{{second, first}}) {
		t.Fatal(p.links)
	}
	p.failProjection = false
	again, err := eng.RunProjectPlan(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.CreatedIssues) != 0 || len(again.ExistingIssues) != 2 || len(again.Warnings) != 0 || len(p.links) != 2 || len(p.orders) != 2 {
		t.Fatalf("retry %+v links=%v orders=%v", again, p.links, p.orders)
	}
	if !reflect.DeepEqual(p.orders[1], []int{report.EpicIssue.Number, first, second}) {
		t.Fatal(p.orders)
	}
}

func TestNativeProjectionExistingBodyAndSelf(t *testing.T) {
	local := tracker.NewLocalTracker(t.TempDir())
	p := &projectionTracker{Tracker: local}
	wu := &contract.WorkUnit{ID: "body", Objective: "Use existing body", Acceptance: []string{"Body used"}, Workflow: "feature", Gate: "none", DependsOn: []int{7, 7, 11, 8}}
	issue := &tracker.Issue{Number: 11, Title: "body", Body: contract.FormatIssueMarkdown(wu)}
	projectIssueDependencies(t.Context(), p, issue, nil)
	if !reflect.DeepEqual(p.links, [][2]int{{11, 7}, {11, 8}}) {
		t.Fatal(p.links)
	}
}

func TestNativeProjectionRoadmapHierarchySkipsReady(t *testing.T) {
	root, eng, local := setupProjectPlanTestRepo(t)
	path := filepath.Join(root, "roadmap.md")
	if err := os.WriteFile(path, []byte(projectionRoadmap), 0600); err != nil {
		t.Fatal(err)
	}
	p := &projectionTracker{Tracker: local, failHierarchy: true}
	eng.Tracker = p
	_, err := eng.RunProjectPlan(t.Context(), ProjectPlanOptions{RepoRoot: root, FromFile: path, Apply: true, Ready: true})
	if err == nil || !strings.Contains(err.Error(), "link created child issue") {
		t.Fatalf("%v", err)
	}
	issues, _ := local.ListIssues(t.Context(), tracker.IssueFilter{State: "all"})
	children := 0
	for _, issue := range issues {
		if issue.IsEpic() {
			continue
		}
		children++
		for _, label := range issue.Labels {
			if label == "swarm:ready" {
				t.Fatal("approved after hierarchy failure")
			}
		}
		if _, err := eng.LeaseManager.GetCurrentState(t.Context(), issue.Number); err == nil {
			t.Fatal("approval state created")
		}
	}
	if children != 1 || len(p.links) != 0 || len(p.orders) != 0 {
		t.Fatalf("children=%d links=%v orders=%v", children, p.links, p.orders)
	}
}
