package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// An unexpected call to any other Tracker method panics through the nil
// interface, making the zero-call assertion unconditional.
type untouchedSubmitTracker struct {
	tracker.Tracker
	calls int
}

func (t *untouchedSubmitTracker) CurrentUser(context.Context) (string, error) {
	t.calls++
	return "", fmt.Errorf("unexpected tracker call")
}
func (t *untouchedSubmitTracker) BackendName() string { t.calls++; return "github" }
func (t *untouchedSubmitTracker) GetIssue(context.Context, int) (*tracker.Issue, error) {
	t.calls++
	return nil, fmt.Errorf("unexpected tracker call")
}
func TestEmptySubmitBeforeAnyTrackerCall(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"human", "json", "fork"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			n := f.create(t, "agent")
			number := strconv.Itoa(n)
			f.must(t, "issue", "approve", number)
			f.must(t, "issue", "claim", number, "--agent", "mock")
			state := f.state(t, n)
			wt := workspace.WorktreePath(f.root, n)
			// Planning injects the pointer and writes ignored active files without commits.
			f.must(t, "issue", "work", number, "--agent", "mock", "--phase", "plan", "--progress=false")
			checkpointWrite := func(name, text string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(wt, name), []byte(text), 0644); err != nil {
					t.Fatal(err)
				}
			}
			checkpointWrite(".swarm/active/IMPLEMENTATION.md", "Blocked before implementation")
			tr := &untouchedSubmitTracker{}
			opts := &RootOptions{RepoRoot: f.root, Backend: "github", Repo: "owner/repo", trackerOverride: tr}
			cmd := f.command(opts)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			args := []string{"issue", "submit", number}
			if mode == "json" {
				args = append(args, "--json")
			}
			if mode == "fork" {
				args = append([]string{"--fork"}, args...)
			}
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(t.Context())
			if err == nil || !strings.Contains(err.Error(), state.BaseSHA) || !strings.Contains(err.Error(), "--phase implement") {
				t.Fatalf("refusal: %v %s", err, out.String())
			}
			if tr.calls != 0 {
				t.Fatalf("submit made %d tracker calls", tr.calls)
			}
			if mode == "json" && !strings.Contains(out.String(), `"status":"failed"`) {
				t.Fatalf("JSON failure absent: %s", out.String())
			}
		})
	}
}
func TestEmptySubmitHintNeverAuthorizes(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	number := strconv.Itoa(n)
	f.must(t, "issue", "approve", number)
	// A failed no-op implementation leaves approved plans and an empty checkpoint.
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", ":")
	if _, err := f.run("issue", "work", number, "--agent", "mock", "--progress=false"); err == nil {
		t.Fatal("no-op passed")
	}
	state := f.state(t, n)
	wt := workspace.WorktreePath(f.root, n)
	for _, base := range []string{"", "bad", "0000000000000000000000000000000000000000"} {
		if err := lease.SaveClaimToken(f.root, n, state.Lease.ClaimID, base); err != nil {
			t.Fatal(err)
		}
		if err := refuseEmptySubmitEarly(t.Context(), f.root, wt, n); err != nil {
			t.Fatalf("invalid hint refused: %v", err)
		}
		out, err := f.run("issue", "submit", number)
		if err == nil || !strings.Contains(err.Error(), state.BaseSHA) || !strings.Contains(err.Error(), "--phase implement") {
			t.Fatalf("backstop: %v %s", err, out)
		}
		if current := f.state(t, n); current.PRNumber != 0 {
			t.Fatal("empty submission created a PR")
		}
	}
}
func TestSubmitCheckpointsRealUncommittedWork(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"tracked", "untracked", "commit"} {
		t.Run(kind, func(t *testing.T) {
			f := newCLIFixture(t)
			n := f.create(t, "agent")
			number := strconv.Itoa(n)
			f.must(t, "issue", "approve", number)
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", ":")
			if _, err := f.run("issue", "work", number, "--agent", "mock", "--progress=false"); err == nil {
				t.Fatal("no-op passed")
			}
			wt := workspace.WorktreePath(f.root, n)
			path := "real.txt"
			if kind == "tracked" {
				path = "README.md"
			}
			if err := os.WriteFile(filepath.Join(wt, path), []byte("real work\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if kind == "commit" {
				prGit(t, wt, "add", path)
				prGit(t, wt, "commit", "-m", "real work")
			}
			if err := refuseEmptySubmitEarly(t.Context(), f.root, wt, n); err != nil {
				t.Fatalf("real work refused: %v", err)
			}
			f.must(t, "issue", "submit", number)
			if state := f.state(t, n); state.PRNumber == 0 || state.HeadSHA == state.BaseSHA {
				t.Fatal("real work not submitted")
			}
		})
	}
}
func TestIssueInstructionsAndStatusEvidence(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	number := strconv.Itoa(n)
	f.must(t, "issue", "approve", number)
	f.must(t, "issue", "work", number, "--agent", "mock", "--instructions", "Use local fixtures", "--progress=false")
	state := f.state(t, n)
	attestationJSON, err := json.Marshal(state.Attestations)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(attestationJSON), "Use local fixtures") || strings.Contains(string(attestationJSON), "operator_steered") {
		t.Fatal("raw guidance entered attestations")
	}
	for _, a := range state.Artifacts {
		if a.Metadata["operator_steered"] != "true" || !strings.Contains(a.Body, "Use local fixtures") {
			t.Fatalf("missing role guidance: %+v", a)
		}
	}
	status := f.must(t, "issue", "status", number)
	if !strings.Contains(status, "Operator-steered") || !strings.Contains(status, "SHA-256") {
		t.Fatal(status)
	}
	f.must(t, "issue", "submit", number)
	state = f.state(t, n)
	pr, err := f.tracker.GetPullRequest(t.Context(), state.PRNumber)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pr.Body, "Operator-steered") || !strings.Contains(pr.Body, "Use local fixtures") {
		t.Fatal("PR lacks guidance")
	}
}

func TestPRNoopFailsAgainstRevisionStartingHead(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent")
	starting := f.state(t, issue).HeadSHA
	if starting == f.state(t, issue).BaseSHA {
		t.Fatal("fixture must begin ahead of base")
	}
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", ":")
	output, err := f.run("pr", "work", strconv.Itoa(number), "--agent", "mock", "--progress=false")
	if err == nil || !strings.Contains(err.Error(), "swarm pr work "+strconv.Itoa(number)+" --phase implement") {
		t.Fatalf("no-op revision: %v %s", err, output)
	}
	state := f.state(t, issue)
	for _, a := range state.Artifacts {
		if a.Type == "implementation" && (a.Verdict != "FAIL" || !strings.Contains(a.Summary, starting)) {
			t.Fatal(a)
		}
	}
	if state.PRRevision == nil || state.PRRevision.LocalHeadSHA != starting {
		t.Fatal("no-op changed checkpoint")
	}
}

func TestIssueBlockedRerunAheadOfBase(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	number := strconv.Itoa(n)
	f.must(t, "issue", "approve", number)
	f.must(t, "issue", "work", number, "--agent", "mock", "--progress=false")
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", ":")
	// An existing issue checkpoint may be tested again without new work.
	f.must(t, "issue", "work", number, "--agent", "mock", "--phase", "implement", "--progress=false")
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf '# Blocked before implementation\n\nMissing recovery fixture\n' > .swarm/active/IMPLEMENTATION.md")
	output, err := f.run("issue", "work", number, "--agent", "mock", "--phase", "implement", "--progress=false")
	if err == nil || !strings.Contains(err.Error(), "Missing recovery fixture") {
		t.Fatalf("blocked rerun: %v %s", err, output)
	}
	status := f.must(t, "issue", "status", number)
	if !strings.Contains(status, "implementation: FAIL") || !strings.Contains(status, "Missing recovery fixture") {
		t.Fatal(status)
	}
	jsonStatus := f.must(t, "issue", "status", number, "--json")
	if !strings.Contains(jsonStatus, "Missing recovery fixture") {
		t.Fatal(jsonStatus)
	}
}

func TestPRRejectedReviewReplansBeforeReview(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent")
	f.setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
	args := []string{"pr", "work", strconv.Itoa(number), "--agent", "mock", "--progress=false"}
	if output, err := f.run(args...); err == nil || !strings.Contains(err.Error(), "CHANGES_REQUESTED") {
		t.Fatalf("expected rejection: %v %s", err, output)
	}
	before := f.state(t, issue)
	f.setenv("SWARM_MOCK_VERDICT", "APPROVED")
	if output, err := f.run(args...); err == nil || !strings.Contains(err.Error(), "re-plan is unchanged") {
		t.Fatalf("rejected PR plan reviewed unchanged: %v %s", err, output)
	}
	after := f.state(t, issue)
	var oldVersion, newVersion int
	for _, a := range before.Artifacts {
		if a.Type == "review" {
			oldVersion = a.Version
		}
	}
	for _, a := range after.Artifacts {
		if a.Type == "review" {
			newVersion = a.Version
			if a.Verdict != "CHANGES_REQUESTED" {
				t.Fatal("unchanged plan got new verdict")
			}
		}
	}
	if oldVersion == 0 || newVersion != oldVersion {
		t.Fatal("unchanged plan invoked reviewer")
	}
}
