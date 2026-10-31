package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/tracker"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

// dimensionLines returns the "Suggested dimensions" line and the hints after it.
func dimensionLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Suggested dimensions:") || strings.HasPrefix(line, "  hint: ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestApprovalPrintsLintDimensionsBeforeApproving(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	acceptance := []string{"Update `internal/alpha/a.go`", "Update `internal/beta/b.go`", "Update `cmd/gamma/main.go`", "Document it", "Keep it tidy", "Make it nicer"}
	args := []string{"issue", "create", "--title", "Oversized task", "--objective", "Change many areas", "--constraint", "Keep policy unchanged", "--gate", "agent"}
	for _, a := range acceptance {
		args = append(args, "--acceptance", a)
	}
	f.must(t, args...)
	oversized := strconv.Itoa(f.create(t, "agent") - 1)

	lint := dimensionLines(f.must(t, "issue", "lint", oversized))
	if len(lint) < 2 || !slices.ContainsFunc(lint, func(l string) bool { return strings.Contains(l, "swarm issue decompose") }) {
		t.Fatalf("fixture should carry a decomposition hint: %v", lint)
	}
	out := f.must(t, "issue", "approve", oversized)
	if got := dimensionLines(out); !slices.Equal(got, lint) {
		t.Fatalf("approval advice %v differs from lint %v", got, lint)
	}
	if advice, approved := strings.Index(out, "Suggested dimensions:"), strings.Index(out, "Approved issue"); advice < 0 || approved < advice {
		t.Fatalf("advice must precede approval:\n%s", out)
	}
	if f.state(t, mustAtoi(t, oversized)).TaskDigest == "" {
		t.Fatal("decomposition hint blocked approval")
	}

	if strings.Count(out, "Suggested dimensions:") != 1 || strings.Contains(out, "Suggested shape:") || strings.Contains(out, "{Size:") {
		t.Fatal(out)
	}
	if !strings.Contains(out, "size=multi_area uncertainty=known risk=standard") || !strings.Contains(out, "scope note: 6 acceptance criteria") {
		t.Fatal(out)
	}
	lintJSON := f.must(t, "issue", "lint", oversized, "--json")
	var advice map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lintJSON), &advice); err != nil {
		t.Fatal(err)
	}
	if _, ok := advice["shape"]; ok {
		t.Fatal(lintJSON)
	}
	if len(advice["signals"]) == 0 || len(advice["suggested_dimensions"]) == 0 {
		t.Fatal(lintJSON)
	}
	var lintResult struct {
		WorkUnit contract.WorkUnit `json:"work_unit"`
	}
	if err := json.Unmarshal([]byte(lintJSON), &lintResult); err != nil {
		t.Fatal(err)
	}
	if lintResult.WorkUnit.Size != "" || lintResult.WorkUnit.Uncertainty != "" || lintResult.WorkUnit.Risk != "" {
		t.Fatal("advice filled absent dimensions")
	}

	f.must(t, "issue", "edit", oversized, "--size", "small", "--uncertainty", "unknown", "--risk", "sensitive")
	f.must(t, "issue", "approve", oversized)
	recorded := f.must(t, "issue", "lint", oversized, "--json")
	if err := json.Unmarshal([]byte(recorded), &lintResult); err != nil {
		t.Fatal(err)
	}
	if lintResult.WorkUnit.Size != "small" || lintResult.WorkUnit.Uncertainty != "unknown" || lintResult.WorkUnit.Risk != "sensitive" {
		t.Fatal("advice changed maintainer dimensions")
	}
	if !strings.Contains(f.must(t, "issue", "lint", oversized), "Suggested dimensions: size=multi_area uncertainty=known risk=standard") {
		t.Fatal("advice depends on recorded dimensions")
	}

	// JSON keeps one document on stdout, with the shape added, and the
	// human advice on stderr.
	routine := strconv.Itoa(f.create(t, "agent"))
	stdout, stderr, err := f.runSplit("issue", "approve", routine, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Kind                string `json:"kind"`
		SuggestedDimensions *struct {
			Size string `json:"size"`
		} `json:"suggested_dimensions"`
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&result); err != nil || result.Kind != "task-approval" || result.SuggestedDimensions == nil || result.SuggestedDimensions.Size == "" || dec.More() {
		t.Fatalf("approval JSON: %s (%v)", stdout, err)
	}
	if !strings.Contains(stderr, "Suggested dimensions:") || strings.Contains(stdout, "Suggested dimensions:") {
		t.Fatalf("advice belongs on stderr in JSON mode:\nstdout=%s\nstderr=%s", stdout, stderr)
	}

	// Approval stays authoritative: advice never turns a rejection into
	// success.
	if out, err := f.run("issue", "approve", "999"); err == nil || !strings.Contains(out, "Suggested dimensions: unavailable") {
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

func TestDecompositionPreviewAndStatusCLI(t *testing.T) {
	f := newCLIFixture(t)
	wu := &contract.WorkUnit{ID: "parent", Objective: "Deliver feature", Acceptance: []string{"Core works", "Integration works"}, Workflow: "feature", Risk: "sensitive", AllowHarnessChanges: true}
	parent, err := f.tracker.CreateIssue(t.Context(), "parent", contract.FormatIssueMarkdown(wu), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := f.run("issue", "approve", strconv.Itoa(parent.Number)); err != nil {
		t.Fatalf("approve: %v %s", err, out)
	}
	opts := f.options(&RootOptions{RepoRoot: f.root, Backend: "local"})
	cmd := newIssueCmd(opts)
	_, m, _, err := opts.resolveLeaseManager(cmd)
	if err != nil {
		t.Fatal(err)
	}
	before, rev, err := m.Store().Load(t.Context(), parent.Number)
	if err != nil {
		t.Fatal(err)
	}
	issues, _ := f.tracker.ListIssues(t.Context(), tracker.IssueFilter{State: "all"})
	comments, _ := f.tracker.ListComments(t.Context(), parent.Number)
	for _, jsonFlag := range []bool{false, true} {
		args := []string{"issue", "decompose", strconv.Itoa(parent.Number), "--dry-run"}
		if jsonFlag {
			args = append(args, "--json")
		}
		out, _, err := f.runSplit(args...)
		if err != nil {
			t.Fatalf("preview: %v %s", err, out)
		}
		for _, field := range []string{"sensitive", "allow_harness_changes"} {
			if !strings.Contains(out, field) {
				t.Fatalf("missing %s: %s", field, out)
			}
		}
	}
	after, rev2, _ := m.Store().Load(t.Context(), parent.Number)
	issues2, _ := f.tracker.ListIssues(t.Context(), tracker.IssueFilter{State: "all"})
	comments2, _ := f.tracker.ListComments(t.Context(), parent.Number)
	if string(before) != string(after) || rev != rev2 || !reflect.DeepEqual(issues, issues2) || !reflect.DeepEqual(comments, comments2) {
		t.Fatal("CLI preview wrote state")
	}
	if out, err := f.run("issue", "decompose", strconv.Itoa(parent.Number)); err != nil {
		t.Fatalf("decompose: %v %s", err, out)
	}
	for _, jsonFlag := range []bool{false, true} {
		args := []string{"issue", "status", strconv.Itoa(parent.Number)}
		if jsonFlag {
			args = append(args, "--json")
		}
		out, err := f.run(args...)
		if err != nil {
			t.Fatalf("status: %v %s", err, out)
		}
		if !strings.Contains(out, "approval needed") || !strings.Contains(out, "waiting on prerequisite") {
			t.Fatal(out)
		}
	}
}

type decompositionStatusTracker struct {
	tracker.Tracker
	missing int
	prs     map[int]*tracker.PullRequest
}

func (tr *decompositionStatusTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	if tr.missing != 0 && n != tr.missing {
		return nil, fmt.Errorf("tracker read unavailable")
	}
	return tr.Tracker.GetIssue(ctx, n)
}
func (tr *decompositionStatusTracker) GetPullRequest(_ context.Context, n int) (*tracker.PullRequest, error) {
	return tr.prs[n], nil
}
func (tr *decompositionStatusTracker) UpstreamRepo() string { return "octo/swarm" }

func TestDecompositionStatusMatrix(t *testing.T) {
	for _, scenario := range []string{"approved root", "parallel branches", "all merged", "tracker read failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCLIFixture(t)
			wu := &contract.WorkUnit{ID: "parent", Objective: "Deliver feature", Acceptance: []string{"Core works", "Integration works"}, Workflow: "feature"}
			parent, err := f.tracker.CreateIssue(t.Context(), "parent", contract.FormatIssueMarkdown(wu), nil)
			if err != nil {
				t.Fatal(err)
			}
			if out, err := f.run("issue", "approve", strconv.Itoa(parent.Number)); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			args := []string{"issue", "decompose", strconv.Itoa(parent.Number)}
			if scenario == "parallel branches" {
				args = append(args, "--sequential=false")
			}
			if out, err := f.run(args...); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			opts := f.options(&RootOptions{RepoRoot: f.root, Backend: "local"})
			_, m, _, err := opts.resolveLeaseManager(newIssueCmd(opts))
			if err != nil {
				t.Fatal(err)
			}
			st, err := m.GetCurrentState(t.Context(), parent.Number)
			if err != nil {
				t.Fatal(err)
			}
			children := st.Decomposition.Children
			tr := &decompositionStatusTracker{Tracker: f.tracker, prs: map[int]*tracker.PullRequest{}}
			for i, c := range children {
				if scenario == "approved root" && i > 0 {
					continue
				}
				if scenario == "tracker read failure" {
					tr.missing = parent.Number
					continue
				}
				if out, err := f.run("issue", "approve", strconv.Itoa(c.Number)); err != nil {
					t.Fatalf("%v %s", err, out)
				}
				if scenario == "all merged" {
					raw, rev, err := m.Store().Load(t.Context(), c.Number)
					if err != nil {
						t.Fatal(err)
					}
					var child lease.IssueState
					if err = json.Unmarshal(raw, &child); err != nil {
						t.Fatal(err)
					}
					head := strings.Repeat("a", 40)
					now := time.Now().UTC()
					child.PRNumber = c.Number
					child.PRURL = fmt.Sprintf("https://github.com/octo/swarm/pull/%d", c.Number)
					child.Branch = fmt.Sprintf("swarm/issue-%d-child", c.Number)
					child.BaseBranch = "main"
					child.SubmittedHeadSHA = head
					child.Lease.Status = "completed"
					raw, _ = json.Marshal(child)
					if _, err = m.Store().CompareAndSwap(t.Context(), c.Number, rev, raw); err != nil {
						t.Fatal(err)
					}
					tr.prs[c.Number] = &tracker.PullRequest{Number: c.Number, HTMLURL: child.PRURL, Head: child.Branch, HeadSHA: head, Base: "main", HeadRepo: "octo/swarm", BaseRepo: "octo/swarm", State: "closed", Merged: true, MergeCommitSHA: strings.Repeat("b", 40), MergedAt: &now, IssueNumber: c.Number}
				}
			}
			for _, asJSON := range []bool{false, true} {
				opts := f.options(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: tr})
				cmd := newStatusCmd(opts)
				var buf bytes.Buffer
				cmd.SetOut(&buf)
				args := []string{strconv.Itoa(parent.Number)}
				if asJSON {
					args = append(args, "--json")
				}
				cmd.SetArgs(args)
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
				out := buf.String()
				if asJSON {
					var result struct {
						Issues []struct {
							Decomposition *lease.DecompositionStatus `json:"decomposition"`
						} `json:"issues"`
					}
					if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if len(result.Issues) != 1 || result.Issues[0].Decomposition == nil {
						t.Fatal(out)
					}
					d := result.Issues[0].Decomposition
					if len(d.Children) != len(children) {
						t.Fatal(out)
					}
					for i, row := range d.Children {
						if row.Number != children[i].Number {
							t.Fatal("lost dependency order", out)
						}
					}
					switch scenario {
					case "approved root", "parallel branches":
						if d.Next != children[0].Number || d.NextState != "workable" || d.AllMerged {
							t.Fatal(out)
						}
						if scenario == "parallel branches" && d.Children[1].State != "workable" {
							t.Fatal(out)
						}
						if scenario == "approved root" && d.Children[1].State != "waiting on prerequisite" {
							t.Fatal(out)
						}
					case "all merged":
						if !d.AllMerged || d.Next != 0 {
							t.Fatal(out)
						}
						for _, row := range d.Children {
							if row.State != "merged" {
								t.Fatal(out)
							}
						}
					case "tracker read failure":
						if d.AllMerged || d.Next != 0 {
							t.Fatal(out)
						}
						for _, row := range d.Children {
							if row.State != "unknown" || !strings.Contains(row.Reason, "tracker read unavailable") {
								t.Fatal(out)
							}
						}
					}
				} else {
					switch scenario {
					case "approved root", "parallel branches":
						if !strings.Contains(out, fmt.Sprintf("Next child #%d: workable", children[0].Number)) {
							t.Fatal(out)
						}
						if scenario == "parallel branches" && !strings.Contains(out, fmt.Sprintf("#%d %s: workable", children[1].Number, children[1].WorkUnit.Title)) {
							t.Fatal(out)
						}
						if scenario == "approved root" && !strings.Contains(out, "waiting on prerequisite") {
							t.Fatal(out)
						}
					case "all merged":
						if !strings.Contains(out, "All children merged; reconcile parent to complete.") || strings.Contains(out, "Next child") {
							t.Fatal(out)
						}
					case "tracker read failure":
						if !strings.Contains(out, "unknown tracker read unavailable") || !strings.Contains(out, "No child currently workable") || strings.Contains(out, "Next child") {
							t.Fatal(out)
						}
					}
				}
			}
		})
	}
}
