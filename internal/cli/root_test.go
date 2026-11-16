package cli

import (
	"bytes"
	"encoding/json"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/storage"
	"github.com/supernovae/swarmos/internal/storageuse"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/workspace"
)

func TestResolveIssueAndWorktreeNeverGuesses(t *testing.T) {
	t.Parallel()
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

func TestInternalNativeCommandRegistration(t *testing.T) {
	t.Parallel()
	root := newRootCmdWithOptions(&RootOptions{Repo: "invalid-repo", RepoRoot: filepath.Join(t.TempDir(), "absent")})
	cmd, args, err := root.Find([]string{"internal-native-exec", "invalid-unit"})
	if err != nil || cmd.Name() != "internal-native-exec" || !cmd.Hidden || !cmd.DisableFlagParsing {
		t.Fatalf("native helper registration: %v %v", cmd, err)
	}
	if err = cmd.Args(cmd, args); err != nil {
		t.Fatal(err)
	}
	if err = root.PersistentPreRunE(cmd, args); err != nil {
		t.Fatalf("helper ran coordinator startup: %v", err)
	}
	for _, args := range [][]string{nil, {"unit", "unexpected"}} {
		if err = cmd.Args(cmd, args); err == nil {
			t.Fatalf("helper accepted args %q", args)
		}
	}
	// Public commands must still validate coordinator options.
	public, _, err := root.Find([]string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	if err = root.PersistentPreRunE(public, nil); err == nil {
		t.Fatal("native exemption leaked to public command")
	}
}

func decodeJSONDocument(t *testing.T, raw string) map[string]any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(raw))
	var v map[string]any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode: %v: %s", err, raw)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		t.Fatalf("extra output: %v: %s", err, raw)
	}
	return v
}
func TestRunnableLeavesHaveJSON(t *testing.T) {
	root := NewRootCmd()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			return
		}
		if c.Runnable() && len(c.Commands()) == 0 {
			flag := c.Flags().Lookup("json")
			if flag == nil {
				flag = c.InheritedFlags().Lookup("json")
			}
			if flag == nil || flag.Value.Type() != "bool" {
				t.Errorf("%s lacks boolean --json", c.CommandPath())
			}
		}
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	walk(root)
}
func TestJSONEarlyCommandFailures(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, kind string
		args       []string
	}{
		{"plan", "project-plan", []string{"repo", "plan"}},
		{"missing", "storage-report", []string{"storage", "report", "--root", filepath.Join(base, "missing")}},
		{"file", "storage-report", []string{"storage", "report", "--root", file}},
		{"pr", "pr-status", []string{"pr", "status", "1", "--root", base}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewRootCmd()
			if tc.kind == "storage-cleanup" {
				f := newCLIFixture(t)
				bulkMerged(t, f)
				t.Setenv("TMPDIR", t.TempDir())
				c = newRootCmdWithOptions(f.options(&RootOptions{RepoRoot: f.root}))
				tc.args = []string{"storage", "cleanup"}
			}
			var out, stderr bytes.Buffer
			c.SetOut(&out)
			c.SetErr(&stderr)
			c.SetArgs(append(tc.args, "--json"))
			err := c.Execute()
			if err == nil || ExitCode(err) == 0 {
				t.Errorf("expected nonzero error, got %v", err)
			}
			v := decodeJSONDocument(t, out.String())
			if v["kind"] != tc.kind || v["status"] != "failed" || v["error"] == "" || v["error"] == nil {
				t.Fatalf("failure: %v", v)
			}
			if err != nil && v["error"] != err.Error() {
				t.Fatalf("error changed: %v %v", v, err)
			}
		})
	}
	for _, root := range []string{filepath.Join(base, "missing"), file, base} {
		c := NewRootCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetArgs([]string{"storage", "report", "--root", root})
		if err := c.Execute(); err != nil || !strings.Contains(out.String(), "Storage report\nScope:") {
			t.Fatalf("text changed: %v %s", err, out.String())
		}
	}
}

func assertSnakeKeys(t *testing.T, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if !regexp.MustCompile("^[a-z][a-z0-9]*(_[a-z0-9]+)*$").MatchString(k) {
				t.Errorf("non snake_case key: %s", k)
			}
			assertSnakeKeys(t, v)
		}
	case []any:
		for _, v := range x {
			assertSnakeKeys(t, v)
		}
	}
}
func TestJSONNamedCommandDocuments(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)
	for _, tc := range []struct {
		kind string
		args []string
	}{
		{"fork-request-status", nil},
		{"onboard", []string{"repo", "onboard"}},
		{"storage-report", []string{"storage", "report", "--root", base}},
		{"storage-cleanup", []string{"storage", "cleanup", "--root", base}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if tc.kind == "fork-request-status" {
				assertForkRequestStatusStateOnly(t)
				return
			}
			c := NewRootCmd()
			if tc.kind == "storage-cleanup" {
				f := newCLIFixture(t)
				bulkMerged(t, f)
				t.Setenv("TMPDIR", t.TempDir())
				c = newRootCmdWithOptions(f.options(&RootOptions{RepoRoot: f.root}))
				tc.args = []string{"storage", "cleanup"}
			}
			var out, stderr bytes.Buffer
			c.SetOut(&out)
			c.SetErr(&stderr)
			c.SetArgs(append(tc.args, "--json"))
			if err := c.Execute(); err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
			v := decodeJSONDocument(t, out.String())
			if v["kind"] != tc.kind {
				t.Fatal(v)
			}
			assertSnakeKeys(t, v)
			if tc.kind == "storage-cleanup" && v["version"] != float64(2) {
				t.Fatal(v)
			}
		})
	}
	n := int64(42)
	report := storageCleanupReport{Kind: "storage-cleanup", Version: 2, Scopes: []storage.SelectedRepository{{}}, Host: storage.ReclamationReport{
		DiscoveryReasons: []string{"fixture"}, Resources: []storage.ReclamationResult{{Resource: storageuse.Resource{Repository: "repo", Identity: "id", Location: "path"}, Before: storage.UsageObservation{LogicalBytes: &n, AllocatedBytes: &n}, ObservedLogicalDelta: &n}},
	}}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	v := decodeJSONDocument(t, string(raw))
	assertSnakeKeys(t, v)
	host := v["host"].(map[string]any)
	resource := host["resources"].([]any)[0].(map[string]any)["resource"].(map[string]any)
	if resource["repository"] != "repo" || resource["identity"] != "id" {
		t.Fatal(resource)
	}
}
func TestJSONClaimShapes(t *testing.T) {
	var previous map[string]bool
	for _, next := range []bool{false, true} {
		f := newCLIFixture(t)
		number := f.create(t, "agent")
		n := strconv.Itoa(number)
		f.must(t, "issue", "approve", n)
		args := []string{"issue", "claim", n, "--agent", "mock", "--json"}
		if next {
			f.must(t, "repo", "add", "octo/repo", f.root)
			args = []string{"issue", "claim", "--next", "--all", "--agent", "mock", "--json"}
		}
		v := decodeJSONDocument(t, f.must(t, args...))
		keys := map[string]bool{}
		for k := range v {
			keys[k] = true
		}
		if previous != nil && !reflect.DeepEqual(keys, previous) {
			t.Fatalf("keys differ: %v %v", keys, previous)
		}
		previous = keys
		if v["kind"] != "claim" || v["status"] != "claimed" || v["issue_number"] != float64(number) || v["branch"] == "" || v["worktree"] == "" || v["state"] == nil {
			t.Fatal(v)
		}
		if next {
			sel := v["selection"].(map[string]any)
			if sel["BranchName"] != v["branch"] || sel["WorktreePath"] != v["worktree"] {
				t.Fatal(v)
			}
		} else if v["selection"] != nil {
			t.Fatal(v)
		}
	}
}
