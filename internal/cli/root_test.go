package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/workspace"
)

func TestResolveIssueAndWorktreeNeverGuesses(t *testing.T) {
	root := t.TempDir()
	if _, _, err := resolveIssueAndWorktree(root, []string{"9"}); err == nil || !strings.Contains(err.Error(), "no worktree for issue #9") {
		t.Fatalf("explicit issue without worktree = %v", err)
	}
	for _, n := range []int{10, 9} {
		if err := os.MkdirAll(workspace.WorktreePath(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := resolveIssueAndWorktree(root, nil); err == nil || !strings.Contains(err.Error(), "#9, #10") {
		t.Fatalf("ambiguous worktrees = %v", err)
	}
	num, wt, err := resolveIssueAndWorktree(root, []string{"#9"})
	if err != nil || num != 9 || wt != workspace.WorktreePath(root, 9) {
		t.Fatalf("explicit issue = %d %q %v", num, wt, err)
	}
	if err := os.RemoveAll(workspace.WorktreePath(root, 10)); err != nil {
		t.Fatal(err)
	}
	if num, wt, err = resolveIssueAndWorktree(root, nil); err != nil || num != 9 || wt != workspace.WorktreePath(root, 9) {
		t.Fatalf("single worktree = %d %q %v", num, wt, err)
	}

	active := t.TempDir()
	if err := os.MkdirAll(filepath.Join(active, ".swarm", "active"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, ".swarm", "active", "workunit.json"), []byte(`{"issue_number": 7}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if num, wt, err = resolveIssueAndWorktree(active, []string{"7"}); err != nil || num != 7 || wt != active {
		t.Fatalf("inside active worktree = %d %q %v", num, wt, err)
	}
	if _, _, err = resolveIssueAndWorktree(active, []string{"8"}); err == nil {
		t.Fatal("active worktree for #7 accepted as #8")
	}
}

func TestJSONCommandsReportFailuresAsJSON(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	issue := strconv.Itoa(f.create(t, "agent"))
	for kind, args := range map[string][]string{
		"claim":         {"issue", "claim", "999", "--json"},
		"status":        {"issue", "status", "999", "--json"},
		"renew":         {"issue", "renew", issue, "--json"},
		"release":       {"issue", "release", issue, "--json"},
		"task-approval": {"issue", "approve", "999", "--json"},
		"submission":    {"issue", "submit", issue, "--json"},
		"validation":    {"issue", "validate", issue, "--json"},
	} {
		out, err := f.run(args...)
		if err == nil {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
		var outcome map[string]any
		if decodeErr := json.NewDecoder(strings.NewReader(out)).Decode(&outcome); decodeErr != nil {
			t.Fatalf("%v failure was not JSON: %v\n%s", args, decodeErr, out)
		}
		if outcome["kind"] != kind || outcome["status"] != "failed" || outcome["error"] != err.Error() {
			t.Fatalf("%v failure document = %v", args, outcome)
		}
	}
}

func TestUtilityCommandsEmitJSON(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	for kind, args := range map[string][]string{
		"version":     {"version", "--json"},
		"repo-list":   {"repo", "list", "--json"},
		"node-init":   {"node", "init", "--id", "test-node", "--json"},
		"node-status": {"node", "status", "--json"},
		"repo-init":   {"repo", "init", "--json"},
		"repo-add":    {"repo", "add", "octo/swarm", f.root, "--json"},
	} {
		out := f.must(t, args...)
		var outcome map[string]any
		if err := json.Unmarshal([]byte(out), &outcome); err != nil {
			t.Fatalf("%v did not emit one JSON document: %v\n%s", args, err, out)
		}
		if outcome["kind"] != kind {
			t.Fatalf("%v kind = %v", args, outcome["kind"])
		}
	}
	out, err := f.run("repo", "doctor", "--json")
	var doctor map[string]any
	if decodeErr := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &doctor); decodeErr != nil || doctor["kind"] != "doctor" {
		t.Fatalf("doctor JSON = %v (%v)\n%s", doctor, decodeErr, out)
	}
	if (err != nil) != (doctor["status"] == "failed") {
		t.Fatalf("doctor status %v disagrees with exit error %v", doctor["status"], err)
	}
}

func TestFailedResumeKeepsClaimForStagingRecovery(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	issue := strconv.Itoa(n)
	f.must(t, "issue", "approve", issue)
	f.must(t, "issue", "claim", issue, "--agent", "mock")
	claim := f.state(t, n).Lease.ClaimID
	worktree := workspace.WorktreePath(f.root, n)
	if err := os.WriteFile(filepath.Join(worktree, "agent-output.txt"), []byte("work from a failed checkpoint\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := f.run("issue", "work", issue, "--agent", "mock", "--progress=false")
	if err == nil || !strings.Contains(out+err.Error(), "swarm issue stage <issue> --phase implement") {
		t.Fatalf("dirty resume = %v\n%s", err, out)
	}
	if state := f.state(t, n); state.Lease.Status != "active" || state.Lease.ClaimID != claim {
		t.Fatalf("failed resume released the existing claim: %+v", state.Lease)
	}
}
