package coordination

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/testutil"
)

var migrationNow = time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)

func leaseState(n int, status string, expires time.Time) []byte {
	return fmt.Appendf(nil, `{"lease":{"issue_number":%d,"status":%q,"expires_at":%q}}`, n, status, expires.Format(time.RFC3339))
}

// seedBranchLedger writes two issues, their retention, a contributor index,
// and a task branch in the branch namespace.
func seedBranchLedger(t *testing.T, f *ledgerFixture) (heads []string) {
	t.Helper()
	ctx := context.Background()
	store := NewGitStoreWithLayout(f.clone, "", BranchLayout)
	if _, err := store.CompareAndSwap(ctx, 1, "", leaseState(1, "released", migrationNow)); err != nil {
		t.Fatal(err)
	}
	rev, err := store.CompareAndSwap(ctx, 2, "", leaseState(2, "active", migrationNow.Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	head := f.commit("checkpoint")
	if _, err := store.CompareAndSwapAndPublish(ctx, 2, rev, leaseState(2, "active", migrationNow.Add(-time.Minute)), "swarm/issue-2-work", head); err != nil {
		t.Fatal(err)
	}
	f.git(f.clone, "push", "origin", f.base+":"+BranchLayout.CheckpointRef(1, f.base))
	f.git(f.clone, "push", "origin", f.base+":"+BranchLayout.ContributorRef("alice"))
	return []string{f.base, head}
}

// installReceiveLog records each receive-pack invocation's ref updates.
func installReceiveLog(t *testing.T, f *ledgerFixture) string {
	t.Helper()
	log := filepath.Join(f.root, "receive.log")
	hook := "#!/bin/sh\n{ cat; echo ---; } >> " + log + "\n"
	if err := testutil.WriteExecutable(filepath.Join(f.remote, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

func TestMigrateLedgerRefsMovesEverythingInOneAtomicPush(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, HiddenLayout)
	heads := seedBranchLedger(t, f)
	wantRefs := map[string]string{}
	for _, line := range strings.Split(f.snapshot(), "\n") {
		ref, sha, _ := strings.Cut(line, " ")
		if to, ok := BranchLayout.counterpartRef(ref); ok {
			wantRefs[to] = sha
		} else {
			wantRefs[ref] = sha
		}
	}
	log := installReceiveLog(t, f)
	report, err := NewGitStore(f.clone).MigrateLedgerRefs(ctx, migrationNow, false)
	if err != nil {
		t.Fatal(err)
	}
	// Two states, two retention refs, and one contributor index.
	if len(report.Moves) != 5 || report.AlreadyHidden != 0 {
		t.Fatalf("report = %+v", report)
	}
	got := map[string]string{}
	for _, line := range strings.Split(f.snapshot(), "\n") {
		ref, sha, _ := strings.Cut(line, " ")
		got[ref] = sha
	}
	if fmt.Sprint(got) != fmt.Sprint(wantRefs) {
		t.Fatalf("remote after migration:\n%v\nwant\n%v", got, wantRefs)
	}
	if got[HiddenLayout.CheckpointRef(2, heads[1])] != heads[1] || got["refs/heads/swarm/issue-2-work"] != heads[1] {
		t.Fatal("retention or task branch lost")
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	pushes := strings.Split(strings.TrimSuffix(string(data), "---\n"), "---\n")
	zero := strings.Repeat("0", 40)
	if len(pushes) != 1 {
		t.Fatalf("migration used %d pushes", len(pushes))
	}
	var creates, deletes int
	for _, line := range strings.Split(strings.TrimSpace(pushes[0]), "\n") {
		fields := strings.Fields(line)
		switch {
		case fields[0] == zero && strings.HasPrefix(fields[2], "refs/swarm/"):
			creates++
		case fields[1] == zero && IsLedgerRef(fields[2]) && strings.HasPrefix(fields[2], "refs/heads/"):
			deletes++
		default:
			t.Errorf("unexpected update %s", line)
		}
	}
	if creates != 5 || deletes != 5 {
		t.Fatalf("one push carried %d creates and %d deletes", creates, deletes)
	}

	// Rerunning is a no-op.
	before := f.snapshot()
	again, err := NewGitStore(f.clone).MigrateLedgerRefs(ctx, migrationNow, false)
	if err != nil || len(again.Moves) != 0 || again.AlreadyHidden != 5 {
		t.Fatalf("rerun = %+v, %v", again, err)
	}
	if f.snapshot() != before {
		t.Fatal("rerun changed refs")
	}

	// Cutover: while branch is still selected, stores refuse rather than
	// recreate a branch ledger; hidden stores read the moved state.
	branchStore := NewGitStoreWithLayout(f.clone, "", BranchLayout)
	if _, _, err := branchStore.Load(ctx, 1); !errors.Is(err, ErrLedgerNamespace) {
		t.Fatalf("branch load after migration: %v", err)
	}
	if _, err := branchStore.CompareAndSwap(ctx, 1, "", []byte(`{}`)); !errors.Is(err, ErrLedgerNamespace) {
		t.Fatalf("branch create after migration: %v", err)
	}
	if f.snapshot() != before {
		t.Fatal("refused branch write changed refs")
	}
	if body, _, err := NewGitStoreWithLayout(f.clone, "", HiddenLayout).Load(ctx, 1); err != nil || !strings.Contains(string(body), `"released"`) {
		t.Fatalf("hidden load after migration: %s, %v", body, err)
	}
}

func TestMigrateLedgerRefsRefusesUnsafeSnapshots(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *ledgerFixture)
		want  string
	}{
		{"active claim", func(t *testing.T, f *ledgerFixture) {
			if _, err := NewGitStore(f.clone).CompareAndSwap(ctx, 3, "", leaseState(3, "active", migrationNow.Add(time.Hour))); err != nil {
				t.Fatal(err)
			}
		}, "active claims in refs/heads/swarm-state/issue-3"},
		{"active claim already hidden", func(t *testing.T, f *ledgerFixture) {
			if _, err := NewGitStoreWithLayout(f.clone, "", HiddenLayout).CompareAndSwap(ctx, 3, "", leaseState(3, "active", migrationNow.Add(time.Hour))); err != nil {
				t.Fatal(err)
			}
		}, "active claims in refs/swarm/state/issue-3"},
		{"malformed state", func(t *testing.T, f *ledgerFixture) {
			if _, err := NewGitStore(f.clone).CompareAndSwap(ctx, 3, "", []byte("not json")); err != nil {
				t.Fatal(err)
			}
		}, "decode refs/heads/swarm-state/issue-3"},
		{"malformed state ref name", func(t *testing.T, f *ledgerFixture) {
			f.git(f.clone, "push", "origin", f.base+":refs/heads/swarm-state/issue-03")
		}, "unexpected ledger ref refs/heads/swarm-state/issue-03"},
		{"retention names another commit", func(t *testing.T, f *ledgerFixture) {
			f.git(f.clone, "push", "origin", f.base+":"+BranchLayout.CheckpointRef(4, strings.Repeat("a", 40)))
		}, "retention ref refs/heads/swarm-checkpoints/issue-4/"},
		{"duplicate with identical SHA", func(t *testing.T, f *ledgerFixture) {
			sha := f.git(f.remote, "rev-parse", BranchLayout.StateRef(1))
			f.git(f.clone, "push", "origin", sha+":"+HiddenLayout.StateRef(1))
		}, "both refs/heads/swarm-state/issue-1 and refs/swarm/state/issue-1 exist"},
		{"conflicting destination", func(t *testing.T, f *ledgerFixture) {
			f.git(f.clone, "push", "origin", f.base+":"+HiddenLayout.ContributorRef("alice"))
		}, "both refs/heads/swarm-contributors/alice and refs/swarm/contributors/alice exist"},
		{"fetch and push destinations differ", func(t *testing.T, f *ledgerFixture) {
			f.git(f.clone, "remote", "set-url", "--push", "origin", filepath.Join(f.root, "elsewhere.git"))
		}, "one identical fetch and push destination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLedgerFixture(t, HiddenLayout)
			seedBranchLedger(t, f)
			tc.setup(t, f)
			before := f.snapshot()
			for _, dryRun := range []bool{true, false} {
				_, err := NewGitStore(f.clone).MigrateLedgerRefs(ctx, migrationNow, dryRun)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("dry-run=%v: %v, want %q", dryRun, err, tc.want)
				}
			}
			if f.snapshot() != before {
				t.Fatal("refused migration changed refs")
			}
		})
	}
}

func TestMigrateLedgerRefsEmptyPartialAndDryRun(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t, HiddenLayout)
	before := f.snapshot()
	report, err := NewGitStore(f.clone).MigrateLedgerRefs(ctx, migrationNow, false)
	if err != nil || len(report.Moves) != 0 || f.snapshot() != before {
		t.Fatalf("empty repository: %+v, %v", report, err)
	}
	hidden := NewGitStoreWithLayout(f.clone, "", HiddenLayout)
	if _, err := hidden.CompareAndSwap(ctx, 1, "", leaseState(1, "released", migrationNow)); err != nil {
		t.Fatal(err)
	}
	branchRev, err := NewGitStore(f.clone).CompareAndSwap(ctx, 2, "", leaseState(2, "released", migrationNow))
	if err != nil {
		t.Fatal(err)
	}
	before = f.snapshot()
	report, err = NewGitStore(f.clone).MigrateLedgerRefs(ctx, migrationNow, true)
	if err != nil || len(report.Moves) != 1 || report.AlreadyHidden != 1 || !report.DryRun || f.snapshot() != before {
		t.Fatalf("dry run: %+v, %v", report, err)
	}
	report, err = NewGitStore(f.clone).MigrateLedgerRefs(ctx, migrationNow, false)
	if err != nil || len(report.Moves) != 1 || report.Moves[0] != (RefMove{From: BranchLayout.StateRef(2), To: HiddenLayout.StateRef(2), SHA: branchRev}) {
		t.Fatalf("partial migration: %+v, %v", report, err)
	}
	if got := f.git(f.remote, "rev-parse", HiddenLayout.StateRef(2)); got != branchRev {
		t.Fatalf("moved state = %s, want %s", got, branchRev)
	}
	f.assertNamespace()
}

// TestMigrateLedgerRefsConcurrentChangeRejectsWholePush races a competing
// clone against the snapshot; the leases reject every move together.
func TestMigrateLedgerRefsConcurrentChangeRejectsWholePush(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		race func(*ledgerFixture)
	}{
		{"source advanced", func(f *ledgerFixture) {
			store := NewGitStore(f.second)
			_, rev, err := store.Load(ctx, 1)
			if err != nil {
				f.t.Fatal(err)
			}
			if _, err := store.CompareAndSwap(ctx, 1, rev, leaseState(1, "active", migrationNow.Add(time.Hour))); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"target created", func(f *ledgerFixture) {
			f.git(f.second, "push", "origin", f.base+":"+HiddenLayout.StateRef(2))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLedgerFixture(t, HiddenLayout)
			seedBranchLedger(t, f)
			var raced string
			beforeMigrationPush = func() {
				tc.race(f)
				raced = f.snapshot()
			}
			t.Cleanup(func() { beforeMigrationPush = nil })
			_, err := NewGitStore(f.clone).MigrateLedgerRefs(ctx, migrationNow, false)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("raced migration: %v", err)
			}
			if f.snapshot() != raced {
				t.Fatal("rejected migration applied some moves")
			}
			for _, ref := range []string{BranchLayout.StateRef(1), BranchLayout.StateRef(2), BranchLayout.ContributorRef("alice")} {
				if f.git(f.remote, "for-each-ref", ref) == "" {
					t.Fatalf("%s moved despite the rejected push", ref)
				}
			}
		})
	}
}
