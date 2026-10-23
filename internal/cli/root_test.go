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
	f := newCLIFixture(t)
	issue := strconv.Itoa(f.create(t, "agent"))
	for kind, args := range map[string][]string{
		"claim":         {"issue", "claim", "999", "--json"},
		"status":        {"issue", "status", "999", "--json"},
		"renew":         {"issue", "renew", issue, "--json"},
		"release":       {"issue", "release", issue, "--json"},
		"task-approval": {"issue", "approve", "999", "--json"},
		"submission":    {"issue", "submit", issue, "--json"},
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
