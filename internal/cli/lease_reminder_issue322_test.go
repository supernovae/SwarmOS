package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"os/exec"
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
