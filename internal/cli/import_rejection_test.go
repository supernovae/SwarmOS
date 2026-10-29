package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestCLIStatusImportRejection(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	state := f.state(t, issue)
	state.NextPhaseReason = "rejected agent changes to: .swarm/active/PLAN.md, .swarm/active/TEST_PLAN.md; returned changes were not imported; worktree was left unchanged; rerun with swarm issue work " + n + " --phase implement"
	state.Lease.NextPhase = "implement"
	state.Lease.Status = "handoff"
	_, rev, err := f.manager.Store().Load(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Store().CompareAndSwap(t.Context(), issue, rev, data); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"issue", "status", n}, {"issue", "status", n, "--json"}} {
		output := f.must(t, args...)
		for _, text := range []string{"PLAN.md", "TEST_PLAN.md", "worktree was left unchanged", "swarm issue work " + n + " --phase implement"} {
			if !strings.Contains(output, text) {
				t.Fatalf("status missing %q: %s", text, output)
			}
		}
	}
}

// The command/flag cases match the actual emitted guidance assertions in
// TestImportRejectionReasonWithPatch, TestDeveloperOutputImportRecoveryGuidance,
// and the completed-role guidance tests in internal/workflow.
func TestCLIImportRecoveryCommands(t *testing.T) {
	for _, route := range []struct{ group, number string }{{"issue", "7"}, {"pr", "42"}} {
		t.Run(route.group, func(t *testing.T) {
			for _, flag := range []string{"instructions", "developer-instructions", "tester-instructions"} {
				root := NewRootCmd()
				cmd, remaining, err := root.Find([]string{route.group, "work", route.number})
				if err != nil || cmd.CommandPath() != "swarm "+route.group+" work" || !cmd.Runnable() || cmd.HasSubCommands() || len(remaining) != 1 || remaining[0] != route.number {
					t.Fatalf("work lookup: %v %v %v", cmd, remaining, err)
				}
				for _, name := range []string{"phase", flag} {
					if cmd.Flags().Lookup(name) == nil {
						t.Fatalf("unregistered flag %s", name)
					}
				}
				if err := cmd.ParseFlags([]string{route.number, "--phase", "implement", "--" + flag, "Reviewed guidance"}); err != nil {
					t.Fatal(err)
				}
				if args := cmd.Flags().Args(); len(args) != 1 || args[0] != route.number {
					t.Fatalf("arguments: %v", args)
				}
			}
			cmd, _, err := NewRootCmd().Find([]string{route.group, "recover-import", route.number, "rejected.patch"})
			if err == nil && cmd.CommandPath() != "swarm "+route.group {
				t.Fatalf("unexpected registered recovery leaf: %s", cmd.CommandPath())
			}
		})
	}
}
