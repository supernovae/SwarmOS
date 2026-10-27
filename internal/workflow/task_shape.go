package workflow

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
)

// TaskShape describes how much is already known about a piece of work, which
// decides how much rigor its plan needs. It is advisory: scan and lint suggest
// a shape from issue text, and the planner confirms or corrects it in PLAN.md.
// It is never stored on the WorkUnit.
type TaskShape string

const (
	ShapeRoutine     TaskShape = "routine"     // clear fix in a known place
	ShapeInvolved    TaskShape = "involved"    // knowable, but needs analysis across several areas
	ShapeExploratory TaskShape = "exploratory" // the right answer is not known yet
	ShapeUrgent      TaskShape = "urgent"      // users are affected now; stabilize first
)

// Title returns the display form used in reports and prompts.
func (s TaskShape) Title() string {
	switch s {
	case ShapeRoutine:
		return "Routine"
	case ShapeInvolved:
		return "Involved"
	case ShapeExploratory:
		return "Exploratory"
	case ShapeUrgent:
		return "Urgent"
	}
	return "-"
}

// ShapeInput is the issue information shape assessment reads.
type ShapeInput struct {
	Title       string
	Objective   string
	Acceptance  []string
	Workflow    string
	DependsOn   []int
	Touchpoints []string
	Overlaps    int
	Protected   bool
}

// ShapeAssessment is a suggested shape plus the plain-language signals behind
// it. Hints are suggestions for strengthening the issue; they never fail lint.
type ShapeAssessment struct {
	Shape   TaskShape `yaml:"shape" json:"shape"`
	Signals []string  `yaml:"signals,omitempty" json:"signals,omitempty"`
	Hints   []string  `yaml:"hints,omitempty" json:"hints,omitempty"`
}

var (
	urgentAnyWords    = wordPattern("outage", "data loss", "data corruption", "cve", "vulnerability", "exploit", "in production", "production down")
	urgentBugfixWords = wordPattern("regression", "crash", "crashes", "panic", "panics", "security", "broken", "hang", "hangs", "deadlock")
	exploratoryWords  = wordPattern("investigate", "explore", "spike", "evaluate", "research", "prototype", "figure out", "should we", "not sure", "unclear", "rfc")
	vagueWords        = wordPattern("improve", "improved", "better", "cleaner", "nicer", "faster", "simpler", "more robust", "user friendly", "user-friendly", "polish")
	checkableWords    = wordPattern("test", "tests", "returns", "exits", "fails", "passes", "prints", "outputs", "rejects", "accepts", "responds", "renders", "logs", "within", "under", "less than", "at most", "at least")
	reproWords        = wordPattern("repro", "reproduce", "reproduces", "reproduction", "regression test", "fails before", "failing test")
	digitPattern      = regexp.MustCompile(`\d`)
	regressionTests   = wordPattern("regression test", "regression tests")
)

func wordPattern(words ...string) *regexp.Regexp {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = regexp.QuoteMeta(w)
	}
	return regexp.MustCompile(`(?i)\b(` + strings.Join(quoted, "|") + `)\b`)
}

// AssessTaskShape suggests a shape for an issue. Urgent wins over Exploratory,
// which wins over Involved; anything else is Routine.
func AssessTaskShape(in ShapeInput) ShapeAssessment {
	text := in.Title + "\n" + in.Objective
	var urgent, exploratory, involved []string

	// "Add a regression test" describes good practice, not a live regression.
	urgencyText := regressionTests.ReplaceAllString(text, "")
	if m := urgentAnyWords.FindString(urgencyText); m != "" {
		urgent = append(urgent, fmt.Sprintf("mentions %q", strings.ToLower(m)))
	} else if in.Workflow == "bugfix" {
		if m := urgentBugfixWords.FindString(urgencyText); m != "" {
			urgent = append(urgent, fmt.Sprintf("bugfix that mentions %q", strings.ToLower(m)))
		}
	}

	if m := exploratoryWords.FindString(text); m != "" {
		exploratory = append(exploratory, fmt.Sprintf("objective says %q", strings.ToLower(m)))
	} else if strings.HasSuffix(strings.TrimSpace(in.Title), "?") {
		exploratory = append(exploratory, "title is a question")
	}
	vague, checkable := 0, 0
	for _, a := range in.Acceptance {
		switch {
		case vagueWords.MatchString(a) && !hasConcreteAnchor(a):
			vague++
		case hasConcreteAnchor(a) || checkableWords.MatchString(a):
			checkable++
		}
	}
	if len(in.Acceptance) > 0 && vague == len(in.Acceptance) {
		exploratory = append(exploratory, "every acceptance criterion is open-ended (\"improve\", \"better\")")
	}

	if areas := touchpointAreas(in.Touchpoints); areas >= 3 {
		involved = append(involved, fmt.Sprintf("touches %d separate areas", areas))
	}
	if in.Overlaps > 0 {
		involved = append(involved, fmt.Sprintf("overlaps with %d other open issue(s)", in.Overlaps))
	}
	if len(in.DependsOn) > 0 {
		involved = append(involved, "depends on other issues")
	}
	if in.Protected {
		involved = append(involved, "touches protected harness or CI paths")
	}
	if len(in.Acceptance) >= 6 {
		involved = append(involved, fmt.Sprintf("%d acceptance criteria", len(in.Acceptance)))
	}

	var out ShapeAssessment
	switch {
	case len(urgent) > 0:
		out.Shape, out.Signals = ShapeUrgent, urgent
	case len(exploratory) > 0:
		out.Shape, out.Signals = ShapeExploratory, exploratory
	case len(involved) > 0:
		out.Shape, out.Signals = ShapeInvolved, involved
	default:
		out.Shape = ShapeRoutine
	}

	if len(in.Acceptance) > 0 && checkable == 0 {
		out.Hints = append(out.Hints, "No acceptance criterion names a command, test, or observable result; add one so reviewers can check the outcome.")
	} else if vague > 0 && vague < len(in.Acceptance) {
		out.Hints = append(out.Hints, fmt.Sprintf("%d acceptance criteria are open-ended (\"improve\", \"better\"); say how the improvement will be measured.", vague))
	}
	if in.Workflow == "bugfix" && !reproWords.MatchString(text+"\n"+strings.Join(in.Acceptance, "\n")) {
		out.Hints = append(out.Hints, "Bugfix has no reproduction step or failing test; add one so the fix can be confirmed.")
	}
	if touchpointAreas(in.Touchpoints) >= 3 {
		out.Hints = append(out.Hints, "Spans several areas; consider `swarm issue decompose` so parallel work merges cleanly.")
	}
	switch out.Shape {
	case ShapeExploratory:
		out.Hints = append(out.Hints, "The answer isn't known yet; write down the open question so the plan can start with a small probe.")
	case ShapeUrgent:
		out.Hints = append(out.Hints, "Users may be affected now; keep this task to the smallest mitigation and file the root cause separately.")
	}
	return out
}

// WorkUnitTouchpoints derives a WorkUnit's likely repository-relative
// touchpoints from its title, objective, and acceptance criteria, the same way
// backlog scans and shape assessment do.
func WorkUnitTouchpoints(repoRoot string, wu *contract.WorkUnit) []string {
	if wu == nil {
		return nil
	}
	return extractTouchpoints(repoRoot, wu.Title+"\n"+wu.Objective+"\n"+strings.Join(wu.Acceptance, "\n"))
}

// TouchpointOverlap is an advisory measure of how much a candidate's likely
// touchpoints collide with work already in flight. It never blocks a claim.
type TouchpointOverlap struct {
	Score  int      `json:"score"`
	Issues []int    `json:"overlaps_with,omitempty"`
	Paths  []string `json:"paths,omitempty"`
}

// AssessTouchpointOverlap scores a candidate by the number of distinct
// in-flight issues whose touchpoints overlap its own. Touchpoints overlap when
// they are equal, one is a directory ancestor of the other, or both are Go
// files in the same package directory. Paths lists the candidate touchpoints
// that matched, so callers can explain the score.
func AssessTouchpointOverlap(candidate []string, inFlight map[int][]string) TouchpointOverlap {
	var out TouchpointOverlap
	paths := make(map[string]bool)
	for issue, tps := range inFlight {
		matched := false
		for _, x := range candidate {
			for _, y := range tps {
				if touchpointsOverlap(x, y) {
					matched, paths[normalizeTouchpoint(x)] = true, true
				}
			}
		}
		if matched {
			out.Issues = append(out.Issues, issue)
		}
	}
	sort.Ints(out.Issues)
	for p := range paths {
		out.Paths = append(out.Paths, p)
	}
	sort.Strings(out.Paths)
	out.Score = len(out.Issues)
	return out
}

func normalizeTouchpoint(p string) string {
	return path.Clean(strings.ReplaceAll(p, "\\", "/"))
}

func touchpointsOverlap(a, b string) bool {
	a, b = normalizeTouchpoint(a), normalizeTouchpoint(b)
	for _, p := range []string{a, b} {
		if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
			return false
		}
	}
	if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
		return true
	}
	return path.Ext(a) == ".go" && path.Ext(b) == ".go" && path.Dir(a) == path.Dir(b)
}

// AssessWorkUnitShape assesses a parsed WorkUnit, deriving touchpoints from its
// text the same way backlog scans do.
func AssessWorkUnitShape(repoRoot string, wu *contract.WorkUnit, spec *contract.ValidationSpec) ShapeAssessment {
	tps := WorkUnitTouchpoints(repoRoot, wu)
	return AssessTaskShape(ShapeInput{
		Title:       wu.Title,
		Objective:   wu.Objective,
		Acceptance:  wu.Acceptance,
		Workflow:    wu.Workflow,
		DependsOn:   wu.DependsOn,
		Touchpoints: tps,
		Protected:   touchesProtectedPath(tps, spec) && !wu.AllowHarnessChanges,
	})
}

// hasConcreteAnchor reports whether a criterion names a command or a number.
func hasConcreteAnchor(s string) bool {
	return strings.Contains(s, "`") || digitPattern.MatchString(s)
}

// touchpointAreas counts distinct top-level areas, treating each package under
// cmd/, internal/, and pkg/ as its own area.
func touchpointAreas(tps []string) int {
	areas := make(map[string]bool)
	for _, tp := range tps {
		parts := strings.Split(tp, "/")
		area := parts[0]
		if len(parts) > 2 && (area == "cmd" || area == "internal" || area == "pkg" || area == "src") {
			area += "/" + parts[1]
		}
		areas[area] = true
	}
	return len(areas)
}

// shapeNextStep is a short, shape-aware note appended to a backlog row's next action.
func shapeNextStep(s TaskShape) string {
	switch s {
	case ShapeRoutine:
		return "good first task"
	case ShapeInvolved:
		return "plan should weigh alternatives"
	case ShapeExploratory:
		return "start with a probe"
	case ShapeUrgent:
		return "stabilize first"
	}
	return ""
}

// applyShape records an assessment on a backlog item. Exploratory raw issues
// get a human plan gate in their suggested WorkUnit, and Urgent ones at least an
// agent gate; the maintainer still reviews and approves the suggestion.
func applyShape(item *IssueSenseItem, a ShapeAssessment) {
	item.Shape, item.ShapeSignals, item.ShapeHints = a.Shape, a.Signals, a.Hints
	if item.SuggestedUpdate == nil {
		return
	}
	switch {
	case a.Shape == ShapeExploratory:
		item.SuggestedUpdate.Gate = contract.GateHuman
	case a.Shape == ShapeUrgent && (item.SuggestedUpdate.Gate == "" || item.SuggestedUpdate.Gate == contract.GateNone):
		item.SuggestedUpdate.Gate = contract.GateAgent
	}
	item.Gate = item.SuggestedUpdate.Gate
}

// renderShapeTally summarizes suggested shapes in one report line.
func renderShapeTally(items []IssueSenseItem) string {
	counts := make(map[TaskShape]int)
	for _, it := range items {
		counts[it.Shape]++
	}
	var parts []string
	for _, s := range []TaskShape{ShapeRoutine, ShapeInvolved, ShapeExploratory, ShapeUrgent} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("`%d` %s", counts[s], strings.ToLower(s.Title())))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "- **Suggested Shapes**: " + strings.Join(parts, ", ") + " (planners confirm the shape when they read the code)\n"
}
