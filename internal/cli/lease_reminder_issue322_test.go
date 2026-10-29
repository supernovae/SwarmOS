package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/fork"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
)

func TestIssue322Advisory(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                string
		duration, remaining time.Duration
		renew, expired      bool
	}{
		{"above", 3 * time.Hour, time.Hour + 1, false, false},
		{"exact", 3 * time.Hour, time.Hour, false, false},
		{"below", 3 * time.Hour, time.Hour - 1, true, false},
		{"ninety", 90 * time.Minute, 29 * time.Minute, true, false},
		{"fraction", 10, 3, true, false},
		{"fractionAbove", 10, 4, false, false},
		{"zero", time.Hour, 0, true, true},
		{"expired", time.Hour, -time.Hour, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantRenew := tc.renew
			state := &lease.IssueState{Lease: contract.Lease{ExpiresAt: now.Add(tc.remaining), ClaimID: "secret-token"}}
			before := *state
			calls := 0
			var out bytes.Buffer
			writeLeaseReminder(t.Context(), func(ctx context.Context, n int) (*lease.IssueState, error) {
				calls++
				if n != 322 {
					t.Fatal(n)
				}
				return state, nil
			}, tc.duration, true, 322, &RootOptions{nowFn: func() time.Time { return now }}, "local", &out)
			if calls != 1 || !reflect.DeepEqual(before, *state) {
				t.Fatal("read mutated state", calls)
			}
			if strings.Contains(out.String(), "Renew:") != wantRenew || strings.Contains(out.String(), "(expired)") != tc.expired || strings.Contains(out.String(), "secret-token") {
				t.Fatal(out.String())
			}
		})
	}
	for _, tc := range []struct {
		name     string
		state    *lease.IssueState
		err      error
		duration time.Duration
	}{
		{"missing", nil, nil, time.Hour}, {"unknown", &lease.IssueState{}, nil, time.Hour}, {"lookup", nil, errors.New("secret lookup"), time.Hour}, {"invalid", nil, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writeLeaseReminder(t.Context(), func(context.Context, int) (*lease.IssueState, error) { return tc.state, tc.err }, tc.duration, true, 322, &RootOptions{}, "local", &out)
			if out.Len() != 0 {
				t.Fatal(out.String())
			}
		})
	}
	var out bytes.Buffer
	writeLeaseReminder(t.Context(), func(context.Context, int) (*lease.IssueState, error) {
		return &lease.IssueState{Lease: contract.Lease{ExpiresAt: now}}, nil
	}, time.Hour, false, 322, &RootOptions{nowFn: func() time.Time { return now }}, "local", &out)
	if strings.Contains(out.String(), "Renew:") {
		t.Fatal(out.String())
	}
}

func TestIssue322Selectors(t *testing.T) {
	for _, tc := range []struct {
		name, backend string
		opts          RootOptions
		contains      []string
		absent        string
	}{
		{"local", "local", RootOptions{RepoRoot: "/some path/it's"}, []string{"--backend 'local'", "--root '/some path/it'\\''s'"}, "--upstream-remote"},
		{"github", "github (owner/repo)", RootOptions{Repo: "owner/repo", ForkRemote: "unused"}, []string{"--backend 'github'", "--upstream-remote 'origin'", "--repo 'owner/repo'"}, "--fork-remote"},
		{"fork", "github", RootOptions{Fork: true}, []string{"--fork --fork-remote 'fork'"}, ""},
		{"profile", "github", RootOptions{Fork: true, profileUpstreamRemote: "upstream"}, []string{"--upstream-remote 'upstream'", "--fork-remote 'origin'"}, ""},
		{"custom", "github", RootOptions{Fork: true, UpstreamRemote: "project", ForkRemote: "personal"}, []string{"--upstream-remote 'project'", "--fork-remote 'personal'"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := leaseReminderRenewCommand(&tc.opts, tc.backend, 322)
			for _, s := range tc.contains {
				if !strings.Contains(command, s) {
					t.Fatal(command, s)
				}
			}
			if tc.absent != "" && strings.Contains(command, tc.absent) {
				t.Fatal(command)
			}
			raw, err := exec.Command("bash", "-c", "set -- "+command+"; printf '%s\\0' \"$@\"").Output()
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
			root := newRootCmdWithOptions(&RootOptions{})
			cmd, rest, err := root.Find(args[1:])
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Name() != "renew" || cmd.Parent().Name() != "issue" {
				t.Fatal(command)
			}
			if err := cmd.ParseFlags(rest); err != nil {
				t.Fatal(err)
			}
			if got := cmd.Flags().Args(); len(got) != 1 || got[0] != "322" {
				t.Fatal(got)
			}
		})
	}
}

func issue322Run(t *testing.T, f *cliFixture, now time.Time, args ...string) (string, string, error) {
	t.Helper()
	cmd := f.command(&RootOptions{nowFn: func() time.Time { return now }})
	var out, diag bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
	err := cmd.Execute()
	return out.String(), diag.String(), err
}
func issue322JSON(t *testing.T, out string) {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(out))
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err, out)
	}
	if err := d.Decode(&v); err != io.EOF {
		t.Fatal("extra JSON", err, out)
	}
}

func TestIssue322IssueCommands(t *testing.T) {
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	s := strconv.Itoa(n)
	f.must(t, "issue", "approve", s)
	f.must(t, "issue", "claim", s, "--agent", "mock")
	wt := filepath.Join(f.root, ".swarm", "worktrees", "issue-"+s)
	for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
		if err := os.WriteFile(filepath.Join(wt, ".swarm", "active", name), []byte("# Plan\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		t.Helper()
		issue322CompletionCases(t, f, n, args...)
		before := f.state(t, n)
		now := before.Lease.ExpiresAt.Add(-time.Minute)
		out, diag, err := issue322Run(t, f, now, args...)
		if err != nil {
			t.Fatal(err, out, diag)
		}
		issue322JSON(t, out)
		if !strings.Contains(diag, "Claim time remaining") || !strings.Contains(diag, "issue renew "+s) {
			t.Fatal(diag)
		}
		after := f.state(t, n)
		if before.Lease.ClaimID != after.Lease.ClaimID || before.Lease.Renewals != after.Lease.Renewals {
			t.Fatal("lease changed")
		}
	}
	run("issue", "stage", s, "--phase", "plan", "--json")
	f.must(t, "issue", "review-plan", s, "--agent", "mock")
	run("issue", "stage", s, "--phase", "implement", "--json")
	run("issue", "validate", s, "--json", "--progress=false")
}

func TestIssue322PRCommands(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	prepared := readPROutcome(t, f.must(t, "pr", "work", p, "--phase", "plan", "--agent", "mock", "--progress=false", "--json"))
	for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
		if err := os.WriteFile(filepath.Join(prepared.Worktree, ".swarm", "active", name), []byte("# Revised plan\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, phase := range []string{"plan", "implement"} {
		if phase == "implement" {
			f.must(t, "pr", "work", p, "--phase", "review", "--agent", "mock", "--progress=false")
		}
		issue322CompletionCases(t, f, issue, "pr", "stage", p, "--phase", phase, "--json")
		before := f.state(t, issue)
		out, diag, err := issue322Run(t, f, before.Lease.ExpiresAt.Add(-time.Minute), "pr", "stage", p, "--phase", phase, "--json")
		if err != nil {
			t.Fatal(err, out, diag)
		}
		issue322JSON(t, out)
		if !strings.Contains(diag, "issue renew "+strconv.Itoa(issue)) {
			t.Fatal(diag)
		}
		after := f.state(t, issue)
		if before.Lease.ClaimID != after.Lease.ClaimID || before.Lease.Renewals != after.Lease.Renewals {
			t.Fatal("lease changed")
		}
	}
}

func TestIssue322FailureAndFreshness(t *testing.T) {
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	s := strconv.Itoa(n)
	f.must(t, "issue", "approve", s)
	f.must(t, "issue", "claim", s, "--agent", "mock")
	state := f.state(t, n)
	// No approved plan: the existing implementation gate must still reject staging.
	out, diag, err := issue322Run(t, f, state.Lease.ExpiresAt.Add(-time.Minute), "issue", "stage", s, "--phase", "implement", "--json")
	if err == nil {
		t.Fatal("unapproved implementation succeeded")
	}
	issue322JSON(t, out)
	if !strings.Contains(diag, "Claim time remaining") {
		t.Fatal(diag)
	}
	// An expired local view is deliberately ignored: the final authoritative read
	// supplies the latest expiry. This clock is sampled only after that read.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var advice bytes.Buffer
	original := errors.New("original outcome")
	for _, lookupErr := range []error{nil, errors.New("secret lookup failure")} {
		got := func() (runErr error) {
			defer writeLeaseReminder(t.Context(), func(context.Context, int) (*lease.IssueState, error) {
				return &lease.IssueState{Lease: contract.Lease{ExpiresAt: now.Add(3 * time.Hour)}}, lookupErr
			}, 4*time.Hour, true, n, &RootOptions{nowFn: func() time.Time { return now }}, "local", &advice)
			return original
		}()
		if got != original {
			t.Fatal("original error replaced", got)
		}
	}
	if strings.Contains(advice.String(), "Renew:") || strings.Contains(advice.String(), "secret") {
		t.Fatal(advice.String())
	}
}

func TestIssue322ValidationFailure(t *testing.T) {
	f := newCLIFixture(t)
	policy := "required_checks:\n  - name: failing-check\n    command: exit 7\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(f.root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".")
	prGit(t, f.root, "commit", "-m", "failing approved check")
	n := f.create(t, "agent")
	s := strconv.Itoa(n)
	f.must(t, "issue", "approve", s)
	f.must(t, "issue", "claim", s, "--agent", "mock")
	for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
		if err := os.WriteFile(filepath.Join(f.root, ".swarm", "worktrees", "issue-"+s, ".swarm", "active", name), []byte("# Plan\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	issue322CompletionCases(t, f, n, "issue", "validate", s, "--json", "--progress=false")
	before := f.state(t, n)
	out, diag, err := issue322Run(t, f, before.Lease.ExpiresAt.Add(-time.Minute), "issue", "validate", s, "--json", "--progress=false")
	if err == nil {
		t.Fatal("validation failure lost")
	}
	issue322JSON(t, out)
	if !strings.Contains(out, `"status":"failed"`) || !strings.Contains(diag, "issue renew "+s) {
		t.Fatal(out, diag)
	}
	after := f.state(t, n)
	if after.Lease.ClaimID != before.Lease.ClaimID || after.Lease.Renewals != before.Lease.Renewals {
		t.Fatal("lease changed")
	}
}

type issue322BrokenWriter struct{}

func (issue322BrokenWriter) Write([]byte) (int, error) { return 0, errors.New("writer failed") }
func TestIssue322ReadOrderingAndWriterFailure(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	readDone := false
	opts := &RootOptions{nowFn: func() time.Time {
		if !readDone {
			t.Fatal("clock sampled before final read")
		}
		return now
	}}
	read := func(context.Context, int) (*lease.IssueState, error) {
		readDone = true
		return &lease.IssueState{Lease: contract.Lease{ExpiresAt: now.Add(time.Minute)}}, nil
	}
	writeLeaseReminder(t.Context(), read, time.Hour, true, 322, opts, "local", issue322BrokenWriter{})
}

func TestIssue322ApprovedDuration(t *testing.T) {
	f := newCLIFixture(t)
	configPath := filepath.Join(f.root, ".swarm/config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// Change the candidate policy after the task was approved and claimed.
	n := f.create(t, "agent")
	s := strconv.Itoa(n)
	f.must(t, "issue", "approve", s)
	f.must(t, "issue", "claim", s, "--agent", "mock")
	state := f.state(t, n)
	cfg, duration, err := approvedLeasePolicy(f.root, "origin", state.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	altered := strings.Replace(string(data), cfg.Lease.Duration, "1ns", 1)
	if altered == string(data) {
		t.Fatal("fixture did not replace duration")
	}
	if err := os.WriteFile(configPath, []byte(altered), 0600); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(f.root, ".swarm/worktrees/issue-"+s)
	for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
		if err := os.WriteFile(filepath.Join(wt, ".swarm/active", name), []byte("# Plan\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, diag, err := issue322Run(t, f, state.Lease.ExpiresAt.Add(-duration/3+time.Nanosecond), "issue", "stage", s, "--phase", "plan", "--json")
	if err != nil {
		t.Fatal(err, out, diag)
	}
	issue322JSON(t, out)
	if !strings.Contains(diag, "Renew:") {
		t.Fatal("candidate duration changed threshold", diag)
	}
}

// issue322CompletionWriter runs only when normal command output is written.
// Stage failure JSON is deferred until after the reminder and cannot use this barrier.
type issue322CompletionWriter struct {
	forward io.Writer
	bytes.Buffer
	complete func()
	writeErr error
	fired    bool
}

func (w *issue322CompletionWriter) Write(p []byte) (int, error) {
	if !w.fired {
		w.fired = true
		w.complete()
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.forward != nil {
		return w.forward.Write(p)
	}
	return w.Buffer.Write(p)
}

func issue322CompletionCases(t *testing.T, f *cliFixture, issue int, args ...string) {
	t.Helper()
	restore := snapshotCLIFixture(t, f)
	defer restore(t)
	initial := f.state(t, issue)
	now := initial.Lease.ExpiresAt.Add(-time.Minute)
	writerErr := errors.New("issue322 stdout sentinel")
	var baseline *lease.IssueState
	for _, mode := range []string{"baseline", "lookup-failure", "fresh-expiry", "writer-failure"} {
		restore(t)
		ctx, cancel := context.WithCancel(t.Context())
		var atOutput *lease.IssueState
		var diag bytes.Buffer
		out := &issue322CompletionWriter{complete: func() {
			atOutput = f.state(t, issue)
			switch mode {
			case "lookup-failure", "writer-failure":
				cancel() // FileStore.Load checks ctx.Err before reading authoritative state.
			case "fresh-expiry":
				store := coordination.NewFileStore(f.root)
				raw, rev, err := store.Load(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				var state lease.IssueState
				if err := json.Unmarshal(raw, &state); err != nil {
					t.Fatal(err)
				}
				state.Lease.ExpiresAt = now.Add(2 * time.Hour)
				raw, err = json.Marshal(&state)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.CompareAndSwap(t.Context(), issue, rev, raw); err != nil {
					t.Fatal(err)
				}
				atOutput = &state // exactly one scripted authoritative update, not a timer heartbeat.
			}
		}}
		if mode == "writer-failure" {
			out.writeErr = writerErr
		}
		cmd := f.command(&RootOptions{nowFn: func() time.Time { return now }})
		cmd.SetOut(out)
		cmd.SetErr(&diag)
		cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
		err := cmd.ExecuteContext(ctx)
		cancel()
		if !out.fired {
			t.Fatal("missing completion barrier", args)
		}
		if mode == "writer-failure" {
			if !errors.Is(err, writerErr) {
				t.Fatalf("writer identity lost: %v", err)
			}
		} else {
			issue322JSON(t, out.String())
			failed := strings.Contains(out.String(), `"status":"failed"`)
			if (err != nil) != failed {
				t.Fatalf("outcome changed: %v %s", err, out.String())
			}
			if failed && !errors.Is(err, validation.ErrRequiredCheckFailed) {
				t.Fatalf("validation identity lost: %v", err)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("advisory error escaped: %v", err)
			}
		}
		text := diag.String()
		if strings.Contains(text, initial.Lease.ClaimID) || strings.Contains(text, "context canceled") {
			t.Fatalf("private advisory data: %s", text)
		}
		want := ""
		if mode == "baseline" {
			want = fmt.Sprintf("Claim time remaining for issue #%d: 1m0s\nRenew: swarm --backend 'local' --root '%s' issue renew %d\n", issue, f.root, issue)
		} else if mode == "fresh-expiry" {
			want = fmt.Sprintf("Claim time remaining for issue #%d: 2h0m0s\n", issue)
		}
		if text != want {
			t.Fatalf("%s diagnostics: got %q want %q", mode, text, want)
		}
		after := f.state(t, issue)
		if !reflect.DeepEqual(atOutput, after) {
			t.Fatalf("%s: reminder/cleanup mutated authoritative state", mode)
		}
		if mode == "baseline" {
			baseline = after
		}
		expected := *baseline
		if mode == "fresh-expiry" {
			expected.Lease.ExpiresAt = now.Add(2 * time.Hour)
		}
		issue322SameAuthority(t, &expected, after)
		if after.Lease.ClaimID != initial.Lease.ClaimID || after.Lease.Owner != initial.Lease.Owner || after.Lease.Renewals != initial.Lease.Renewals {
			t.Fatal("command changed claim/renewals", mode)
		}
		if baseline != nil && (after.BaseSHA != baseline.BaseSHA || after.PolicyDigest != baseline.PolicyDigest) {
			t.Fatal("command changed approved authority", mode)
		}
	}
}

func TestIssue322GitHubCommands(t *testing.T) {
	for _, forkMode := range []bool{false, true} {
		t.Run(strconv.FormatBool(forkMode), func(t *testing.T) {
			env := newFixtureEnv(t)
			ctx := context.Background()
			tmp := t.TempDir()

			git := func(dir string, args ...string) string {
				t.Helper()
				out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
				}
				return strings.TrimSpace(string(out))
			}

			originBare := filepath.Join(tmp, "octo", "swarm.git")
			aliceForkBare := filepath.Join(tmp, "alice", "swarm.git")
			for _, dir := range []string{originBare, aliceForkBare} {
				if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			git(tmp, "init", "--bare", "--initial-branch=main", originBare)

			upstreamRoot := filepath.Join(tmp, "upstream")
			git(tmp, "clone", originBare, upstreamRoot)
			git(upstreamRoot, "config", "user.name", "Maintainer")
			git(upstreamRoot, "config", "user.email", "maintainer@example.invalid")
			pin := "a1b2c3d4e5f60123456789abcdef0123456789ab"
			if _, err := harness.InitRepository(harness.InitOptions{
				RepoRoot:        upstreamRoot,
				SwarmVersion:    pin,
				ForkCoordinator: true,
			}); err != nil {
				t.Fatal(err)
			}
			cfgPath := filepath.Join(upstreamRoot, ".swarm", "config.yaml")
			cfgBytes, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			cfgWithMock := string(cfgBytes) + "\n  mock:\n    binary: git\n    harness: mock\n"
			if err := os.WriteFile(cfgPath, []byte(cfgWithMock), 0o644); err != nil {
				t.Fatal(err)
			}
			checksPolicy := "required_checks:\n  - name: git-available\n    command: test ! -f issue322-fail\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
			if err := os.WriteFile(filepath.Join(upstreamRoot, ".swarm", "validation", "required-checks.yaml"), []byte(checksPolicy), 0o644); err != nil {
				t.Fatal(err)
			}
			git(upstreamRoot, "add", ".")
			git(upstreamRoot, "commit", "-m", "approved upstream baseline")
			git(upstreamRoot, "push", "origin", "main")
			git(originBare, "symbolic-ref", "HEAD", "refs/heads/main")

			git(tmp, "clone", "--bare", originBare, aliceForkBare)

			var mu sync.Mutex
			recordingSentinel := errors.New("issue322 recording transport sentinel")
			injectRecordingSentinel := false
			pollRecordingFault := false
			now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
			nextCommentID := int64(1000)
			nextPRNum := 0
			wuBody := "```yaml swarm-workunit\nid: ISSUE-42\nobjective: Exercise fork contributor flow\nconstraints: [No upstream push access]\nacceptance: [Cross-repo Draft PR opened]\nworkflow: feature\ngate: agent\nallow_harness_changes: true\n```"
			issues := map[int]*tracker.Issue{
				42: {
					Number:  42,
					Title:   "Fork mode smoke task",
					Body:    wuBody,
					State:   "open",
					Author:  "maintainer",
					HTMLURL: "https://github.com/octo/swarm/issues/42",
				},
			}
			comments := make(map[int64]*tracker.Comment)
			issueComments := make(map[int][]int64)
			prs := make(map[int]*tracker.PullRequest)

			resolveHeadSHA := func(head string) string {
				parts := strings.SplitN(head, ":", 2)
				branch := head
				bare := originBare
				if len(parts) == 2 {
					branch = parts[1]
					switch parts[0] {
					case "alice":
						bare = aliceForkBare
					}
				}
				out, err := gitfixture.Command(t, "", "-C", bare, "rev-parse", "refs/heads/"+branch).Output()
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(out))
			}

			newGHTracker := func(login, userType string) *tracker.GitHubTracker {
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					writeResp := func(v any) {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(v)
					}
					switch {
					case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-runs"):
						writeResp(map[string]any{"check_runs": []any{}})
					case r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/reviews") || strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/pulls/") && strings.HasSuffix(r.URL.Path, "/comments")):
						writeResp([]any{})
					case r.Method == http.MethodGet && r.URL.Path == "/user":
						writeResp(map[string]any{"login": login, "type": userType})
					case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/collaborators/"):
						perm := "read"
						if login == "maintainer" {
							perm = "admin"
						}
						writeResp(map[string]any{"permission": perm})
					case r.Method == http.MethodGet && r.URL.Path == "/repos/alice/swarm":
						owner := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")[0]
						writeResp(map[string]any{
							"full_name":   owner + "/swarm",
							"name":        "swarm",
							"fork":        true,
							"owner":       map[string]any{"login": owner},
							"source":      map[string]any{"full_name": "octo/swarm"},
							"permissions": map[string]any{"push": login == owner},
						})
					case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/"):
						id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/"), 10, 64)
						c, ok := comments[id]
						if !ok {
							http.Error(w, "not found", http.StatusNotFound)
							return
						}
						writeResp(map[string]any{
							"id":         c.ID,
							"body":       c.Body,
							"created_at": c.CreatedAt,
							"updated_at": c.UpdatedAt,
							"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
						})
					case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/"):
						id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/"), 10, 64)
						c, ok := comments[id]
						if !ok {
							http.Error(w, "not found", http.StatusNotFound)
							return
						}
						var payload struct {
							Body string `json:"body"`
						}
						_ = json.NewDecoder(r.Body).Decode(&payload)
						c.Body = payload.Body
						writeResp(map[string]any{
							"id":         c.ID,
							"body":       c.Body,
							"created_at": c.CreatedAt,
							"updated_at": c.UpdatedAt,
							"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
						})
					case strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/"):
						rest := strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/issues/")
						parts := strings.Split(rest, "/")
						issueNum, _ := strconv.Atoi(parts[0])
						iss, ok := issues[issueNum]
						if !ok {
							http.Error(w, "not found", http.StatusNotFound)
							return
						}
						if len(parts) == 1 && r.Method == http.MethodGet {
							var lbs []map[string]string
							for _, l := range iss.Labels {
								lbs = append(lbs, map[string]string{"name": l})
							}
							writeResp(map[string]any{
								"id":     issueNum,
								"number": iss.Number,
								"title":  iss.Title,
								"body":   iss.Body,
								"state":  iss.State,
								"user":   map[string]any{"login": iss.Author},
								"labels": lbs,
							})
							return
						}
						if len(parts) == 2 && parts[1] == "comments" {
							if r.Method == http.MethodGet {
								var list []map[string]any
								for _, cid := range issueComments[issueNum] {
									c := comments[cid]
									list = append(list, map[string]any{
										"id":         c.ID,
										"body":       c.Body,
										"created_at": c.CreatedAt,
										"updated_at": c.UpdatedAt,
										"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
									})
								}
								// Match GitHub's bounded pages as the command matrix accumulates comments.
								page, _ := strconv.Atoi(r.URL.Query().Get("page"))
								if page < 1 {
									page = 1
								}
								first := min((page-1)*100, len(list))
								last := min(first+100, len(list))
								writeResp(list[first:last])
								return
							}
							if r.Method == http.MethodPost {
								var payload struct {
									Body string `json:"body"`
								}
								_ = json.NewDecoder(r.Body).Decode(&payload)
								nextCommentID++
								c := &tracker.Comment{
									ID:         nextCommentID,
									IssueNum:   issueNum,
									Author:     login,
									AuthorType: userType,
									Body:       payload.Body,
									CreatedAt:  now,
									UpdatedAt:  now,
								}
								comments[c.ID] = c
								issueComments[issueNum] = append(issueComments[issueNum], c.ID)
								writeResp(map[string]any{
									"id":         c.ID,
									"body":       c.Body,
									"created_at": c.CreatedAt,
									"updated_at": c.UpdatedAt,
									"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
								})
								return
							}
						}
						if len(parts) == 2 && parts[1] == "labels" && r.Method == http.MethodPost {
							if login != "maintainer" && login != fork.ActionsBotLogin {
								http.Error(w, "contributor cannot mutate upstream labels", http.StatusForbidden)
								return
							}
							var payload struct {
								Labels []string `json:"labels"`
							}
							_ = json.NewDecoder(r.Body).Decode(&payload)
							for _, lb := range payload.Labels {
								if !slices.Contains(iss.Labels, lb) {
									iss.Labels = append(iss.Labels, lb)
								}
							}
							writeResp(map[string]any{})
							return
						}
						if len(parts) == 3 && parts[1] == "labels" && r.Method == http.MethodDelete {
							if login != "maintainer" && login != fork.ActionsBotLogin {
								http.Error(w, "contributor cannot mutate upstream labels", http.StatusForbidden)
								return
							}
							target := parts[2]
							var kept []string
							for _, lb := range iss.Labels {
								if lb != target {
									kept = append(kept, lb)
								}
							}
							iss.Labels = kept
							w.WriteHeader(http.StatusNoContent)
							return
						}
					case r.URL.Path == "/repos/octo/swarm/git/ref/heads/main" && r.Method == http.MethodGet:
						writeResp(map[string]any{"object": map[string]string{"sha": resolveHeadSHA("main")}})
						return
					case r.URL.Path == "/repos/octo/swarm/pulls" && r.Method == http.MethodGet:
						wantHead := r.URL.Query().Get("head")
						wantBase := r.URL.Query().Get("base")
						var list []map[string]any
						for _, pr := range prs {
							qualified := pr.Head
							if pr.HeadRepo != "" {
								owner := strings.Split(pr.HeadRepo, "/")[0]
								qualified = owner + ":" + pr.Head
							}
							if pr.Draft && qualified == wantHead && pr.Base == wantBase {
								list = append(list, map[string]any{
									"number":   pr.Number,
									"title":    pr.Title,
									"body":     pr.Body,
									"draft":    pr.Draft,
									"html_url": pr.HTMLURL,
								})
							}
						}
						writeResp(list)
						return
					case r.URL.Path == "/repos/octo/swarm/pulls" && r.Method == http.MethodPost:
						var payload struct {
							Title string `json:"title"`
							Body  string `json:"body"`
							Head  string `json:"head"`
							Base  string `json:"base"`
							Draft bool   `json:"draft"`
						}
						_ = json.NewDecoder(r.Body).Decode(&payload)
						nextPRNum++
						headBranch := payload.Head
						headRepo := "octo/swarm"
						if parts := strings.SplitN(payload.Head, ":", 2); len(parts) == 2 {
							headRepo = parts[0] + "/swarm"
							headBranch = parts[1]
						}
						pr := &tracker.PullRequest{
							Number:   nextPRNum,
							Title:    payload.Title,
							Body:     payload.Body,
							Head:     headBranch,
							HeadRepo: headRepo,
							HeadSHA:  resolveHeadSHA(payload.Head),
							Base:     payload.Base,
							BaseRepo: "octo/swarm",
							Draft:    payload.Draft,
							HTMLURL:  fmt.Sprintf("https://github.com/octo/swarm/pull/%d", nextPRNum),
						}
						prs[pr.Number] = pr
						issues[pr.Number] = &tracker.Issue{Number: pr.Number, State: "open", Body: pr.Body, Author: login}
						writeResp(map[string]any{
							"number":   pr.Number,
							"title":    pr.Title,
							"body":     pr.Body,
							"draft":    pr.Draft,
							"html_url": pr.HTMLURL,
						})
						return
					case strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/pulls/") && r.Method == http.MethodPatch:
						prNum, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/pulls/"))
						pr, ok := prs[prNum]
						if !ok {
							http.Error(w, "not found", http.StatusNotFound)
							return
						}
						var payload struct {
							Title string `json:"title"`
							Body  string `json:"body"`
						}
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Error(err)
						}
						pr.Title, pr.Body = payload.Title, payload.Body

						writeResp(map[string]any{
							"number":   pr.Number,
							"title":    pr.Title,
							"body":     pr.Body,
							"draft":    pr.Draft,
							"html_url": pr.HTMLURL,
							"head":     map[string]any{"ref": pr.Head},
							"base":     map[string]any{"ref": pr.Base},
						})
						return
					case strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/pulls/") && r.Method == http.MethodGet:
						prNum, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/pulls/"))
						pr, ok := prs[prNum]
						if !ok {
							http.Error(w, "not found", http.StatusNotFound)
							return
						}
						writeResp(map[string]any{
							"number":   pr.Number,
							"title":    pr.Title,
							"body":     pr.Body,
							"state":    "open",
							"draft":    pr.Draft,
							"html_url": pr.HTMLURL,
							"head": map[string]any{
								"ref":  pr.Head,
								"sha":  pr.HeadSHA,
								"repo": map[string]any{"full_name": pr.HeadRepo},
							},
							"base": map[string]any{
								"ref":  pr.Base,
								"repo": map[string]any{"full_name": pr.BaseRepo},
							},
						})
						return
					default:
						http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
					}
				})
				client, err := api.NewRESTClient(api.ClientOptions{
					Host:      "github.com",
					AuthToken: "test-token",
					Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						mu.Lock()
						fail := pollRecordingFault && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/comments")
						mu.Unlock()
						if fail {
							return nil, recordingSentinel
						}
						cloned := req.Clone(req.Context())
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, cloned)
						resp := rec.Result()
						resp.Request = cloned
						return resp, nil
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				return tracker.NewGitHubTrackerWithClient("octo", "swarm", client)
			}

			maintTracker := newGHTracker("maintainer", "User")
			botTracker := newGHTracker(fork.ActionsBotLogin, "Bot")
			aliceTracker := newGHTracker("alice", "User")

			coord := &fork.Coordinator{
				RepoRoot:     upstreamRoot,
				UpstreamRepo: "octo/swarm",
				Tracker:      botTracker,
				NowFn:        func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
				ForkGitURLFunc: func(forkRepo string) string {
					switch forkRepo {
					case "alice/swarm":
						return aliceForkBare
					default:
						return filepath.Join(tmp, "missing.git")
					}
				},
			}
			operationCounts := map[string]int{}
			recordingFence := false
			dispatch := func(ctx context.Context, issueNumber int, c *tracker.Comment) error {
				req, err := fork.ParseRequestComment(c.Body)
				if err != nil {
					return err
				}
				key := req.Operation
				if req.Artifact != nil {
					key += "/" + string(req.Artifact.Type)
				}
				operationCounts[key]++
				if injectRecordingSentinel && req.Operation == "update-artifact" && req.Artifact != nil && req.Artifact.Type == contract.ArtifactValidation {
					mu.Lock()
					pollRecordingFault = true
					mu.Unlock()
					return recordingSentinel
				}
				if recordingFence && req.Operation == "update-artifact" && req.Artifact != nil && req.Artifact.Type == contract.ArtifactValidation {
					// Corrupt only this recording request, leaving the live claim and
					// concurrent heartbeat reads intact. The real coordinator rejects it.
					req.ClaimID = "issue322-stale-recording-token"
					body, err := fork.FormatRequestComment(*req)
					if err != nil {
						return err
					}
					mu.Lock()
					c.Body = body
					comments[c.ID].Body = body
					mu.Unlock()
				}
				var ev fork.IssueCommentEvent
				ev.Action = "created"
				ev.Repository.FullName = "octo/swarm"
				ev.Issue.Number = issueNumber
				ev.Issue.State = "open"
				ev.Comment.ID = c.ID
				ev.Comment.Body = c.Body
				ev.Comment.CreatedAt = c.CreatedAt
				ev.Comment.UpdatedAt = c.UpdatedAt
				ev.Comment.User.Login = c.Author
				ev.Comment.User.Type = c.AuthorType
				_, err = coord.HandleEvent(ctx, ev)
				return err
			}

			root := upstreamRoot
			actor := maintTracker
			if forkMode {
				root = filepath.Join(tmp, "alice-clone")
				git(tmp, "clone", originBare, root)
				git(root, "config", "user.name", "Alice")
				git(root, "config", "user.email", "alice@example.invalid")
				git(root, "remote", "add", "fork", aliceForkBare)
				actor = aliceTracker
			}
			git(root, "remote", "add", "project", originBare)
			git(root, "remote", "add", "personal", aliceForkBare)
			// Real GitHub REST tracker with an in-memory transport, real Git ledger,
			// and real fork coordinator; no live service or renderer stand-ins.
			originIsFork := false
			finalReadFault := false
			run := func(opts RootOptions, complete func(), args ...string) (string, string, error) {
				opts.RepoRoot = root
				if opts.trackerOverride == nil {
					opts.githubTrackerFunc = func(string) (tracker.Tracker, error) { return actor, nil }
				}
				opts.nowFn = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
				opts.lookupEnv = env.lookup()
				opts.forkDispatcher = dispatch
				opts.forkPollTimeout = time.Second
				opts.remoteSlugFunc = func(_, remote string) string {
					if remote == "fork" || remote == "personal" || remote == "origin" && originIsFork {
						return "alice/swarm"
					}
					return "octo/swarm"
				}
				commandCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				cmd := newRootCmdWithOptions(&opts)
				var out bytes.Buffer
				var diag bytes.Buffer
				cmd.SetOut(&out)
				if complete != nil {
					cmd.SetOut(&issue322CompletionWriter{complete: func() {
						complete()
						if finalReadFault {
							cancel()
						}
					}, forward: &out})
				}
				cmd.SetErr(&diag)
				cmd.SetArgs(args)
				err := cmd.ExecuteContext(commandCtx)
				return out.String(), diag.String(), err
			}
			must := func(opts RootOptions, args ...string) string {
				t.Helper()
				out, diag, err := run(opts, nil, args...)
				if err != nil {
					t.Fatalf("%v: %v %s %s", args, err, out, diag)
				}
				return out
			}
			// Approval uses the maintainer checkout, before worker mutations.
			approveOpts := RootOptions{RepoRoot: upstreamRoot, Backend: "github", trackerOverride: maintTracker, nowFn: func() time.Time { return now }, lookupEnv: env.lookup()}
			approve := newRootCmdWithOptions(&approveOpts)
			approve.SetOut(io.Discard)
			approve.SetErr(io.Discard)
			approve.SetArgs([]string{"issue", "approve", "42"})
			if err := approve.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}
			flags := []string{}
			if forkMode {
				flags = append(flags, "--fork")
			}
			worker := func(args ...string) []string { return append(append([]string{}, flags...), args...) }
			must(RootOptions{Backend: "github"}, worker("issue", "claim", "42", "--agent", "mock")...)
			store := trustedLedgerStore(t, upstreamRoot, "origin")
			read := func() *lease.IssueState {
				t.Helper()
				raw, _, err := store.Load(t.Context(), 42)
				if err != nil {
					t.Fatal(err)
				}
				var st lease.IssueState
				if err := json.Unmarshal(raw, &st); err != nil {
					t.Fatal(err)
				}
				return &st
			}
			mu.Lock()
			now = read().Lease.ExpiresAt.Add(-time.Minute)
			mu.Unlock()
			wt := filepath.Join(root, ".swarm", "worktrees", "issue-42")
			for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
				if err := os.WriteFile(filepath.Join(wt, ".swarm", "active", name), []byte("# Plan\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Each run observes the authoritative ledger revision and operation counts
			// at output completion. Advice and heartbeat Stop must add no writes.
			check := func(opts RootOptions, expectedRenew string, args ...string) {
				t.Helper()
				// Equivalent repository/API snapshots isolate ordinary command
				// writes from the final-read failure. Setup writes are excluded.
				saved := filepath.Join(t.TempDir(), "snapshot")
				copyFixtureTree(t, saved, tmp)
				savedComments, err := json.Marshal(comments)
				if err != nil {
					t.Fatal(err)
				}
				savedIssueComments, err := json.Marshal(issueComments)
				if err != nil {
					t.Fatal(err)
				}
				savedID := nextCommentID
				savedCounts := map[string]int{}
				for k, v := range operationCounts {
					savedCounts[k] = v
				}
				var baselineCounts map[string]int
				var baselineWrites int
				var baseline *lease.IssueState
				for _, fault := range []bool{false, true} {
					if fault {
						if err := os.RemoveAll(tmp); err != nil {
							t.Fatal(err)
						}
						copyFixtureTree(t, tmp, saved)
						if err := json.Unmarshal(savedComments, &comments); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(savedIssueComments, &issueComments); err != nil {
							t.Fatal(err)
						}
						nextCommentID = savedID
						operationCounts = map[string]int{}
						for k, v := range savedCounts {
							operationCounts[k] = v
						}
					}
					finalReadFault = fault
					before := read()
					ref := store.Layout().StateRef(42)
					countBefore, err := strconv.Atoi(git(originBare, "rev-list", "--count", ref))
					if err != nil {
						t.Fatal(err)
					}
					var outputState *lease.IssueState
					var outputRev string
					var outputCounts map[string]int
					out, diag, err := run(opts, func() {
						outputState = read()
						_, outputRev, _ = store.Load(t.Context(), 42)
						outputCounts = map[string]int{}
						for k, v := range operationCounts {
							outputCounts[k] = v
						}
					}, worker(args...)...)
					if err != nil && !errors.Is(err, validation.ErrRequiredCheckFailed) {
						t.Fatalf("%v: %v %s %s", args, err, out, diag)
					}
					if slices.Contains(args, "--json") && (err != nil) != strings.Contains(out, `"status":"failed"`) {
						t.Fatal("validation status changed", err, out)
					}
					if slices.Contains(args, "--json") {
						issue322JSON(t, out)
					} else if out == "" {
						t.Fatal("lost text outcome")
					}
					want := "Claim time remaining for issue #42: 1m0s\nRenew: " + expectedRenew + "\n"
					if fault {
						if strings.Contains(diag, "Claim time remaining") || strings.Contains(diag, "Renew:") || strings.Contains(diag, "context canceled") {
							t.Fatal("partial/failed reminder leaked", diag)
						}
					} else {
						if strings.Count(diag, want) != 1 {
							t.Fatalf("missing exact command advisory %q in %q", want, diag)
						}
						// Parse the invocation extracted from actual command stderr.
						actual := strings.SplitN(strings.SplitN(diag, "Renew: ", 2)[1], "\n", 2)[0]
						issue322ParseEmittedRenew(t, actual, "42")
					}
					if strings.Contains(diag, before.Lease.ClaimID) || strings.Contains(diag, "test-token") {
						t.Fatal("credential leak", diag)
					}
					_, rev, err := store.Load(t.Context(), 42)
					if err != nil {
						t.Fatal(err)
					}
					after := read()
					if outputRev != rev || !reflect.DeepEqual(outputState, after) || !reflect.DeepEqual(outputCounts, operationCounts) {
						t.Fatal("advisory added authoritative/coordinator writes")
					}
					expectedLease := before.Lease
					expectedLease.Phase = after.Lease.Phase
					expectedLease.NextPhase = after.Lease.NextPhase
					if !reflect.DeepEqual(expectedLease, after.Lease) {
						t.Fatal("lease mutation")
					}
					if operationCounts["renew"] != 0 {
						t.Fatal("automatic renewal")
					}
					counts := map[string]int{}
					for k, v := range operationCounts {
						if delta := v - savedCounts[k]; delta != 0 {
							counts[k] = delta
						}
					}
					countAfter, err := strconv.Atoi(git(originBare, "rev-list", "--count", ref))
					if err != nil {
						t.Fatal(err)
					}
					if !fault {
						baselineCounts, baselineWrites, baseline = counts, countAfter-countBefore, after
					} else {
						if !reflect.DeepEqual(counts, baselineCounts) || countAfter-countBefore != baselineWrites {
							t.Fatal("final read altered command writes", counts, baselineCounts)
						}
						issue322SameAuthority(t, baseline, after)
					}
					t.Logf("fork=%t args=%v final-read-fault=%t command writes=%d coordinator operations=%v; reminder writes=0", forkMode, args, fault, countAfter-countBefore, counts)
				}
				finalReadFault = false
			}

			checkBoth := func(opts RootOptions, expectedRenew string, args ...string) {
				t.Helper()
				check(opts, expectedRenew, args...)
				var textArgs []string
				for _, arg := range args {
					if arg != "--json" {
						textArgs = append(textArgs, arg)
					}
				}
				check(opts, expectedRenew, textArgs...)
			}

			renew := "swarm --backend 'github' --root '" + root + "' --upstream-remote 'origin'"
			if forkMode {
				renew += " --fork --fork-remote 'fork'"
			}
			renew += " issue renew 42"
			checkBoth(RootOptions{}, renew, "issue", "stage", "42", "--phase", "plan", "--json")
			// The authenticated maintainer approves the exact staged plan bindings.
			reviewOpts := approveOpts
			review := newRootCmdWithOptions(&reviewOpts)
			review.SetOut(io.Discard)
			review.SetErr(io.Discard)
			review.SetArgs([]string{"issue", "review-plan", "42", "--approve", "--notes", "Reviewed exact staged plan"})
			if err := review.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}
			explicitRenew := "swarm --backend 'github' --repo 'octo/swarm' --root '" + root + "' --upstream-remote 'project'"
			if forkMode {
				explicitRenew += " --fork --fork-remote 'personal'"
			}
			explicitRenew += " issue renew 42"
			explicit := RootOptions{Backend: "github", Repo: "octo/swarm", UpstreamRemote: "project"}
			if forkMode {
				explicit.ForkRemote = "personal"
			}
			if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git(wt, "add", "feature.txt")
			git(wt, "commit", "-m", "feature")
			checkBoth(explicit, explicitRenew, "issue", "stage", "42", "--phase", "implement", "--json")
			checkBoth(RootOptions{Backend: "github"}, renew, "issue", "validate", "42", "--json", "--progress=false")
			failPath := filepath.Join(wt, "issue322-fail")
			if err := os.WriteFile(failPath, []byte("fail"), 0600); err != nil {
				t.Fatal(err)
			}
			checkBoth(RootOptions{Backend: "github"}, renew, "issue", "validate", "42", "--json", "--progress=false")
			if err := os.Remove(failPath); err != nil {
				t.Fatal(err)
			}

			if forkMode {
				for _, failCheck := range []bool{false, true} {
					path := filepath.Join(wt, "issue322-fail")
					if failCheck {
						if err := os.WriteFile(path, []byte("fail"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					for _, sentinel := range []bool{false, true} {
						for _, lookupFault := range []bool{false, true} {
							finalReadFault = lookupFault
							recordingFence = !sentinel
							injectRecordingSentinel = sentinel
							before := read()
							out, diag, err := run(RootOptions{Backend: "github"}, func() {}, worker("issue", "validate", "42", "--json", "--progress=false")...)
							wantErr := error(lease.ErrClaimFence)
							if sentinel {
								wantErr = recordingSentinel
							}
							if !errors.Is(err, wantErr) {
								t.Fatalf("recording identity: %v", err)
							}
							if errors.Is(err, validation.ErrRequiredCheckFailed) != failCheck {
								t.Fatalf("joined validation identity: %v", err)
							}
							issue322JSON(t, out)
							if lookupFault {
								if strings.Contains(diag, "Claim time remaining") || strings.Contains(diag, "Renew:") || strings.Contains(diag, "context canceled") {
									t.Fatal("lookup error leaked", diag)
								}
							} else if !strings.Contains(diag, "Renew: "+renew) {
								t.Fatal("missing recording failure advisory", diag)
							}
							if !strings.Contains(out, `"status":"failed"`) || strings.Contains(diag, recordingSentinel.Error()) {
								t.Fatal(out, diag)
							}
							if !reflect.DeepEqual(before, read()) {
								t.Fatal("rejected recording changed authority")
							}
							mu.Lock()
							pollRecordingFault = false
							mu.Unlock()
						}
					}
					finalReadFault = false
					if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
				}
				recordingFence = false
				injectRecordingSentinel = false
			}
			must(RootOptions{Backend: "github"}, worker("issue", "submit", "42", "--progress=false", "--json")...)
			prepared := must(RootOptions{Backend: "github"}, worker("pr", "work", "1", "--phase", "plan", "--agent", "mock", "--progress=false", "--json")...)
			prWork := readPROutcome(t, prepared)
			for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
				if err := os.WriteFile(filepath.Join(prWork.Worktree, ".swarm", "active", name), []byte("# PR revision plan\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			mu.Lock()
			now = read().Lease.ExpiresAt.Add(-time.Minute)
			mu.Unlock()
			// Nonfork PR accepts --fork-remote but must omit it from renew advice.
			prOpts := RootOptions{Backend: "github"}
			if !forkMode {
				prOpts.ForkRemote = "personal"
			}
			checkBoth(prOpts, renew, "pr", "stage", "1", "--phase", "plan", "--json")
			reviewOpts = approveOpts
			review = newRootCmdWithOptions(&reviewOpts)
			review.SetOut(io.Discard)
			review.SetErr(io.Discard)
			review.SetArgs([]string{"issue", "review-plan", "42", "--approve", "--notes", "Reviewed PR plan"})
			if err := review.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}
			checkBoth(explicit, explicitRenew, "pr", "stage", "1", "--phase", "implement", "--json")
			profilePath := filepath.Join(t.TempDir(), "node.yaml")
			if err := os.WriteFile(profilePath, []byte("node_id: issue322-node\nupstream_remote: project\n"), 0600); err != nil {
				t.Fatal(err)
			}
			env.set("SWARM_NODE_CONFIG", profilePath)
			env.set("SWARM_BACKEND", "github")
			profileRenew := "swarm --backend 'github' --root '" + root + "' --upstream-remote 'project'"
			if forkMode {
				originIsFork = true
				git(root, "remote", "set-url", "origin", aliceForkBare)
				profileRenew += " --fork --fork-remote 'origin'"
			}
			profileRenew += " issue renew 42"
			checkBoth(RootOptions{}, profileRenew, "pr", "stage", "1", "--phase", "implement", "--json")

		})
	}
}

func issue322ParseEmittedRenew(t *testing.T, command, issue string) {
	t.Helper()
	raw, err := exec.Command("bash", "-c", "set -- "+command+"; printf '%s\\0' \"$@\"").Output()
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	root := newRootCmdWithOptions(&RootOptions{})
	cmd, rest, err := root.Find(args[1:])
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name() != "renew" || cmd.Parent().Name() != "issue" {
		t.Fatal(command)
	}
	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatal(err)
	}
	if got := cmd.Flags().Args(); len(got) != 1 || got[0] != issue {
		t.Fatal(got)
	}
}

// Artifact report bodies contain check durations; compare authority and artifact
// identities separately while the output-boundary comparison checks the full state.
func issue322SameAuthority(t *testing.T, want, got *lease.IssueState) {
	t.Helper()
	left, right := *want, *got
	left.Artifacts, right.Artifacts = nil, nil
	left.Attestations = append([]contract.RoleAttestation(nil), want.Attestations...)
	right.Attestations = append([]contract.RoleAttestation(nil), got.Attestations...)
	for i := range left.Attestations {
		left.Attestations[i].ArtifactDigest = ""
	}
	for i := range right.Attestations {
		right.Attestations[i].ArtifactDigest = ""
	}
	left.RoleRuns, right.RoleRuns = nil, nil
	if !reflect.DeepEqual(lease.ReviewBindings(want), lease.ReviewBindings(got)) {
		t.Fatal("review bindings changed")
	}
	if len(want.RoleRuns) != len(got.RoleRuns) {
		t.Fatal("role run count changed")
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatal("baseline authority changed")
	}
	if len(want.Artifacts) != len(got.Artifacts) {
		t.Fatal("artifact count changed")
	}
	for i, a := range want.Artifacts {
		b := got.Artifacts[i]
		if a.ID != b.ID || a.Type != b.Type || a.Version != b.Version || a.Verdict != b.Verdict || a.Producer != b.Producer {
			t.Fatal("artifact identity changed")
		}
	}
}
