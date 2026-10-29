package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
)

// Block a real foreground projection only after the command attaches its tracker.
// Renewal contexts are masked and cannot satisfy this handoff.
type commandActivityTracker struct {
	tracker.Tracker
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *commandActivityTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	e := progress.Event{Status: "running"}
	progress.ActivityFrom(ctx).Enrich(&e, time.Now())
	if e.Operation != "" {
		first := false
		b.once.Do(func() { first = true; close(b.entered) })
		if first {
			select {
			case <-b.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return b.Tracker.GetIssue(ctx, n)
}
func commandActivityWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("command activity handoff timed out")
	}
}
func TestCommandProgressShowsOperation(t *testing.T) {
	for _, path := range []string{"issue-work", "review-plan", "pr-work", "issue-submit"} {
		t.Run(path, func(t *testing.T) {
			f := newCLIFixture(t)
			n := f.create(t, "agent")
			f.must(t, "issue", "approve", strconv.Itoa(n))
			args := []string{"issue", "work", strconv.Itoa(n), "--agent", "mock", "--phase", "plan", "--json"}
			switch path {
			case "review-plan":
				f.must(t, "issue", "work", strconv.Itoa(n), "--agent", "mock", "--phase", "plan", "--progress=false")
				args = []string{"issue", "review-plan", strconv.Itoa(n), "--agent", "mock", "--json"}
			case "issue-submit":
				f.must(t, "issue", "work", strconv.Itoa(n), "--agent", "mock", "--progress=false")
				args = []string{"issue", "submit", strconv.Itoa(n), "--json"}
			case "pr-work":
				var number int
				f, _, number = submittedPRFixture(t, "agent")
				args = []string{"pr", "work", strconv.Itoa(number), "--agent", "mock", "--phase", "plan", "--json"}
			}
			b := &commandActivityTracker{Tracker: f.tracker, entered: make(chan struct{}), release: make(chan struct{})}
			ticks := make(chan time.Time, 1)
			events := make(chan progress.Event, 100)
			base := time.Now()
			var seconds atomic.Int64
			clock := func() time.Time { return base.Add(time.Duration(seconds.Load()) * time.Second) }
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			var release sync.Once
			var reporter *workflow.ProgressReporter
			var session *progress.Session
			var runErr error
			// Cancellation and release precede the bounded join even on assertion failure.
			t.Cleanup(func() { cancel(); release.Do(func() { close(b.release) }); commandActivityWait(t, done) })
			opts := &RootOptions{trackerOverride: b}
			observe := func(emit func(progress.Event)) func(progress.Event) {
				return func(e progress.Event) {
					emit(e)
					select {
					case events <- e:
					case <-ctx.Done():
					}
				}
			}
			opts.progressReporter = func(ctx context.Context, emit func(workflow.ProgressEvent)) *workflow.ProgressReporter {
				reporter = workflow.NewProgressReporterWithTicks(ctx, observe(emit), ticks, clock)
				return reporter
			}
			opts.progressSession = func(ctx context.Context, emit func(progress.Event)) *progress.Session {
				session = progress.New(ctx, 0, 0, observe(emit), ticks, clock, nil)
				return session
			}
			cmd := f.command(opts)
			var out, diag bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&diag)
			cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
			go func() { defer close(done); runErr = cmd.ExecuteContext(ctx) }()
			commandActivityWait(t, b.entered)
			previous := time.Duration(0)
			for _, sec := range []int64{30, 45} {
				seconds.Store(sec)
				ticks <- clock()
				waiting := true
				for waiting {
					select {
					case e := <-events:
						if e.Status == "running" {
							if e.Operation != "tracker update" || e.Role != "" || e.OperationElapsed <= previous || e.Elapsed != time.Duration(sec)*time.Second {
								t.Fatal(e)
							}
							previous = e.OperationElapsed
							waiting = false
						}
					case <-time.After(5 * time.Second):
						t.Fatal("missing enriched command heartbeat")
					}
				}
			}
			release.Do(func() { close(b.release) })
			commandActivityWait(t, done)
			if runErr != nil {
				t.Fatalf("%v: %s", runErr, diag.String())
			}
			if reporter != nil {
				reporter.Close()
			}
			if session != nil {
				commandActivityWait(t, session.Done())
			}
			ticks <- time.Now()
			if len(ticks) != 1 {
				t.Fatal("reporter consumed tick after completion")
			}
			for len(events) > 0 {
				e := <-events
				if e.Status == "running" {
					t.Fatal("unacknowledged heartbeat", e)
				}
			}
			if strings.Count(diag.String(), "; coordinator tracker update for ") != 2 {
				t.Fatal(diag.String())
			}
			dec := json.NewDecoder(&out)
			var document map[string]any
			if err := dec.Decode(&document); err != nil {
				t.Fatal(err)
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				t.Fatal("extra stdout", err)
			}
			if path != "issue-submit" {
				keys := make([]string, 0, len(document))
				for k := range document {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				want := []string{"kind", "result", "status"}
				if !reflect.DeepEqual(keys, want) {
					t.Fatalf("%s JSON keys: %v", path, keys)
				}
			}
		})
	}
}
