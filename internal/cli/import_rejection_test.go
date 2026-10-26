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
