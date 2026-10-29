package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

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
			cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: tr})
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
			cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
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
	cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: tr})
	var out, diagnostics bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "issue", "claim", "--next", "--agent", "mock", "--json"})
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
			cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
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
	cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "issue", "claim", "--next", "--agent", "mock", "--json"})
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
			got, overlaps, err := orderByOverlap(ctx, tc.tr, f.manager, f.root, candidates(), tc.now, &diagnostics)
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
	got, overlaps, err := orderByOverlap(ctx, f.tracker, f.manager, f.root, candidates(), time.Now(), &diagnostics)
	if err != nil || !reflect.DeepEqual(issueNumbers(got), []int{overlapping, free, unknown}) || overlaps[overlapping].InFlight != 0 {
		t.Fatalf("released: %v %v %+v", issueNumbers(got), err, overlaps[overlapping])
	}

	// A claimed issue whose text no longer parses yields a partial signal, not a refusal.
	f.claimInFlight(t, free)
	if _, err := f.tracker.UpdateIssueBody(ctx, free, "not a work unit"); err != nil {
		t.Fatal(err)
	}
	diagnostics.Reset()
	got, overlaps, err = orderByOverlap(ctx, f.tracker, f.manager, f.root, candidates()[:1], time.Now(), &diagnostics)
	if err != nil || len(got) != 1 || overlaps[overlapping].Complete || !strings.Contains(overlaps[overlapping].Reason, "partial") || !strings.Contains(diagnostics.String(), "warning: overlap scheduling signal is partial") {
		t.Fatalf("partial: %v %+v %s", err, overlaps[overlapping], &diagnostics)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := orderByOverlap(canceled, f.tracker, f.manager, f.root, candidates(), time.Now(), &diagnostics); !errors.Is(err, context.Canceled) {
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
