package pr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
)

type workTracker struct {
	tracker.Tracker
	observations []tracker.PullRequest
	calls        int
	err          error
}

func (t *workTracker) BackendName() string { return "github" }
func (t *workTracker) GetPullRequest(ctx context.Context, n int) (*tracker.PullRequest, error) {
	t.calls++
	if t.err != nil {
		return nil, t.err
	}
	i := t.calls - 1
	if i >= len(t.observations) {
		i = len(t.observations) - 1
	}
	p := t.observations[i]
	return &p, nil
}
func boolValue(b bool) *bool { return &b }

func TestWorkPreflightClassification(t *testing.T) {
	for _, state := range []string{"blocked", "behind", "unstable", "dirty", "unknown", "clean"} {
		for _, value := range []*bool{nil, boolValue(false), boolValue(true)} {
			p := tracker.PullRequest{Mergeable: value, MergeableState: state}
			want := "clean"
			if value == nil || state == "unknown" {
				want = "unknown"
			}
			if value != nil && !*value || value == nil && state == "dirty" {
				want = "conflicting"
			}
			if got := mergeability(&p); got != want {
				t.Fatalf("%+v: %s != %s", p, got, want)
			}
		}
	}
}

func TestWorkPreflightPolling(t *testing.T) {
	initial := tracker.PullRequest{Number: 33, State: "open", HeadSHA: "head", Head: "topic", Base: "main", BaseSHA: "base"}
	for _, tc := range []struct {
		name    string
		change  func(*tracker.PullRequest)
		failure error
		calls   int
		blocked bool
		wantErr bool
	}{
		{name: "persistent unknown", calls: 2},
		{name: "clean", change: func(p *tracker.PullRequest) { p.Mergeable = boolValue(true) }, calls: 1},
		{name: "conflict", change: func(p *tracker.PullRequest) { p.Mergeable = boolValue(false) }, calls: 1, blocked: true},
		{name: "base advances", change: func(p *tracker.PullRequest) { p.BaseSHA = "next" }, calls: 1, blocked: true},
		{name: "head changes", change: func(p *tracker.PullRequest) { p.HeadSHA = "next" }, calls: 1, wantErr: true},
		{name: "retarget", change: func(p *tracker.PullRequest) { p.Base = "other" }, calls: 1, wantErr: true},
		{name: "closed", change: func(p *tracker.PullRequest) { p.State = "closed" }, calls: 1, wantErr: true},
		{name: "API failure", failure: errors.New("API unavailable"), calls: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := initial
			if tc.change != nil {
				tc.change(&next)
			}
			tr := &workTracker{observations: []tracker.PullRequest{next}, err: tc.failure}
			s := &Service{Tracker: tr, workWait: func(ctx context.Context) error {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("missing budget")
				}
				return nil
			}}
			r := &Result{PR: &initial, BaseSHA: "base", State: &lease.IssueState{}, Worktree: t.TempDir() + "/missing"}
			err := s.checkWork(context.Background(), r)
			if (err != nil) != tc.wantErr || tr.calls != tc.calls || (len(r.WorkPreflight.Blockers) > 0) != tc.blocked {
				t.Fatalf("result=%+v calls=%d err=%v", r.WorkPreflight, tr.calls, err)
			}
		})
	}
}

func TestWorkPreflightCancellationAndDeadline(t *testing.T) {
	for _, callerCanceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		s := &Service{Tracker: &workTracker{}, workWait: func(context.Context) error {
			if callerCanceled {
				cancel()
				return context.Canceled
			}
			return context.DeadlineExceeded
		}}
		r := &Result{PR: &tracker.PullRequest{BaseSHA: "base"}, BaseSHA: "base", State: &lease.IssueState{}, Worktree: t.TempDir()}
		err := s.checkWork(ctx, r)
		cancel()
		if callerCanceled && !errors.Is(err, context.Canceled) || !callerCanceled && (err != nil || len(r.WorkPreflight.Warnings) != 1) {
			t.Fatalf("%+v %v", r.WorkPreflight, err)
		}
	}
}

func TestPRWorkRefusesBeforeMutation(t *testing.T) {
	for _, fork := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			for _, moved := range []bool{false, true} {
				t.Run(fmt.Sprintf("fork=%t/dry=%t/moved=%t", fork, dry, moved), func(t *testing.T) {
					s, issue, number, _ := fixture(t)
					p, err := s.Tracker.GetPullRequest(context.Background(), number)
					if err != nil {
						t.Fatal(err)
					}
					p.Mergeable = boolValue(false)
					p.MergeableState = "dirty"
					if moved {
						p.BaseSHA = "new-base"
					}
					s.Tracker = &workTracker{Tracker: s.Tracker, observations: []tracker.PullRequest{*p}}
					// The check itself is identical after remote authorization in contributor mode.
					// Test fork guidance independently without requiring an actual GitHub remote.
					r, _, err := s.openForMutation(context.Background(), number, false)
					if err != nil {
						t.Fatal(err)
					}
					s.Fork = fork
					s.ForkRemote = "my-fork"
					before, _ := s.Manager.GetCurrentState(context.Background(), issue)
					snapshot, _ := json.Marshal(before)
					if fork {
						err = s.checkWork(context.Background(), r)
					} else {
						r, err = s.Work(context.Background(), Options{Number: number, Work: workflow.WorkOptions{InspectOnly: dry}})
					}
					if !fork && (err == nil) != dry {
						t.Fatalf("dry=%t err=%v", dry, err)
					}
					if r == nil || r.WorkPreflight.Status != "blocked" {
						t.Fatalf("%+v %v", r, err)
					}
					report, _ := json.Marshal(r.WorkPreflight)
					for _, part := range []string{"dirty", "rebase", "--continue", "validate", "submit"} {
						if !strings.Contains(string(report), part) {
							t.Fatalf("missing %s: %s", part, report)
						}
					}
					if moved && !strings.Contains(string(report), fmt.Sprintf("approve %d", issue)) {
						t.Fatal(string(report))
					}
					if fork && !strings.Contains(string(report), "--fork-remote") {
						t.Fatal(string(report))
					}
					after, _ := s.Manager.GetCurrentState(context.Background(), issue)
					actual, _ := json.Marshal(after)
					if string(snapshot) != string(actual) {
						t.Fatal("state changed")
					}
					if _, err := os.Stat(r.Worktree); !os.IsNotExist(err) {
						t.Fatalf("worktree created: %v", err)
					}
				})
			}
		}
	}
}

func TestWorkPreflightLocalRebaseEvidence(t *testing.T) {
	for _, fork := range []bool{false, true} {
		s, issue, number, base := fixture(t)
		r, remote, err := s.openForMutation(context.Background(), number, false)
		if err != nil {
			t.Fatal(err)
		}
		path, branch, err := s.preparePR(context.Background(), r, remote, "", false)
		if err != nil {
			t.Fatal(err)
		}
		git := func(dir string, args ...string) string {
			t.Helper()
			out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		if err := os.WriteFile(filepath.Join(path, "topic.txt"), []byte("topic\n"), 0644); err != nil {
			t.Fatal(err)
		}
		git(path, "add", "topic.txt")
		git(path, "commit", "-m", "topic")
		oldHead := git(path, "rev-parse", "HEAD")
		if err := os.WriteFile(filepath.Join(s.Root, "base.txt"), []byte("base advance\n"), 0644); err != nil {
			t.Fatal(err)
		}
		git(s.Root, "add", "base.txt")
		git(s.Root, "commit", "-m", "advance base")
		newBase := git(s.Root, "rev-parse", "HEAD")
		git(path, "rebase", newBase)
		newHead := git(path, "rev-parse", "HEAD")
		r.BaseSHA = newBase
		r.State.BaseSHA = newBase
		r.PR.BaseSHA = newBase
		r.PR.HeadSHA = oldHead
		r.Worktree = path
		r.PR.Mergeable = boolValue(false)
		r.PR.MergeableState = "dirty"
		r.State.PRRevision = &lease.PRRevision{Number: number, Intent: "rebase", LocalHeadSHA: newHead, RemoteHeadSHA: oldHead, Branch: branch}
		r.State.LastRebase = &lease.RebaseEvidence{Status: "completed", OldBase: base, NewBase: newBase, OldHead: oldHead, NewHead: newHead, Verified: false}
		s.Tracker = &workTracker{Tracker: s.Tracker}
		s.Fork = fork
		s.ForkRemote = "fork"
		for _, tc := range []struct {
			name    string
			mutate  func(*Result)
			allowed bool
		}{
			{name: "completed unpublished", allowed: true},
			{name: "missing", mutate: func(r *Result) { r.State.LastRebase = nil }},
			{name: "in progress", mutate: func(r *Result) { r.State.LastRebase.Status = "in-progress" }},
			{name: "wrong PR", mutate: func(r *Result) { r.State.PRRevision.Number++ }},
			{name: "wrong intent", mutate: func(r *Result) { r.State.PRRevision.Intent = "work" }},
			{name: "wrong head", mutate: func(r *Result) { r.State.LastRebase.NewHead = "missing" }},
			{name: "wrong base", mutate: func(r *Result) { r.State.LastRebase.NewBase = "missing" }},
			{name: "missing objects", mutate: func(r *Result) { r.State.LastRebase.OldHead = "missing"; r.State.LastRebase.Verified = true }},
			{name: "missing worktree", mutate: func(r *Result) { r.Worktree += "-absent" }},
			{name: "target advanced", mutate: func(r *Result) { r.PR.BaseSHA = "advanced" }},
		} {
			t.Run(fmt.Sprintf("fork=%t/%s", fork, tc.name), func(t *testing.T) {
				// Copy all evidence so the verification cannot mutate the source fixture.
				raw, _ := json.Marshal(r)
				var candidate Result
				if err := json.Unmarshal(raw, &candidate); err != nil {
					t.Fatal(err)
				}
				if tc.mutate != nil {
					tc.mutate(&candidate)
				}
				before, _ := json.Marshal(candidate.State)
				indexPath := git(path, "rev-parse", "--git-path", "index")
				indexBefore, err := os.ReadFile(indexPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.checkWork(context.Background(), &candidate); err != nil {
					t.Fatal(err)
				}
				if (candidate.WorkPreflight.Status == "ready") != tc.allowed {
					t.Fatalf("%+v", candidate.WorkPreflight)
				}
				indexAfter, err := os.ReadFile(indexPath)
				if err != nil || !bytes.Equal(indexBefore, indexAfter) {
					t.Fatal("inspection changed index", err)
				}
				if got := git(path, "rev-parse", "HEAD"); got != newHead {
					t.Fatal("inspection changed HEAD")
				}
				after, _ := json.Marshal(candidate.State)
				if string(before) != string(after) {
					t.Fatal("evidence mutated")
				}
				if candidate.WorkPreflight.Mergeable == nil || *candidate.WorkPreflight.Mergeable {
					t.Fatal("raw conflict lost")
				}
				if tc.allowed && !strings.Contains(strings.Join(candidate.WorkPreflight.Warnings, " "), "published head superseded by local rebase") {
					t.Fatal("missing warning")
				}
			})
		}
		_ = issue
	}
}

type forkWorkTracker struct{ *workTracker }

func (forkWorkTracker) UpstreamRepo() string { return "octo/swarm" }

func TestPRWorkForkRefusesBeforeGateway(t *testing.T) {
	s, issue, number, base := fixture(t)
	ctx := context.Background()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", s.Root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if _, err := s.Manager.ApproveTask(ctx, issue, "alice"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Manager.AcquireFork(ctx, issue, "alice", "mock", "all", "alice/swarm", time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	branch := workspace.ForkBranchName(issue, st.Lease.ClaimID)
	bare := filepath.Join(t.TempDir(), "fork.git")
	git("init", "--bare", bare)
	git("remote", "add", "fork", bare)
	git("config", "remote.fork.swarmSlug", "alice/swarm")
	git("push", "fork", base+":refs/heads/"+branch)
	if _, err = s.Manager.RecordCheckpoint(ctx, issue, "alice", branch, base); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Manager.RecordSubmission(ctx, issue, "alice", number, fmt.Sprintf("https://example.invalid/pull/%d", number)); err != nil {
		t.Fatal(err)
	}
	view := &forkPRTracker{Tracker: s.Tracker, branch: branch, head: func() string { return base }}
	p, err := view.GetPullRequest(ctx, number)
	if err != nil {
		t.Fatal(err)
	}
	p.Mergeable = boolValue(false)
	p.MergeableState = "dirty"
	tr := forkWorkTracker{&workTracker{Tracker: view, observations: []tracker.PullRequest{*p}}}
	gateway := &loopbackGateway{t: t, upstream: s.Manager, actor: "alice"}
	manager := lease.NewManager(tr, contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
	manager.SetStore(s.Root, s.Manager.Store())
	manager.SetWorkerGateway(gateway)
	svc := &Service{Root: s.Root, Tracker: tr, Manager: manager, Fork: true, ForkRemote: "fork"}
	before, _ := s.Manager.GetCurrentState(ctx, issue)
	snapshot, _ := json.Marshal(before)
	for _, dry := range []bool{false, true} {
		r, err := svc.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{InspectOnly: dry}})
		if (err == nil) != dry || r == nil || r.WorkPreflight.Status != "blocked" {
			t.Fatalf("dry=%t %+v %v", dry, r, err)
		}
		if len(gateway.operations) != 0 {
			t.Fatal("gateway mutation", gateway.operations)
		}
		after, _ := s.Manager.GetCurrentState(ctx, issue)
		raw, _ := json.Marshal(after)
		if !bytes.Equal(snapshot, raw) {
			t.Fatal("state changed")
		}
	}
}

func TestPRWorkUnknownDryRunPreservesWorkflowPreflight(t *testing.T) {
	s, issue, number, _ := fixture(t)
	p, err := s.Tracker.GetPullRequest(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	tr := &workTracker{Tracker: s.Tracker, observations: []tracker.PullRequest{*p}}
	s.Tracker = tr
	s.workWait = func(context.Context) error { return nil }
	before, _ := s.Manager.GetCurrentState(context.Background(), issue)
	snapshot, _ := json.Marshal(before)
	r, err := s.Work(context.Background(), Options{Number: number, Work: workflow.WorkOptions{InspectOnly: true}})
	if err != nil || r.Preflight == nil || r.WorkPreflight.Mergeability != "unknown" || len(r.WorkPreflight.Warnings) != 1 || tr.calls != 3 {
		t.Fatalf("%+v calls=%d err=%v", r, tr.calls, err)
	}
	after, _ := s.Manager.GetCurrentState(context.Background(), issue)
	raw, _ := json.Marshal(after)
	if !bytes.Equal(snapshot, raw) {
		t.Fatal("state changed")
	}
}

func TestPRWorkPausedPreflight(t *testing.T) {
	s, _, number, _ := fixture(t)
	r, remote, err := s.openForMutation(context.Background(), number, false)
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := s.preparePR(context.Background(), r, remote, "", false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", path, "rev-parse", "--git-path", "rebase-merge").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(strings.TrimSpace(string(out)), 0755); err != nil {
		t.Fatal(err)
	}
	r.PR.Mergeable = boolValue(false)
	r.PR.MergeableState = "dirty"
	s.Tracker = &workTracker{Tracker: s.Tracker, observations: []tracker.PullRequest{*r.PR}}
	result, err := s.Work(context.Background(), Options{Number: number, Work: workflow.WorkOptions{InspectOnly: true}})
	if err != nil || result.Status != "blocked" || !strings.Contains(strings.Join(result.WorkPreflight.Blockers, " "), "paused rebase") {
		t.Fatalf("%+v %v", result, err)
	}
	if len(result.WorkPreflight.Commands) != 3 || !strings.Contains(result.WorkPreflight.Commands[1], "--continue --resolve") || !strings.Contains(result.WorkPreflight.Commands[2], "--abort") {
		t.Fatalf("%+v", result.WorkPreflight)
	}
}
