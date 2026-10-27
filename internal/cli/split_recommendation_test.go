package cli

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
)

// cliSplitPlan is appended to mock plans through SWARM_MOCK_PLAN_APPEND. The
// mock agent reads that variable from the process environment, not the
// fixture lookup, so tests that set it stay serial.
const cliSplitPlan = "Split recommendation: required\n\n### Child one\n\n**Objective**: First half.\n\n**Acceptance Criteria**\n- [ ] `go test ./a` passes"

// runSplit runs swarm and returns stdout and stderr separately.
func (f *cliFixture) runSplit(args ...string) (string, string, error) {
	cmd := f.command(nil)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

// shapeLines returns the "Suggested shape" line and the hints after it.
func shapeLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Suggested shape:") || strings.HasPrefix(line, "  hint: ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestApprovalPrintsLintShapeBeforeApproving(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	acceptance := []string{"Update `internal/alpha/a.go`", "Update `internal/beta/b.go`", "Update `cmd/gamma/main.go`", "Document it", "Keep it tidy", "Make it nicer"}
	args := []string{"issue", "create", "--title", "Oversized task", "--objective", "Change many areas", "--constraint", "Keep policy unchanged", "--gate", "agent"}
	for _, a := range acceptance {
		args = append(args, "--acceptance", a)
	}
	f.must(t, args...)
	oversized := strconv.Itoa(f.create(t, "agent") - 1)

	lint := shapeLines(f.must(t, "issue", "lint", oversized))
	if len(lint) < 2 || !slices.ContainsFunc(lint, func(l string) bool { return strings.Contains(l, "swarm issue decompose") }) {
		t.Fatalf("fixture should carry a decomposition hint: %v", lint)
	}
	out := f.must(t, "issue", "approve", oversized)
	if got := shapeLines(out); !slices.Equal(got, lint) {
		t.Fatalf("approval advice %v differs from lint %v", got, lint)
	}
	if advice, approved := strings.Index(out, "Suggested shape:"), strings.Index(out, "Approved issue"); advice < 0 || approved < advice {
		t.Fatalf("advice must precede approval:\n%s", out)
	}
	if f.state(t, mustAtoi(t, oversized)).TaskDigest == "" {
		t.Fatal("decomposition hint blocked approval")
	}

	// JSON keeps one document on stdout, with the shape added, and the
	// human advice on stderr.
	routine := strconv.Itoa(f.create(t, "agent"))
	stdout, stderr, err := f.runSplit("issue", "approve", routine, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Kind  string `json:"kind"`
		Shape *struct {
			Shape string `json:"shape"`
		} `json:"shape"`
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&result); err != nil || result.Kind != "task-approval" || result.Shape == nil || result.Shape.Shape == "" || dec.More() {
		t.Fatalf("approval JSON: %s (%v)", stdout, err)
	}
	if !strings.Contains(stderr, "Suggested shape:") || strings.Contains(stdout, "Suggested shape:") {
		t.Fatalf("advice belongs on stderr in JSON mode:\nstdout=%s\nstderr=%s", stdout, stderr)
	}

	// Approval stays authoritative: advice never turns a rejection into
	// success.
	if out, err := f.run("issue", "approve", "999"); err == nil || !strings.Contains(out, "Suggested shape: unavailable") {
		t.Fatalf("missing issue: advice should be unavailable and approval should fail: %v\n%s", err, out)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func assertNoSplitProgress(t *testing.T, st *lease.IssueState) {
	t.Helper()
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactReview || a.Type == contract.ArtifactImplementation || a.Verdict == "FAIL" {
			t.Fatalf("split stop recorded %s (%s)", a.Type, a.Verdict)
		}
	}
}

func TestIssueWorkStopsOnSplitRecommendation(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	t.Setenv("SWARM_MOCK_PLAN_APPEND", cliSplitPlan)
	out := f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
	for _, want := range []string{"split-recommended: phase=plan", "The planner recommends a split", "swarm --backend 'local' --root '" + f.root + "' issue decompose " + n} {
		if !strings.Contains(out, want) {
			t.Fatalf("issue work output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "review-plan") {
		t.Fatalf("split stop should not suggest plan review:\n%s", out)
	}
	assertNoSplitProgress(t, f.state(t, number))

	// Resuming with the recorded plan stops again, and JSON reports it.
	raw := f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false", "--json")
	var outcome struct {
		Status string `json:"status"`
		Result struct {
			SplitRecommended bool   `json:"split_recommended"`
			DecomposeCommand string `json:"decompose_command"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &outcome); err != nil || outcome.Status != "split-recommended" || !outcome.Result.SplitRecommended || !strings.HasSuffix(outcome.Result.DecomposeCommand, "issue decompose "+n) {
		t.Fatalf("issue work JSON: %s (%v)", raw, err)
	}
	assertNoSplitProgress(t, f.state(t, number))
}

func TestPRWorkSplitRecommendationNamesLinkedIssue(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	t.Setenv("SWARM_MOCK_PLAN_APPEND", cliSplitPlan)
	raw := f.must(t, "pr", "work", strconv.Itoa(number), "--agent", "mock", "--progress=false", "--json")
	result := readPROutcome(t, raw)
	if result.Status != "split-recommended" || result.Work == nil || !result.Work.SplitRecommended || !strings.HasSuffix(result.Work.DecomposeCommand, "issue decompose "+strconv.Itoa(issue)) {
		t.Fatalf("pr work JSON: %s", raw)
	}
	assertNoSplitProgress(t, f.state(t, issue))
	out := f.must(t, "pr", "work", strconv.Itoa(number), "--agent", "mock", "--progress=false")
	if !strings.Contains(out, "The planner recommends a split") || !strings.Contains(out, "issue decompose "+strconv.Itoa(issue)) || strings.Contains(out, "issue decompose "+strconv.Itoa(number)) {
		t.Fatalf("pr work guidance must name the linked issue #%d, not PR #%d:\n%s", issue, number, out)
	}
}

func TestNodeRunStopsOnSplitRecommendationWithoutSubmitting(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	t.Setenv("SWARM_MOCK_PLAN_APPEND", cliSplitPlan)
	out := f.must(t, "node", "run", "--agent", "mock", "--max-jobs", "1", "--auto-submit")
	if !strings.Contains(out, "#"+n+" split-recommended: phase=plan") || !strings.Contains(out, "issue decompose "+n) {
		t.Fatalf("node output:\n%s", out)
	}
	st := f.state(t, number)
	if st.PRNumber != 0 {
		t.Fatal("split stop auto-submitted a PR")
	}
	assertNoSplitProgress(t, st)
}

func TestNodeRunSplitRecommendationJSON(t *testing.T) {
	f := newCLIFixture(t)
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	t.Setenv("SWARM_MOCK_PLAN_APPEND", cliSplitPlan)
	stdout, _, err := f.runSplit("node", "run", "--agent", "mock", "--max-jobs", "1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var outcome struct {
		Status string `json:"status"`
		Splits []struct {
			IssueNumber      int    `json:"issue_number"`
			DecomposeCommand string `json:"decompose_command"`
		} `json:"split_recommendations"`
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&outcome); err != nil || dec.More() || outcome.Status != "drained" || len(outcome.Splits) != 1 || !strings.HasSuffix(outcome.Splits[0].DecomposeCommand, "issue decompose "+n) {
		t.Fatalf("node JSON: %s (%v)", stdout, err)
	}
}

func TestNormalPlansProceedWithoutSplit(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	out := f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
	if strings.Contains(out, "split") || !strings.Contains(out, "completed") {
		t.Fatalf("normal plan should run through implementation:\n%s", out)
	}
	if f.state(t, number).TaskDigest == "" {
		t.Fatal("approval lost")
	}
}
