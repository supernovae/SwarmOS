package coordination

import (
	"strings"
	"testing"
)

func TestRefLayoutNamesAndCounterparts(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		namespace                      string
		layout                         RefLayout
		state, checkpoint, contributor string
	}{
		{"", BranchLayout, "refs/heads/swarm-state/issue-7", "refs/heads/swarm-checkpoints/issue-7/" + head, "refs/heads/swarm-contributors/alice"},
		{"branch", BranchLayout, "refs/heads/swarm-state/issue-7", "refs/heads/swarm-checkpoints/issue-7/" + head, "refs/heads/swarm-contributors/alice"},
		{"hidden", HiddenLayout, "refs/swarm/state/issue-7", "refs/swarm/checkpoints/issue-7/" + head, "refs/swarm/contributors/alice"},
	} {
		layout, err := LayoutFor(tc.namespace)
		if err != nil || layout != tc.layout {
			t.Fatalf("LayoutFor(%q) = %v, %v", tc.namespace, layout, err)
		}
		got := []string{layout.StateRef(7), layout.CheckpointRef(7, head), layout.ContributorRef("alice")}
		want := []string{tc.state, tc.checkpoint, tc.contributor}
		other := layout.Counterpart()
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s ref %d = %s, want %s", layout.Name(), i, got[i], want[i])
			}
			if !IsLedgerRef(got[i]) {
				t.Errorf("%s not recognized as a ledger ref", got[i])
			}
			counterpart, ok := layout.counterpartRef(got[i])
			if !ok || !strings.HasPrefix(counterpart, other.prefixes()[i]) || strings.TrimPrefix(counterpart, other.prefixes()[i]) != strings.TrimPrefix(got[i], layout.prefixes()[i]) {
				t.Errorf("counterpart of %s = %s, %v", got[i], counterpart, ok)
			}
		}
	}
	if _, err := LayoutFor("public"); err == nil {
		t.Fatal("unknown namespace accepted")
	}
	for _, ref := range []string{"refs/heads/swarm/issue-7-x", "refs/heads/main", "refs/swarm/state/", "refs/heads/swarm-state/", "refs/swarm/other/x"} {
		if IsLedgerRef(ref) {
			t.Errorf("%s treated as a ledger ref", ref)
		}
	}
}
