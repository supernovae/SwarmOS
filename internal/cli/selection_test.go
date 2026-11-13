package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/registry"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"gopkg.in/yaml.v3"
)

type orderingTracker struct {
	tracker.Tracker
	parents                     map[int]int
	children                    map[int][]*tracker.Issue
	parentErr, typeErr, listErr error
	calls                       map[int]int
	nonEpic                     bool
}

func (f *orderingTracker) ParentIssueNumber(_ context.Context, n int) (int, error) {
	return f.parents[n], f.parentErr
}
func (f *orderingTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	if f.Tracker != nil {
		return f.Tracker.GetIssue(ctx, n)
	}
	typ := "Epic"
	if f.nonEpic {
		typ = ""
	}
	return &tracker.Issue{Number: n, Type: typ}, f.typeErr
}
func (f *orderingTracker) ListSubIssues(_ context.Context, n int) ([]*tracker.Issue, error) {
	f.calls[n]++
	return f.children[n], f.listErr
}
func numbered(ns ...int) []*tracker.Issue {
	out := make([]*tracker.Issue, 0, len(ns))
	for _, n := range ns {
		out = append(out, &tracker.Issue{Number: n})
	}
	return out
}
func issueNumbers(issues []*tracker.Issue) []int {
	out := make([]int, 0, len(issues))
	for _, issue := range issues {
		out = append(out, issue.Number)
	}
	return out
}
func TestReadyIssueOrdering(t *testing.T) {
	for _, tc := range []struct {
		name        string
		input, want []int
		parents     map[int]int
		children    map[int][]*tracker.Issue
		nonEpic     bool
	}{
		{"empty", nil, []int{}, nil, nil, false},
		{"singleton", []int{10}, []int{10}, nil, nil, false},
		{"oldest", []int{30, 10, 20}, []int{10, 20, 30}, nil, nil, false},
		{"mixed", []int{30, 20, 10}, []int{30, 20, 10}, map[int]int{10: 100, 30: 100}, map[int][]*tracker.Issue{100: numbered(99, 30, 10, 88)}, false},
		{"independent epics", []int{40, 30, 20, 10}, []int{30, 40, 10, 20}, map[int]int{10: 100, 30: 100, 20: 200, 40: 200}, map[int][]*tracker.Issue{100: numbered(30, 10), 200: numbered(40, 20)}, false},
		{"non epic", []int{30, 10}, []int{10, 30}, map[int]int{10: 100, 30: 100}, map[int][]*tracker.Issue{100: numbered(30, 10)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &orderingTracker{parents: tc.parents, children: tc.children, calls: map[int]int{}, nonEpic: tc.nonEpic}
			var diagnostics bytes.Buffer
			input := numbered(tc.input...)
			got, err := orderReadyIssues(context.Background(), f, input, &diagnostics)
			if err != nil || !reflect.DeepEqual(issueNumbers(got), tc.want) || diagnostics.Len() != 0 {
				t.Fatalf("got %v, %v, %s", issueNumbers(got), err, &diagnostics)
			}
			if !reflect.DeepEqual(issueNumbers(input), issueNumbers(numbered(tc.input...))) {
				t.Fatal("mutated input")
			}
			for _, count := range f.calls {
				if count != 1 {
					t.Fatal("repeated sibling read")
				}
			}
		})
	}
}
func TestReadyIssueOrderingReadFailure(t *testing.T) {
	for _, mode := range []string{"parent", "type", "list", "missing", "duplicate", "nil"} {
		t.Run(mode, func(t *testing.T) {
			f := &orderingTracker{parents: map[int]int{10: 100, 30: 100}, children: map[int][]*tracker.Issue{100: numbered(30, 10)}, calls: map[int]int{}}
			outage := errors.New("offline")
			switch mode {
			case "parent":
				f.parentErr = outage
			case "type":
				f.typeErr = outage
			case "list":
				f.listErr = outage
			case "missing":
				f.children[100] = numbered(30)
			case "duplicate":
				f.children[100] = numbered(30, 10, 30)
			case "nil":
				f.children[100] = append(numbered(30, 10), nil)
			}
			var diagnostics bytes.Buffer
			got, err := orderReadyIssues(context.Background(), f, numbered(30, 20, 10), &diagnostics)
			if err != nil || !reflect.DeepEqual(issueNumbers(got), []int{10, 20, 30}) || !strings.Contains(diagnostics.String(), "warning:") {
				t.Fatalf("%v %v %s", issueNumbers(got), err, &diagnostics)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var diagnostics bytes.Buffer
	if _, err := orderReadyIssues(ctx, &orderingTracker{}, numbered(10), &diagnostics); !errors.Is(err, context.Canceled) || diagnostics.Len() != 0 {
		t.Fatalf("cancellation: %v %s", err, &diagnostics)
	}
}

func TestNextSelectionEntryPoints(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"claim", "work", "node", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			ctx := context.Background()
			epic, err := f.tracker.CreateTypedIssue(ctx, "Epic", "", nil, "Epic")
			if err != nil {
				t.Fatal(err)
			}
			first := f.create(t, "agent")
			second := f.create(t, "agent")
			for _, n := range []int{first, second} {
				f.must(t, "issue", "approve", strconv.Itoa(n))
			}
			for _, n := range []int{second, first} {
				if err := f.tracker.LinkSubIssue(ctx, epic.Number, n); err != nil {
					t.Fatal(err)
				}
			}
			var tr tracker.Tracker = f.tracker
			want := second
			if mode == "fallback" {
				tr = &orderingTracker{Tracker: f.tracker, parents: map[int]int{first: epic.Number, second: epic.Number}, listErr: errors.New("offline"), calls: map[int]int{}}
				want = first
			}
			cmd := f.command(&RootOptions{RepoRoot: f.root, trackerOverride: tr})
			var output, diagnostics bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&diagnostics)
			args := []string{"issue", "claim", "--next", "--agent", "mock", "--json"}
			if mode == "work" {
				args = []string{"issue", "work", "--next", "--agent", "mock", "--headless", "--progress=false"}
			}
			if mode == "node" {
				args = []string{"node", "run", "--max-jobs", "1", "--agent", "mock"}
			}
			cmd.SetArgs(append([]string{"--root", f.root}, args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%v %s %s", err, &output, &diagnostics)
			}
			if f.state(t, want).Lease.ClaimID == "" {
				t.Fatal("expected sibling was not selected")
			}
			other := first
			if want == first {
				other = second
			}
			if f.state(t, other).Lease.ClaimID != "" {
				t.Fatal("selected extra candidate")
			}
			if mode == "fallback" {
				if !strings.Contains(diagnostics.String(), "warning:") || !json.Valid(output.Bytes()) {
					t.Fatalf("warning/output: %s %s", &diagnostics, &output)
				}
			}
		})
	}
}

// Repository traversal retains the registry's existing order, regardless of
// relative issue numbers in different repositories.
func TestNextSelectionRepositoryOrder(t *testing.T) {
	t.Parallel()
	for _, emptyFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(emptyFirst), func(t *testing.T) {
			a := newCLIFixture(t)
			_ = a.create(t, "agent") // Deliberately give the first repo a higher issue number.
			high := a.create(t, "agent")
			if !emptyFirst {
				a.must(t, "issue", "approve", strconv.Itoa(high))
			}
			b := newCLIFixture(t)
			low := b.create(t, "agent")
			b.must(t, "issue", "approve", strconv.Itoa(low))
			b.must(t, "repo", "add", "z/second", b.root)
			b.must(t, "repo", "add", "a/first", a.root)
			raw := b.must(t, "issue", "claim", "--next", "--all", "--agent", "mock", "--json")
			var result struct{ Selection pulledWorkUnit }
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				t.Fatal(err)
			}
			wantRepo, wantNumber := "a/first", high
			if emptyFirst {
				wantRepo, wantNumber = "z/second", low
			}
			if result.Selection.Repo != wantRepo || result.Selection.Issue.Number != wantNumber {
				t.Fatalf("unexpected selection: %+v", result.Selection)
			}
		})
	}
}

func TestNextSelectionFallbackEligibility(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	ctx := context.Background()
	epic, err := f.tracker.CreateTypedIssue(ctx, "Epic", "", []string{"swarm:ready"}, "Epic")
	if err != nil {
		t.Fatal(err)
	}
	blocked := f.create(t, "agent")
	capability := f.create(t, "agent")
	active := f.create(t, "agent")
	human := f.create(t, "human")
	prOpen := f.create(t, "agent")
	winner := f.create(t, "agent")
	for _, n := range []int{blocked, capability} {
		issue, err := f.tracker.GetIssue(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
		if err != nil {
			t.Fatal(err)
		}
		if n == blocked {
			wu.DependsOn = []int{999}
		} else {
			wu.RequiredCapabilities = []string{"unavailable-test-capability"}
		}
		raw, err := yaml.Marshal(wu)
		if err != nil {
			t.Fatal(err)
		}
		// Replace the body with a canonical fenced contract before approval.
		if _, err := f.tracker.UpdateIssueBody(ctx, n, "```yaml\n"+string(raw)+"```\n"); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []int{blocked, capability, active, human, prOpen, winner} {
		f.must(t, "issue", "approve", strconv.Itoa(n))
	}
	f.must(t, "issue", "claim", strconv.Itoa(active), "--agent", "mock")
	f.must(t, "issue", "work", strconv.Itoa(human), "--agent", "mock", "--headless")
	if err := f.tracker.AddLabels(ctx, prOpen, []string{"swarm:pr-open"}); err != nil {
		t.Fatal(err)
	}
	parents := map[int]int{}
	for _, n := range []int{blocked, capability, active, human, prOpen, winner} {
		parents[n] = epic.Number
	}
	tr := &orderingTracker{Tracker: f.tracker, parents: parents, listErr: errors.New("offline"), calls: map[int]int{}}
	cmd := f.command(&RootOptions{RepoRoot: f.root, trackerOverride: tr})
	var out, diagnostics bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs([]string{"--root", f.root, "issue", "claim", "--next", "--agent", "mock", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v %s %s", err, &out, &diagnostics)
	}
	var result struct{ Selection pulledWorkUnit }
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Selection.Issue.Number != winner || !strings.Contains(diagnostics.String(), "warning:") {
		t.Fatalf("selected %d: %s", result.Selection.Issue.Number, &diagnostics)
	}
}

// createTouching creates an approved task whose objective names touchpoint.
func (f *cliFixture) createTouching(t *testing.T, touchpoint string) int {
	t.Helper()
	f.must(t, "issue", "create", "--title", "Approved task", "--objective", "Change "+touchpoint, "--constraint", "Keep policy unchanged", "--acceptance", "Produce a checked checkpoint", "--gate", "agent")
	issues, err := f.tracker.ListIssues(context.Background(), tracker.IssueFilter{State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	number := 0
	for _, issue := range issues {
		number = max(number, issue.Number)
	}
	f.must(t, "issue", "approve", strconv.Itoa(number))
	return number
}

// claimInFlight claims issue and drops its ready label, so in-flight discovery
// cannot rely on the ready filter.
func (f *cliFixture) claimInFlight(t *testing.T, issue int) {
	t.Helper()
	f.must(t, "issue", "claim", strconv.Itoa(issue), "--agent", "mock")
	if err := f.tracker.RemoveLabel(context.Background(), issue, "swarm:ready"); err != nil {
		t.Fatal(err)
	}
}

func TestOverlapSelectionPrefersNonOverlapping(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"claim", "work", "node"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newCLIFixture(t)
			active := f.createTouching(t, "internal/lease/a.go")
			f.claimInFlight(t, active)
			overlapping := f.createTouching(t, "internal/lease/b.go")
			free := f.createTouching(t, "docs/guide.md")
			cmd := f.command(nil)
			var output, diagnostics bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&diagnostics)
			args := []string{"issue", "claim", "--next", "--agent", "mock", "--json"}
			if mode == "work" {
				args = []string{"issue", "work", "--next", "--agent", "mock", "--phase", "plan", "--headless", "--json", "--progress=false"}
			}
			if mode == "node" {
				args = []string{"node", "run", "--max-jobs", "1", "--agent", "mock", "--json"}
			}
			cmd.SetArgs(append([]string{"--root", f.root}, args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%v %s %s", err, &output, &diagnostics)
			}
			if f.state(t, free).Lease.ClaimID == "" || f.state(t, overlapping).Lease.ClaimID != "" {
				t.Fatalf("expected #%d over #%d: %s", free, overlapping, &diagnostics)
			}
			why := "Issue #" + strconv.Itoa(free) + " selected: overlap score 0"
			if !strings.Contains(diagnostics.String(), why) || !strings.Contains(diagnostics.String(), "preferred over #"+strconv.Itoa(overlapping)) {
				t.Fatalf("missing explanation: %s", &diagnostics)
			}
			if strings.Contains(output.String(), "selected:") {
				t.Fatal("explanation on stdout")
			}
			var doc struct {
				Overlap    *overlapSelection `json:"selection"`
				Selections []struct {
					IssueNumber int               `json:"issue_number"`
					Overlap     *overlapSelection `json:"overlap"`
				}
			}
			decoder := json.NewDecoder(&output)
			if mode == "claim" {
				var claim struct {
					Selection struct{ Overlap *overlapSelection }
				}
				if err := decoder.Decode(&claim); err != nil {
					t.Fatal(err)
				}
				doc.Overlap = claim.Selection.Overlap
			} else if err := decoder.Decode(&doc); err != nil {
				t.Fatal(err)
			}
			if decoder.More() {
				t.Fatal("extra stdout after JSON")
			}
			got := doc.Overlap
			if mode == "node" {
				if len(doc.Selections) != 1 || doc.Selections[0].IssueNumber != free {
					t.Fatalf("node selections: %+v", doc.Selections)
				}
				got = doc.Selections[0].Overlap
			}
			if got == nil || got.Score != 0 || !got.Complete || got.InFlight != 1 || !reflect.DeepEqual(got.Deferred, []int{overlapping}) || !strings.Contains(got.Reason, "preferred over") {
				t.Fatalf("selection metadata: %+v", got)
			}
		})
	}
}

func TestOverlapSelectionIsAdvisory(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	active := f.createTouching(t, "internal/lease/a.go")
	f.claimInFlight(t, active)
	first := f.createTouching(t, "internal/lease/b.go internal/lease/c.go")
	second := f.createTouching(t, "internal/lease")
	cmd := f.command(nil)
	var output, diagnostics bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs([]string{"--root", f.root, "issue", "claim", "--next", "--agent", "mock", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("overlap refused a claim: %v %s", err, &diagnostics)
	}
	var result struct{ Selection pulledWorkUnit }
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	got := result.Selection.Overlap
	// Equal scores keep ready order, and two matched paths still count one issue.
	if result.Selection.Issue.Number != first || got == nil || got.Score != 1 || !reflect.DeepEqual(got.Issues, []int{active}) ||
		!reflect.DeepEqual(got.Paths, []string{"internal/lease/b.go", "internal/lease/c.go"}) || len(got.Deferred) != 0 {
		t.Fatalf("selection %d: %+v", result.Selection.Issue.Number, got)
	}
	if !strings.Contains(diagnostics.String(), "overlap score 1") || !strings.Contains(diagnostics.String(), "ties keep ready order") {
		t.Fatalf("missing explanation: %s", &diagnostics)
	}
	// An explicit claim of an overlapping task is unaffected.
	f.must(t, "issue", "claim", strconv.Itoa(second), "--agent", "mock")
}

type listFailTracker struct {
	tracker.Tracker
	err error
}

func (l *listFailTracker) ListIssues(ctx context.Context, filter tracker.IssueFilter) ([]*tracker.Issue, error) {
	if len(filter.Labels) == 0 {
		return nil, l.err
	}
	return l.Tracker.ListIssues(ctx, filter)
}

func TestOverlapSelectionSnapshot(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	ctx := context.Background()
	active := f.createTouching(t, "internal/lease/a.go")
	f.claimInFlight(t, active)
	overlapping := f.createTouching(t, "internal/lease/b.go")
	free := f.createTouching(t, "docs/guide.md")
	unknown := f.createTouching(t, "nothing in particular")
	candidates := func() []*tracker.Issue {
		var out []*tracker.Issue
		for _, n := range []int{overlapping, free, unknown} {
			issue, err := f.tracker.GetIssue(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, issue)
		}
		return out
	}
	expires := f.state(t, active).Lease.ExpiresAt
	for _, tc := range []struct {
		name     string
		now      time.Time
		tr       tracker.Tracker
		want     []int
		complete bool
		inFlight int
		warning  string
	}{
		{"active", expires.Add(-time.Minute), f.tracker, []int{free, unknown, overlapping}, true, 1, ""},
		{"expires at now", expires, f.tracker, []int{overlapping, free, unknown}, true, 0, ""},
		{"expired", expires.Add(time.Minute), f.tracker, []int{overlapping, free, unknown}, true, 0, ""},
		{"list failure", expires.Add(-time.Minute), &listFailTracker{Tracker: f.tracker, err: errors.New("offline")}, []int{overlapping, free, unknown}, false, 0, "warning: cannot list open issues"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diagnostics bytes.Buffer
			got, overlaps, err := orderByOverlap(ctx, tc.tr, f.manager.GetCurrentState, f.root, candidates(), tc.now, &diagnostics)
			if err != nil || !reflect.DeepEqual(issueNumbers(got), tc.want) {
				t.Fatalf("got %v, %v", issueNumbers(got), err)
			}
			for _, n := range tc.want {
				if o := overlaps[n]; o == nil || o.Complete != tc.complete || o.InFlight != tc.inFlight || o.Reason == "" {
					t.Fatalf("#%d overlap: %+v", n, o)
				}
			}
			if tc.warning == "" && diagnostics.Len() != 0 || !strings.Contains(diagnostics.String(), tc.warning) {
				t.Fatalf("diagnostics: %s", &diagnostics)
			}
		})
	}

	// Released claims no longer count as in-flight work.
	if err := f.tracker.AddLabels(ctx, active, []string{"swarm:ready"}); err != nil {
		t.Fatal(err)
	}
	f.must(t, "issue", "release", strconv.Itoa(active))
	var diagnostics bytes.Buffer
	got, overlaps, err := orderByOverlap(ctx, f.tracker, f.manager.GetCurrentState, f.root, candidates(), time.Now(), &diagnostics)
	if err != nil || !reflect.DeepEqual(issueNumbers(got), []int{overlapping, free, unknown}) || overlaps[overlapping].InFlight != 0 {
		t.Fatalf("released: %v %v %+v", issueNumbers(got), err, overlaps[overlapping])
	}

	// A claimed issue whose text no longer parses yields a partial signal, not a refusal.
	f.claimInFlight(t, free)
	if _, err := f.tracker.UpdateIssueBody(ctx, free, "not a work unit"); err != nil {
		t.Fatal(err)
	}
	diagnostics.Reset()
	got, overlaps, err = orderByOverlap(ctx, f.tracker, f.manager.GetCurrentState, f.root, candidates()[:1], time.Now(), &diagnostics)
	if err != nil || len(got) != 1 || overlaps[overlapping].Complete || !strings.Contains(overlaps[overlapping].Reason, "partial") || !strings.Contains(diagnostics.String(), "warning: overlap scheduling signal is partial") {
		t.Fatalf("partial: %v %+v %s", err, overlaps[overlapping], &diagnostics)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := orderByOverlap(canceled, f.tracker, f.manager.GetCurrentState, f.root, candidates(), time.Now(), &diagnostics); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestSelectorRefusesCommandOnlyTestBeforeResolution(t *testing.T) {
	// nil dependencies make any target resolution, tracker, lease, or agent
	// activity fail immediately; phase validation must be the first operation.
	_, err := pullNextReadyIssue(t.Context(), nil, nil, true, workflow.WorkOptions{Phase: "test"}, 0)
	if err == nil || !strings.Contains(err.Error(), "invalid workflow phase") {
		t.Fatalf("selector: %v", err)
	}
}

func TestSelectorRefusesCommandOnlyTestWithLiveDependencies(t *testing.T) {
	f := newCLIFixture(t)
	f.must(t, "issue", "approve", strconv.Itoa(f.create(t, "none")))
	tr := &untouchedSelectionTracker{}
	runs := 0
	f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &countingRunner{runs: &runs}, nil
	}
	opts := f.options(&RootOptions{RepoRoot: f.root, Repo: "owner/repo", trackerOverride: tr})
	before := rootSnapshot(t, f.root)
	for _, all := range []bool{false, true} {
		if _, err := pullNextReadyIssue(t.Context(), f.command(opts), opts, all, workflow.WorkOptions{Phase: "test", AgentName: "mock"}, 0); err == nil || !strings.Contains(err.Error(), "invalid workflow phase") {
			t.Fatalf("all=%t: %v", all, err)
		}
	}
	if tr.calls != 0 || runs != 0 || !reflect.DeepEqual(before, rootSnapshot(t, f.root)) {
		t.Fatalf("selector activity: tracker=%d agents=%d", tr.calls, runs)
	}
}

func TestDecompositionNextSelectionEntryPoints(t *testing.T) {
	for _, mode := range []string{"claim", "work", "node"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			parent := f.create(t, "agent")
			f.must(t, "issue", "approve", strconv.Itoa(parent))
			f.must(t, "issue", "decompose", strconv.Itoa(parent))
			_ = f.tracker.RemoveLabel(t.Context(), parent, "swarm:decomposed")
			_ = f.tracker.AddLabels(t.Context(), parent, []string{"swarm:ready"})
			for _, verb := range []string{"claim", "work"} {
				args := []string{"issue", verb, strconv.Itoa(parent), "--agent", "mock"}
				if verb == "work" {
					args = append(args, "--headless", "--progress=false")
				}
				out, err := f.run(args...)
				if err == nil || !strings.Contains(err.Error()+out, "child issues") {
					t.Fatalf("direct %s: %v %s", verb, err, out)
				}
			}
			winner := f.create(t, "agent")
			f.must(t, "issue", "approve", strconv.Itoa(winner))
			_ = f.tracker.AddLabels(t.Context(), winner, []string{"swarm:decomposed"})
			args := []string{"issue", "claim", "--next", "--agent", "mock"}
			if mode == "work" {
				args = []string{"issue", "work", "--next", "--agent", "mock", "--headless", "--progress=false"}
			}
			if mode == "node" {
				args = []string{"node", "run", "--max-jobs", "1", "--agent", "mock"}
			}
			if out, err := f.run(args...); err != nil {
				t.Fatalf("%s: %v %s", mode, err, out)
			}
			if f.state(t, winner).Lease.ClaimID == "" {
				t.Fatal("ordinary issue with forged decomposed label was skipped")
			}
			if f.state(t, parent).Lease.Status != "decomposed" {
				t.Fatal("retired parent was executed")
			}
		})
	}
}

func TestNextSelectionHumanPlanAdvice(t *testing.T) {
	for _, mode := range []string{"work", "claim", "node"} {
		t.Run(mode, func(t *testing.T) {
			_, n, _, _, run := recoveryCommandFixture(t, "local", "human", 1)
			number := strconv.Itoa(n)
			for _, args := range [][]string{{"issue", "work", number, "--phase", "plan", "--agent", "mock", "--progress=false"}, {"issue", "release", number}} {
				if out, err := run(args...); err != nil {
					t.Fatal(err, out)
				}
			}
			args := []string{"issue", mode, "--next", "--agent", "mock"}
			want := ExitNeedsApproval
			if mode == "claim" {
				want = ExitFailure
			}
			if mode == "node" {
				args = []string{"node", "run", "--max-jobs", "1", "--agent", "mock"}
				want = ExitOK
			}
			out, err := run(args...)
			combined := out
			if err != nil {
				combined += fmt.Sprintf("Error: %v\n", err)
			}
			if ExitCode(err) != want || strings.Count(combined, "Next step:") != 1 || strings.Count(combined, "issue review-plan "+number+" --approve") != 1 || strings.Contains(combined, "issue review-plan 0") || !strings.Contains(combined, "independent maintainer") {
				t.Fatalf("selection: %v\n%s", err, combined)
			}
		})
	}
}

func TestParentLabelOrdering(t *testing.T) {
	f := newCLIFixture(t)
	ctx := context.Background()
	parent, err := f.tracker.CreateIssue(ctx, "Parent", "", []string{tracker.ParentLabel})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := f.tracker.CreateIssue(ctx, "a", "", nil)
	b, _ := f.tracker.CreateIssue(ctx, "b", "", nil)
	if err := f.tracker.LinkSubIssue(ctx, parent.Number, b.Number); err != nil {
		t.Fatal(err)
	}
	if err := f.tracker.LinkSubIssue(ctx, parent.Number, a.Number); err != nil {
		t.Fatal(err)
	}
	got, err := orderReadyIssues(ctx, f.tracker, []*tracker.Issue{a, b}, &bytes.Buffer{})
	if err != nil || !reflect.DeepEqual(issueNumbers(got), []int{b.Number, a.Number}) {
		t.Fatalf("%v %v", issueNumbers(got), err)
	}
}

func TestNextSelectionEntryPointsParentLabel(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"claim", "work", "node", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			ctx := context.Background()
			epic, err := f.tracker.CreateIssue(ctx, "Parent", "", []string{tracker.ParentLabel})
			if err != nil {
				t.Fatal(err)
			}
			first := f.create(t, "agent")
			second := f.create(t, "agent")
			for _, n := range []int{first, second} {
				f.must(t, "issue", "approve", strconv.Itoa(n))
			}
			for _, n := range []int{second, first} {
				if err := f.tracker.LinkSubIssue(ctx, epic.Number, n); err != nil {
					t.Fatal(err)
				}
			}
			var tr tracker.Tracker = f.tracker
			want := second
			if mode == "fallback" {
				tr = &orderingTracker{Tracker: f.tracker, parents: map[int]int{first: epic.Number, second: epic.Number}, listErr: errors.New("offline"), calls: map[int]int{}}
				want = first
			}
			cmd := f.command(&RootOptions{RepoRoot: f.root, trackerOverride: tr})
			var output, diagnostics bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&diagnostics)
			args := []string{"issue", "claim", "--next", "--agent", "mock", "--json"}
			if mode == "work" {
				args = []string{"issue", "work", "--next", "--agent", "mock", "--headless", "--progress=false"}
			}
			if mode == "node" {
				args = []string{"node", "run", "--max-jobs", "1", "--agent", "mock"}
			}
			cmd.SetArgs(append([]string{"--root", f.root}, args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%v %s %s", err, &output, &diagnostics)
			}
			if f.state(t, want).Lease.ClaimID == "" {
				t.Fatal("expected sibling was not selected")
			}
			other := first
			if want == first {
				other = second
			}
			if f.state(t, other).Lease.ClaimID != "" {
				t.Fatal("selected extra candidate")
			}
			if mode == "fallback" {
				if !strings.Contains(diagnostics.String(), "warning:") || !json.Valid(output.Bytes()) {
					t.Fatalf("warning/output: %s %s", &diagnostics, &output)
				}
			}
		})
	}
}

// Repository traversal retains the registry's existing order, regardless of
// relative issue numbers in different repositories.
func TestNextSelectionFallbackEligibilityParentLabel(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	ctx := context.Background()
	epic, err := f.tracker.CreateIssue(ctx, "Parent", "", []string{"swarm:ready", tracker.ParentLabel})
	if err != nil {
		t.Fatal(err)
	}
	blocked := f.create(t, "agent")
	capability := f.create(t, "agent")
	active := f.create(t, "agent")
	human := f.create(t, "human")
	prOpen := f.create(t, "agent")
	winner := f.create(t, "agent")
	for _, n := range []int{blocked, capability} {
		issue, err := f.tracker.GetIssue(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		wu, err := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
		if err != nil {
			t.Fatal(err)
		}
		if n == blocked {
			wu.DependsOn = []int{999}
		} else {
			wu.RequiredCapabilities = []string{"unavailable-test-capability"}
		}
		raw, err := yaml.Marshal(wu)
		if err != nil {
			t.Fatal(err)
		}
		// Replace the body with a canonical fenced contract before approval.
		if _, err := f.tracker.UpdateIssueBody(ctx, n, "```yaml\n"+string(raw)+"```\n"); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []int{blocked, capability, active, human, prOpen, winner} {
		f.must(t, "issue", "approve", strconv.Itoa(n))
	}
	f.must(t, "issue", "claim", strconv.Itoa(active), "--agent", "mock")
	f.must(t, "issue", "work", strconv.Itoa(human), "--agent", "mock", "--headless")
	if err := f.tracker.AddLabels(ctx, prOpen, []string{"swarm:pr-open"}); err != nil {
		t.Fatal(err)
	}
	parents := map[int]int{}
	for _, n := range []int{blocked, capability, active, human, prOpen, winner} {
		parents[n] = epic.Number
	}
	tr := &orderingTracker{Tracker: f.tracker, parents: parents, listErr: errors.New("offline"), calls: map[int]int{}}
	cmd := f.command(&RootOptions{RepoRoot: f.root, trackerOverride: tr})
	var out, diagnostics bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs([]string{"--root", f.root, "issue", "claim", "--next", "--agent", "mock", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v %s %s", err, &out, &diagnostics)
	}
	var result struct{ Selection pulledWorkUnit }
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Selection.Issue.Number != winner || !strings.Contains(diagnostics.String(), "warning:") {
		t.Fatalf("selected %d: %s", result.Selection.Issue.Number, &diagnostics)
	}
}

// createTouching creates an approved task whose objective names touchpoint.

type selectionCountingStore struct {
	coordination.Store
	provider         func(context.Context, bool, int, func(int64) error, ...coordination.InventoryDecoder) (map[int][]byte, error)
	readNumbers      []int
	data             map[int][]byte
	snapshots, loads int
	failure          error
	after            func()
}

func (s *selectionCountingStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	s.loads++
	s.readNumbers = append(s.readNumbers, n)
	return s.Store.Load(ctx, n)
}
func (s *selectionCountingStore) ReadInventorySnapshot(ctx context.Context, _ bool, _ int, reserve func(int64) error, d ...coordination.InventoryDecoder) (map[int][]byte, error) {
	s.snapshots++
	if s.provider != nil {
		return s.provider(ctx, true, 10000, reserve, d...)
	}
	if s.failure != nil {
		return nil, s.failure
	}
	out := map[int][]byte{}
	for n, b := range s.data {
		v, e := d[0](ctx, bytes.NewReader(b), reserve)
		if e != nil {
			return nil, e
		}
		out[n] = v
	}
	if s.after != nil {
		s.after()
	}
	return out, nil
}
func selectionFixtureStore(t *testing.T, f *cliFixture) *selectionCountingStore {
	t.Helper()
	s := &selectionCountingStore{Store: f.manager.Store(), data: map[int][]byte{}}
	issues, e := f.tracker.ListIssues(t.Context(), tracker.IssueFilter{State: "open"})
	if e != nil {
		t.Fatal(e)
	}
	for _, issue := range issues {
		b, _, e := s.Store.Load(t.Context(), issue.Number)
		if errors.Is(e, coordination.ErrNotFound) {
			continue
		}
		if e != nil {
			t.Fatal(e)
		}
		s.data[issue.Number] = b
	}
	return s
}
func selectFixture(t *testing.T, f *cliFixture, store coordination.Store, options workflow.WorkOptions) (*pulledWorkUnit, string, error) {
	return selectFixtureContext(t.Context(), t, f, store, options)
}
func selectFixtureContext(ctx context.Context, t *testing.T, f *cliFixture, store coordination.Store, options workflow.WorkOptions) (*pulledWorkUnit, string, error) {
	t.Helper()
	cfg, e := harness.LoadConfig(f.root)
	if e != nil {
		t.Fatal(e)
	}
	mgr := lease.NewManager(f.tracker, cfg.Security)
	mgr.SetStore(f.root, store)
	opts := f.options(&RootOptions{RepoRoot: f.root})
	cmd := f.command(opts)
	var diagnostics bytes.Buffer
	cmd.SetErr(&diagnostics)
	cmd.SetOut(io.Discard)
	result, e := pullNextReadyIssueResolved(ctx, cmd, opts, false, options, time.Hour, func(*cobra.Command, registry.RepoEntry) (tracker.Tracker, *lease.Manager, *contract.SwarmConfig, error) {
		return f.tracker, mgr, cfg, nil
	})
	return result, diagnostics.String(), e
}
func TestSelectionBatchCount(t *testing.T) {
	for _, count := range []int{1, 10} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			f := newCLIFixture(t)
			for i := 0; i < count; i++ {
				n := f.createTouching(t, "docs/change.md")
				if i < count-1 {
					switch i {
					case 0, 1:
						editSelectionState(t, f, n, func(st *lease.IssueState) { st.Lease.Status = "completed" })
					case 2, 3:
						editSelectionState(t, f, n, func(st *lease.IssueState) { st.TaskDigest = "changed" })
					default:
						f.claimInFlight(t, n)
					}
				}
			}
			s := selectionFixtureStore(t, f)
			got, diag, e := selectFixture(t, f, s, workflow.WorkOptions{Phase: "plan", AgentName: "mock"})
			if e != nil {
				t.Fatalf("%v %s", e, diag)
			}
			if got == nil || s.snapshots != 1 {
				t.Fatalf("got=%v snapshots=%d", got, s.snapshots)
			}
			// Only the winning issue is read authoritatively by preflight/acquisition.
			t.Logf("authoritative loads: %v", s.readNumbers)
			if !reflect.DeepEqual(s.readNumbers, []int{got.Issue.Number, got.Issue.Number}) {
				t.Fatalf("selection performed individual reads: %d", s.loads)
			}
		})
	}
}
func TestSelectionBatchEquivalence(t *testing.T) {
	var wantNum int
	var wantOverlap *overlapSelection
	var wantDiag string
	for _, batch := range []bool{false, true} {
		f := newCLIFixture(t)
		active := f.createTouching(t, "internal/lease/a.go")
		f.claimInFlight(t, active)
		f.createTouching(t, "internal/lease/b.go")
		f.createTouching(t, "docs/change.md")
		s := selectionFixtureStore(t, f)
		var store coordination.Store = s.Store
		if batch {
			store = s
		}
		got, diag, e := selectFixture(t, f, store, workflow.WorkOptions{Phase: "plan", AgentName: "mock"})
		if e != nil {
			t.Fatalf("%v %s", e, diag)
		}
		if !batch {
			wantNum, wantOverlap, wantDiag = got.Issue.Number, got.Overlap, diag
		} else if got.Issue.Number != wantNum || !reflect.DeepEqual(got.Overlap, wantOverlap) || diag != wantDiag {
			t.Fatalf("batch differs: %d %+v %q vs %d %+v %q", got.Issue.Number, got.Overlap, diag, wantNum, wantOverlap, wantDiag)
		}
	}
}
func TestSelectionBatchSnapshotFallback(t *testing.T) {
	for _, failure := range []error{errors.New("inventory output limit exceeded"), errors.New("unsupported state snapshot"), errors.New("budget exceeded"), errors.New("fetch failed"), errors.New("closed issue object unreadable"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			f := newCLIFixture(t)
			n := f.createTouching(t, "docs/change.md")
			s := selectionFixtureStore(t, f)
			s.failure = failure
			got, diag, e := selectFixture(t, f, s, workflow.WorkOptions{Phase: "plan", AgentName: "mock"})
			if e != nil || got.Issue.Number != n || s.snapshots != 1 || s.loads == 0 || strings.Count(diag, "warning: batched selection state unavailable") != 1 {
				t.Fatalf("%v %s snapshots=%d loads=%d", e, diag, s.snapshots, s.loads)
			}
		})
	}
}
func TestSelectionBatchClaimRace(t *testing.T) {
	f := newCLIFixture(t)
	first := f.createTouching(t, "docs/first.md")
	second := f.createTouching(t, "docs/second.md")
	s := selectionFixtureStore(t, f)
	var competitor *lease.IssueState
	cfg, err := harness.LoadConfig(f.root)
	if err != nil {
		t.Fatal(err)
	}
	other := lease.NewManager(f.tracker.WithUser("competitor", ""), cfg.Security)
	other.SetStore(f.root, s.Store)
	var raceErr error
	reporter := workflow.NewProgressReporter(t.Context(), func(e workflow.ProgressEvent) {
		if e.Stage == "claim" && e.IssueNumber == first && competitor == nil {
			competitor, raceErr = other.Acquire(t.Context(), first, "competitor", "mock", "plan", time.Hour, 1)
		}
	})
	defer reporter.Close()
	got, diag, err := selectFixture(t, f, s, workflow.WorkOptions{Phase: "plan", AgentName: "mock", SetupProgress: reporter})
	if err != nil || got.Issue.Number != second || competitor == nil || raceErr != nil {
		t.Fatalf("%v %s got=%v competitor=%v race=%v", err, diag, got, competitor, raceErr)
	}
	if _, e := lease.LoadClaimToken(f.root, first); !os.IsNotExist(e) {
		t.Fatalf("loser token: %v", e)
	}
	if !reflect.DeepEqual(s.readNumbers, []int{first, first, second, second}) {
		t.Fatalf("fresh reads: %v", s.readNumbers)
	}
	current := f.state(t, first)
	if current.Lease.ClaimID != competitor.Lease.ClaimID {
		t.Fatal("competitor overwritten")
	}
	if _, err := os.Stat(filepath.Join(f.root, ".swarm", "worktrees", fmt.Sprintf("issue-%d", first))); !os.IsNotExist(err) {
		t.Fatalf("loser workspace: %v", err)
	}
}

// Script only the inventory commands; fixture preflight and real acquisition use Git.
func selectionInventoryScript(t *testing.T, s *selectionCountingStore, layout coordination.RefLayout, mode string) string {
	t.Helper()
	dir := t.TempDir()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	var rows strings.Builder
	for n, b := range s.data {
		sha := fmt.Sprintf("%040x", n)
		fmt.Fprintf(&rows, "%s\t%s\n", sha, layout.StateRef(n))
		if err := os.WriteFile(filepath.Join(dir, sha), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, data string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(dir, name), []byte(data), 0700); e != nil {
			t.Fatal(e)
		}
	}
	write("rows", rows.String())
	var over strings.Builder
	for n := 1; n <= 4000; n++ {
		fmt.Fprintf(&over, "%040x\t%s\n", n, layout.StateRef(n))
	}
	write("overflow", over.String())
	log := filepath.Join(dir, "log")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
ls-remote)
 if [ "$2" = '--refs' ]; then
  echo list >> '%[1]s/log'
  n=0; [ ! -f '%[1]s/count' ] || n=$(cat '%[1]s/count'); n=$((n+1)); echo "$n" > '%[1]s/count'
  case '%[3]s' in
   overflow) cat '%[1]s/overflow'; exit 0;;
   unsupported) printf '%%s\t%%s\n' '%[4]s' '%[5]s'; exit 0;;
  esac
  cat '%[1]s/rows'
  if { [ '%[3]s' = persistent ] && [ $((n%%2)) = 1 ]; } || { [ '%[3]s' = stabilize ] && [ "$n" = 1 ]; }; then
   printf '%%s\t%%s\n' '%[4]s' '%[6]s'
  fi
  exit 0
 fi;;
fetch)
 if [ "$2 $3" = '--no-tags --no-write-fetch-head' ]; then echo fetch >> '%[1]s/log';exit 0;fi;;
show)
 sha=${2%%:state.json}
 if [ -f '%[1]s/'"$sha" ]; then echo show >> '%[1]s/log';cat '%[1]s/'"$sha";exit 0;fi;;
esac
exec '%[2]s' "$@"
`, dir, realGit, mode, strings.Repeat("a", 40), layout.Counterpart().StateRef(999), layout.StateRef(999))
	write("git", script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}
func TestSelectionBatchSnapshotRefreshRetry(t *testing.T) {
	for _, layout := range []coordination.RefLayout{coordination.BranchLayout, coordination.HiddenLayout} {
		for _, mode := range []string{"stabilize", "persistent", "overflow", "unsupported"} {
			t.Run(fmt.Sprintf("%v/%s", layout, mode), func(t *testing.T) {
				f := newCLIFixture(t)
				want := f.createTouching(t, "docs/change.md")
				s := selectionFixtureStore(t, f)
				log := selectionInventoryScript(t, s, layout, mode)
				gitStore := coordination.NewGitStoreWithLayout(f.root, "", layout)
				s.provider = gitStore.ReadInventorySnapshot
				got, diag, e := selectFixture(t, f, s, workflow.WorkOptions{Phase: "plan", AgentName: "mock"})
				if e != nil || got == nil || got.Issue.Number != want {
					t.Fatalf("%v %s got=%v", e, diag, got)
				}
				calls, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				attempts, lists, fetches, warnings := 1, 1, 0, 1
				switch mode {
				case "stabilize":
					attempts, lists, fetches, warnings = 2, 4, 2, 0
				case "persistent":
					attempts, lists, fetches = 3, 6, 3
				}
				if s.snapshots != attempts || strings.Count(string(calls), "list\n") != lists || strings.Count(string(calls), "fetch\n") != fetches || strings.Count(diag, "warning: batched selection state unavailable") != warnings {
					t.Fatalf("snapshots=%d calls=%s diag=%s", s.snapshots, calls, diag)
				}
			})
		}
	}
}

func editSelectionState(t *testing.T, f *cliFixture, n int, edit func(*lease.IssueState)) {
	t.Helper()
	b, rev, e := f.manager.Store().Load(t.Context(), n)
	if e != nil {
		t.Fatal(e)
	}
	var st lease.IssueState
	if e = json.Unmarshal(b, &st); e != nil {
		t.Fatal(e)
	}
	edit(&st)
	b, e = json.Marshal(st)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.manager.Store().CompareAndSwap(t.Context(), n, rev, b); e != nil {
		t.Fatal(e)
	}
}

// Paired fixtures exercise filtering and blocked diagnostics before any eligible candidate.
func TestSelectionBatchEquivalenceEligibility(t *testing.T) {
	for _, noReady := range []bool{false, true} {
		var wantNum int
		var wantOverlap *overlapSelection
		var wantDiag, wantErr string
		for _, mode := range []string{"ordinary", "batch", "limit", "unsupported listing", "budget", "fetch", "unrelated closed object", "internally canceled"} {
			f := newCLIFixture(t)
			active := f.createTouching(t, "internal/lease/a.go")
			f.claimInFlight(t, active)
			for _, kind := range []string{"completed", "decomposed", "unapproved", "dependency", "capability", "human"} {
				n := f.createTouching(t, "docs/blocked.md")
				switch kind {
				case "completed":
					editSelectionState(t, f, n, func(st *lease.IssueState) { st.Lease.Status = "completed" })
				case "decomposed":
					editSelectionState(t, f, n, func(st *lease.IssueState) { st.Decomposition = &lease.Decomposition{ID: "test"} })
				case "unapproved":
					editSelectionState(t, f, n, func(st *lease.IssueState) { st.TaskDigest = "changed" })
				case "human":
					editSelectionState(t, f, n, func(st *lease.IssueState) {
						st.EffectiveGate = contract.GateHuman
						st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactDesign, Body: "plan"})
					})
				default:
					issue, e := f.tracker.GetIssue(t.Context(), n)
					if e != nil {
						t.Fatal(e)
					}
					wu, e := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body)
					if e != nil {
						t.Fatal(e)
					}
					if kind == "dependency" {
						wu.DependsOn = []int{999}
					} else {
						wu.RequiredCapabilities = []string{"unavailable-test-capability"}
					}
					raw, e := yaml.Marshal(wu)
					if e != nil {
						t.Fatal(e)
					}
					if _, e = f.tracker.UpdateIssueBody(t.Context(), n, "```yaml\n"+string(raw)+"```\n"); e != nil {
						t.Fatal(e)
					}
					f.must(t, "issue", "approve", strconv.Itoa(n))
				}
			}
			// Missing state still appears as open ready work.
			issue, e := f.tracker.GetIssue(t.Context(), active)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = f.tracker.CreateIssue(t.Context(), "missing", issue.Body, []string{"swarm:ready"}); e != nil {
				t.Fatal(e)
			}
			if !noReady {
				n := f.createTouching(t, "docs/winner.md")
				editSelectionState(t, f, n, func(st *lease.IssueState) {
					st.Lease.Status = "active"
					st.Lease.ExpiresAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
				})
				f.createTouching(t, "docs/tied.md")
			}
			s := selectionFixtureStore(t, f)
			var store coordination.Store = s.Store
			if mode != "ordinary" {
				store = s
			}
			if mode != "ordinary" && mode != "batch" {
				s.failure = errors.New(mode)
				if mode == "internally canceled" {
					s.failure = context.Canceled
				}
			}
			got, diag, e := selectFixture(t, f, store, workflow.WorkOptions{Phase: "all", AgentName: "mock"})
			diag = strings.ReplaceAll(diag, "warning: batched selection state unavailable; using per-issue reads\n", "")
			diag = strings.ReplaceAll(diag, f.root, "<root>")
			errText := strings.ReplaceAll(fmt.Sprint(e), f.root, "<root>")
			n := 0
			var overlap *overlapSelection
			if got != nil {
				n, overlap = got.Issue.Number, got.Overlap
			}
			if mode == "ordinary" {
				wantNum, wantOverlap, wantDiag, wantErr = n, overlap, diag, errText
			} else if n != wantNum || !reflect.DeepEqual(overlap, wantOverlap) || diag != wantDiag || errText != wantErr {
				t.Fatalf("%s differs: %d %+v %q %q vs %d %+v %q %q", mode, n, overlap, diag, errText, wantNum, wantOverlap, wantDiag, wantErr)
			}
			if noReady && !errors.Is(e, errNoReadyWork) {
				t.Fatalf("expected no ready: %v", e)
			}
			if !noReady && e != nil {
				t.Fatalf("%v %s", e, diag)
			}
		}
	}
}
func TestSelectionBatchCancellation(t *testing.T) {
	f := newCLIFixture(t)
	f.createTouching(t, "docs/test.md")
	s := selectionFixtureStore(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	s.provider = func(context.Context, bool, int, func(int64) error, ...coordination.InventoryDecoder) (map[int][]byte, error) {
		cancel()
		return nil, errors.New("transport failed")
	}
	_, diag, e := selectFixtureContext(ctx, t, f, s, workflow.WorkOptions{Phase: "plan", AgentName: "mock"})
	if !errors.Is(e, context.Canceled) || s.loads != 0 || strings.Contains(diag, "batched selection state unavailable") {
		t.Fatalf("%v %q loads=%d", e, diag, s.loads)
	}
}

type selectionFailLoadStore struct {
	coordination.Store
	number int
	err    error
}

func (s *selectionFailLoadStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	if n == s.number {
		return nil, "", s.err
	}
	return s.Store.Load(ctx, n)
}
func TestSelectionBatchSnapshotFallbackRequestedLoadError(t *testing.T) {
	f := newCLIFixture(t)
	n := f.createTouching(t, "docs/change.md")
	s := selectionFixtureStore(t, f)
	sentinel := errors.New("requested issue load failed")
	s.Store = &selectionFailLoadStore{Store: s.Store, number: n, err: sentinel}
	s.failure = errors.New("snapshot fetch failed")
	got, diag, err := selectFixture(t, f, s, workflow.WorkOptions{Phase: "plan", AgentName: "mock"})
	if got != nil || !errors.Is(err, sentinel) || s.snapshots != 1 || strings.Count(diag, "batched selection state unavailable") != 1 || !strings.Contains(diag, "overlap scheduling signal is partial") {
		t.Fatalf("%v %s got=%v", err, diag, got)
	}
}
