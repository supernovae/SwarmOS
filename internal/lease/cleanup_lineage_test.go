package lease

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/workspace"
)

func TestCleanupRevisedLegacyLineage(t *testing.T) {
	m, tr, n, _ := mergedCleanupFixture(t)
	ctx := context.Background()
	git := func(args ...string) string {
		t.Helper()
		b, e := exec.Command("git", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("git: %s %v", b, e)
		}
		return strings.TrimSpace(string(b))
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	old := st.HeadSHA
	path := workspace.WorktreePath(m.repoRoot, n)
	git("worktree", "add", "-b", st.Branch, path, old)
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "revised")
	final := git("rev-parse", "HEAD")
	st.HeadSHA, st.SubmittedHeadSHA = final, final
	st.CheckpointHeads = nil // Legacy Git state obtains lineage from pinned history.
	raw, _ := json.Marshal(st)
	if _, err = m.store.CompareAndSwap(ctx, n, rev, raw); err != nil {
		t.Fatal(err)
	}
	tr.pr.HeadSHA = final
	git("push", "origin", final+":refs/heads/"+st.Branch)
	p, err := InspectCleanup(ctx, m.repoRoot, m.tracker, m, n)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Resources {
		if r.Name == path && (!r.Eligible || r.ExpectedHead != old) {
			t.Fatalf("legacy ancestor: %+v", r)
		}
	}
	reservation, err := m.BeginCleanup(ctx, n, p.Outcome)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.RetireCleanupLocalResources(ctx, n, reservation.OperationID, p); err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Resources {
		if r.Name == path && !r.Retired {
			t.Fatalf("legacy retirement: %+v", r)
		}
	}
}

func TestCleanupHeadLineage(t *testing.T) {
	m, _, n, _ := mergedCleanupFixture(t)
	ctx := context.Background()
	st, rev, err := m.load(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	head := st.HeadSHA
	if why := m.cleanupHeadReason(ctx, n, st, rev, head, head); why != "" {
		t.Fatal(why)
	}

	git := func(args ...string) string {
		t.Helper()
		b, e := exec.Command("git", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("git: %s %v", b, e)
		}
		return strings.TrimSpace(string(b))
	}
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "unrecorded child")
	child := git("rev-parse", "HEAD")
	git("update-ref", "refs/heads/swarm-checkpoints/issue-1/"+child, child)
	if why := m.cleanupHeadReason(ctx, n, st, rev, child, head); !strings.Contains(why, "lineage") {
		t.Fatal("retention ref granted authority: " + why)
	}
	st.CheckpointHeads = []string{child}
	if why := m.cleanupHeadReason(ctx, n, st, rev, child, head); !strings.Contains(why, "not an ancestor") {
		t.Fatal(why)
	}
	missing := strings.Repeat("a", 40)
	if why := m.cleanupHeadReason(ctx, n, st, rev, missing, head); !strings.Contains(why, "lineage") {
		t.Fatal(why)
	}
	st.CheckpointHeads = []string{missing}
	if why := m.cleanupHeadReason(ctx, n, st, rev, missing, head); !strings.Contains(why, "unavailable") {
		t.Fatal(why)
	}
	if why := m.cleanupHeadReason(ctx, n, st, strings.Repeat("0", 40), missing, head); !strings.Contains(why, "history unavailable") {
		t.Fatal(why)
	}
	// Missing historical evidence must not break the exact-tip case.
	if why := m.cleanupHeadReason(ctx, n, st, strings.Repeat("0", 40), head, head); why != "" {
		t.Fatal(why)
	}
	preserveCheckpointHeads(st, head, head)
	count := 0
	for _, h := range st.CheckpointHeads {
		if h == head {
			count++
		}
	}
	if count != 1 {
		t.Fatal("checkpoint list is not deduplicated")
	}
}
