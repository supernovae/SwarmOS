package lease

import (
	"context"
	"encoding/json"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestCleanupRevisedLegacyLineage(t *testing.T) {
	m, tr, n, _ := mergedCleanupFixture(t)
	ctx := context.Background()
	git := func(args ...string) string {
		t.Helper()
		b, e := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
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
		b, e := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot}, args...)...).CombinedOutput()
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

func TestCleanupRebasedLegacyLineage(t *testing.T) {
	for _, identity := range []string{"unchanged", "changed"} {
		t.Run(identity, func(t *testing.T) {
			m, tr, n, remote := mergedCleanupFixture(t)
			ctx := context.Background()
			git := func(args ...string) string {
				t.Helper()
				b, e := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
				if e != nil {
					t.Fatalf("git: %s %v", b, e)
				}
				return strings.TrimSpace(string(b))
			}
			st, rev, err := m.load(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			tree := git("rev-parse", "HEAD^{tree}")
			old := git("commit-tree", tree, "-p", st.HeadSHA, "-m", "feature")
			base := git("commit-tree", tree, "-p", st.HeadSHA, "-m", "advanced base")
			final := git("commit-tree", tree, "-p", base, "-m", "feature rebased")
			path := workspace.WorktreePath(m.repoRoot, n)
			git("worktree", "add", "-b", st.Branch, path, old)
			st.HeadSHA = old
			st.LastRebase = &RebaseEvidence{OldHead: old, NewHead: final, OldBase: st.BaseSHA, NewBase: base, Status: "completed", PatchIdentity: identity, Verified: true}
			raw, _ := json.Marshal(st)
			if rev, err = m.store.CompareAndSwap(ctx, n, rev, raw); err != nil {
				t.Fatal(err)
			}
			// A later rebase replaced LastRebase; the transition survives only in
			// the pinned ledger history.
			st.HeadSHA, st.SubmittedHeadSHA = final, final
			st.CheckpointHeads, st.RebaseTransitions = nil, nil
			st.LastRebase = &RebaseEvidence{OldHead: final, OldBase: base, NewBase: base, Status: "in-progress", PatchIdentity: "unavailable"}
			raw, _ = json.Marshal(st)
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
					t.Fatalf("legacy rebase: %+v", r)
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
					t.Fatalf("legacy rebase retirement: %+v", r)
				}
			}
			if git("--git-dir="+remote, "rev-parse", "refs/heads/swarm-state/issue-1") == "" {
				t.Fatal("ledger retired")
			}
		})
	}
}

func TestCleanupRebasePaths(t *testing.T) {
	m, _, n, _ := mergedCleanupFixture(t)
	ctx := context.Background()
	git := func(args ...string) string {
		t.Helper()
		b, e := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("git: %s %v", b, e)
		}
		return strings.TrimSpace(string(b))
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	tree := git("rev-parse", "HEAD^{tree}")
	commit := func(parent, msg string) string { return git("commit-tree", tree, "-p", parent, "-m", msg) }
	root := st.HeadSHA
	old := commit(root, "old")
	o1 := commit(old, "old child")
	r1 := commit(commit(root, "base one"), "first rebase")
	r1c := commit(r1, "between rebases")
	r2 := commit(commit(root, "base two"), "second rebase")
	tip := commit(r2, "after rebase")
	other := commit(root, "unrelated")
	missing := strings.Repeat("d", 40)
	edge := func(from, to string) coordination.CheckpointRebase {
		return coordination.CheckpointRebase{OldHead: from, NewHead: to, Status: "completed", PatchIdentity: "unchanged", Verified: true}
	}
	unverified, unavailable, changed := edge(o1, r1), edge(o1, r1), edge(o1, r1)
	unverified.Verified = false
	unavailable.PatchIdentity = "unavailable"
	changed.PatchIdentity = "changed"
	changedTip := edge(old, tip)
	changedTip.PatchIdentity = "changed"
	for _, tc := range []struct {
		name    string
		head    string
		edges   []coordination.CheckpointRebase
		members []string
		fork    bool
		want    string
	}{
		{"chain with ancestry segments", o1, []coordination.CheckpointRebase{edge(r1c, r2), edge(o1, r1)}, nil, false, ""},
		{"changed chain", o1, []coordination.CheckpointRebase{edge(r1c, r2), changed}, nil, false, ""},
		{"direct to tip", old, []coordination.CheckpointRebase{edge(old, tip)}, nil, false, ""},
		{"changed direct to tip", old, []coordination.CheckpointRebase{changedTip}, nil, false, ""},
		{"old head outside checkpoint list", old, []coordination.CheckpointRebase{changedTip}, []string{}, false, ""},
		{"unrecorded head", o1, []coordination.CheckpointRebase{edge(old, tip)}, []string{}, false, "lineage"},
		{"ancestor of old head", old, []coordination.CheckpointRebase{edge(o1, tip)}, nil, false, "old head of a verified rebase"},
		{"incomplete chain", o1, []coordination.CheckpointRebase{edge(o1, r1)}, nil, false, "no verified rebase path"},
		{"reversed", old, []coordination.CheckpointRebase{edge(r1, o1), edge(r2, r1c)}, nil, false, "old head of a verified rebase"},
		{"disconnected", old, []coordination.CheckpointRebase{edge(other, r2)}, nil, false, "old head of a verified rebase"},
		{"cycle", old, []coordination.CheckpointRebase{edge(old, other), edge(other, old)}, nil, false, "no verified rebase path"},
		{"missing object", old, []coordination.CheckpointRebase{edge(old, missing), edge(missing, tip)}, nil, false, "no verified rebase path"},
		{"unverified link", o1, []coordination.CheckpointRebase{unverified, edge(r1c, r2)}, nil, false, "old head of a verified rebase"},
		{"only unverified", o1, []coordination.CheckpointRebase{unverified, unavailable}, nil, false, "no rebase has verified evidence"},
		{"fork", old, []coordination.CheckpointRebase{edge(old, tip)}, nil, true, "not an ancestor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := *st
			cp.CheckpointHeads = []string{tc.head}
			if tc.members != nil {
				cp.CheckpointHeads = tc.members
			}
			cp.RebaseTransitions = tc.edges
			if tc.fork {
				cp.ForkRepo = "contributor/swarm"
			}
			why := m.cleanupHeadReason(ctx, n, &cp, rev, tc.head, tip)
			if (tc.want == "") != (why == "") || !strings.Contains(why, tc.want) {
				t.Fatalf("reason %q, want %q", why, tc.want)
			}
		})
	}
	// Patch-equivalent heads need recorded evidence, not just shared content.
	cp := *st
	cp.CheckpointHeads = []string{other}
	if why := m.cleanupHeadReason(ctx, n, &cp, rev, other, tip); !strings.Contains(why, "not an ancestor") {
		t.Fatal(why)
	}
}

func TestRecordRebaseTransition(t *testing.T) {
	old, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	evidence := RebaseEvidence{OldHead: old, NewHead: head, Status: "completed", PatchIdentity: "unchanged", Verified: true}
	st := &IssueState{}
	for _, mutate := range []func(*RebaseEvidence){
		func(r *RebaseEvidence) { r.NewHead = strings.Repeat("c", 40) },
		func(r *RebaseEvidence) { r.PatchIdentity = "unavailable" },
		func(r *RebaseEvidence) { r.Verified = false },
		func(r *RebaseEvidence) { r.Status = "aborted" },
	} {
		r := evidence
		mutate(&r)
		st.LastRebase = &r
		recordRebaseTransition(st, head)
	}
	if len(st.RebaseTransitions) != 0 {
		t.Fatalf("recorded unverified transition: %+v", st.RebaseTransitions)
	}
	st.LastRebase = &evidence
	recordRebaseTransition(st, head)
	recordRebaseTransition(st, head)
	if len(st.RebaseTransitions) != 1 || st.RebaseTransitions[0].OldHead != old {
		t.Fatalf("transition: %+v", st.RebaseTransitions)
	}
	// A conflict resolution that changed the patch is still verified evidence.
	changed := RebaseEvidence{OldHead: head, NewHead: strings.Repeat("c", 40), Status: "completed", PatchIdentity: "changed", Verified: true}
	st.LastRebase = &changed
	recordRebaseTransition(st, changed.NewHead)
	if len(st.RebaseTransitions) != 2 || st.RebaseTransitions[1].PatchIdentity != "changed" {
		t.Fatalf("changed transition: %+v", st.RebaseTransitions)
	}
}

func TestCompletePRRevisionRecordsVerifiedRebase(t *testing.T) {
	for _, mode := range []string{"same-repo", "fork", "failed-publication"} {
		t.Run(mode, func(t *testing.T) {
			m, _, issue := testManager(t)
			ctx := context.Background()
			git := func(args ...string) string {
				t.Helper()
				b, e := gitfixture.Command(t, "", append([]string{"-C", m.repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)...).CombinedOutput()
				if e != nil {
					t.Fatalf("git: %s %v", b, e)
				}
				return strings.TrimSpace(string(b))
			}
			if _, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
				t.Fatal(err)
			}
			base := git("rev-parse", "HEAD")
			git("checkout", "-q", "-b", "feature")
			if err := os.WriteFile(filepath.Join(m.repoRoot, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", "feature.txt")
			git("commit", "-q", "-m", "feature")
			old := git("rev-parse", "HEAD")
			git("checkout", "-q", "-b", "upstream", base)
			if err := os.WriteFile(filepath.Join(m.repoRoot, "upstream.txt"), []byte("upstream\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", "upstream.txt")
			git("commit", "-q", "-m", "upstream")
			newBase := git("rev-parse", "HEAD")
			git("cherry-pick", old)
			rebased := git("rev-parse", "HEAD")
			st, rev, err := m.load(ctx, issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			st.PRNumber = 37
			st.HeadSHA = old
			st.PRRevision = &PRRevision{Number: 37, RemoteHeadSHA: old, LocalHeadSHA: rebased, Branch: "swarm/pr-37", Intent: "rebase"}
			st.LastRebase = &RebaseEvidence{OldHead: old, NewHead: rebased, OldBase: base, NewBase: newBase, Status: "completed"}
			if mode == "fork" {
				st.ForkRepo = "contributor/swarm"
			}
			if _, err = m.save(ctx, issue.Number, rev, st); err != nil {
				t.Fatal(err)
			}
			if mode == "failed-publication" {
				if _, err = m.CompletePRRevision(ctx, issue.Number, "local-dev", 37, old, rebased); err == nil {
					t.Fatal("publication succeeded without an atomic store")
				}
			} else if _, err = m.CompletePRRevisionLocal(ctx, issue.Number, "local-dev", 37, old, rebased); err != nil {
				t.Fatal(err)
			}
			got, err := m.GetCurrentState(ctx, issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "same-repo" {
				want = 1
			}
			if len(got.RebaseTransitions) != want {
				t.Fatalf("transitions: %+v", got.RebaseTransitions)
			}
			if want == 1 && (got.RebaseTransitions[0].OldHead != old || got.RebaseTransitions[0].NewHead != rebased || got.RebaseTransitions[0].PatchIdentity != "unchanged") {
				t.Fatalf("transition: %+v", got.RebaseTransitions[0])
			}
		})
	}
}
