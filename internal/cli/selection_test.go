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

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
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
