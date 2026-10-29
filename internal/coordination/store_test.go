package coordination

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitCASAcrossClonesAndAtomicCheckpointFence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(root, "remote.git")
	git(root, "init", "--bare", "--initial-branch=main", remote)
	first := filepath.Join(root, "first")
	git(root, "clone", remote, first)
	git(first, "config", "user.name", "CAS Test")
	git(first, "config", "user.email", "test@example.invalid")
	git(first, "commit", "--allow-empty", "-m", "baseline")
	git(first, "push", "origin", "main")
	second := filepath.Join(root, "second")
	git(root, "clone", remote, second)
	stores := []*GitStore{NewGitStore(first), NewGitStore(second)}
	initial, err := stores[0].CompareAndSwap(ctx, 1, "", []byte(`{"status":"ready"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range stores {
		if _, _, err := store.Load(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for index, store := range stores {
		workers.Add(1)
		go func(index int, store *GitStore) {
			defer workers.Done()
			_, err := store.CompareAndSwap(ctx, 1, initial, []byte(fmt.Sprintf(`{"claim":"worker-%d"}`, index)))
			results <- err
		}(index, store)
	}
	workers.Wait()
	close(results)
	winners, conflicts := 0, 0
	for err := range results {
		if err == nil {
			winners++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("CAS admitted %d winners and %d conflicts", winners, conflicts)
	}
	_, current, err := stores[0].Load(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	head := git(first, "rev-parse", "HEAD")
	branch := "swarm/issue-1-checkpoint"
	published, err := stores[0].CompareAndSwapAndPublish(ctx, 1, current, []byte(`{"status":"checkpointed"}`), branch, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "candidate.txt"), []byte("stale change"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(first, "add", ".")
	git(first, "commit", "-m", "stale candidate")
	staleHead := git(first, "rev-parse", "HEAD")
	if _, err := stores[0].CompareAndSwap(ctx, 1, published, []byte(`{"claim":"successor"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := stores[0].CompareAndSwapAndPublish(ctx, 1, published, []byte(`{"claim":"stale"}`), branch, staleHead); err == nil {
		t.Fatal("stale worker published checkpoint")
	}
	if remoteHead := git(remote, "rev-parse", "refs/heads/"+branch); remoteHead != head {
		t.Fatalf("rejected atomic push changed checkpoint to %s", remoteHead)
	}
}

func TestPRPublishRequiresCapturedStateAndRemoteHead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(root, "remote.git")
	git(root, "init", "--bare", "--initial-branch=main", remote)
	first := filepath.Join(root, "first")
	git(root, "clone", remote, first)
	git(first, "config", "user.name", "PR CAS Test")
	git(first, "config", "user.email", "test@example.invalid")
	git(first, "commit", "--allow-empty", "-m", "baseline")
	base := git(first, "rev-parse", "HEAD")
	git(first, "push", "origin", "main")
	branch := "swarm/issue-2-pr"
	git(first, "branch", branch, base)
	git(first, "push", "origin", branch)
	store := NewGitStore(first)
	oldState, err := store.CompareAndSwap(ctx, 2, "", []byte(`{"revision":"pending"}`))
	if err != nil {
		t.Fatal(err)
	}
	git(first, "checkout", "-b", "swarm/pr-9", base)
	git(first, "commit", "--allow-empty", "-m", "local PR candidate")
	candidate := git(first, "rev-parse", "HEAD")
	if _, err := store.CompareAndSwapAndPublishPR(ctx, 2, strings.Repeat("f", 40), []byte(`{"revision":"published"}`), branch, base, candidate); err == nil {
		t.Fatal("stale state lease published PR")
	}
	if got := git(remote, "rev-parse", "refs/heads/"+branch); got != base {
		t.Fatalf("stale state push moved PR to %s", got)
	}
	if _, err := store.CompareAndSwapAndPublishPR(ctx, 2, oldState, []byte(`{"revision":"published"}`), branch, strings.Repeat("e", 40), candidate); err == nil {
		t.Fatal("stale PR head lease published")
	}
	if _, err := store.CompareAndSwapAndPublishPR(ctx, 2, oldState, []byte(`{"revision":"published"}`), branch, base, candidate); err != nil {
		t.Fatal(err)
	}
	if got := git(remote, "rev-parse", "refs/heads/"+branch); got != candidate {
		t.Fatalf("atomic publish head = %s, want %s", got, candidate)
	}
	if body, _, err := store.Load(ctx, 2); err != nil || string(body) != `{"revision":"published"}` {
		t.Fatalf("atomic publish state = %s: %v", body, err)
	}
}

func TestConcurrentIssueReadsStayPinnedAndTransportErrorsSurface(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	clone := filepath.Join(root, "clone")
	for _, args := range [][]string{{"init", "--bare", remote}, {"clone", remote, clone}} {
		if out, err := gitfixture.Command(t, "", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	store := NewGitStore(clone)
	ctx := context.Background()
	for issue := 1; issue <= 4; issue++ {
		if _, err := store.CompareAndSwap(ctx, issue, "", []byte(fmt.Sprint(issue))); err != nil {
			t.Fatal(err)
		}
	}
	var workers sync.WaitGroup
	for issue := 1; issue <= 4; issue++ {
		workers.Add(1)
		go func(issue int) {
			defer workers.Done()
			body, _, err := store.Load(ctx, issue)
			if err != nil || string(body) != fmt.Sprint(issue) {
				t.Errorf("issue %d read %q: %v", issue, body, err)
			}
		}(issue)
	}
	workers.Wait()
	if out, err := gitfixture.Command(t, "", "-C", clone, "remote", "set-url", "origin", filepath.Join(root, "missing")).CombinedOutput(); err != nil {
		t.Fatalf("set-url: %v: %s", err, out)
	}
	if _, _, err := store.Load(ctx, 1); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("transport error hidden as absent state: %v", err)
	}
}

func TestForkIndexedStoreAtomicCapAndReplay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(root, "origin.git")
	git(root, "init", "--bare", "--initial-branch=main", remote)
	clone := filepath.Join(root, "clone")
	git(root, "clone", remote, clone)
	git(clone, "config", "user.name", "Fork Index Test")
	git(clone, "config", "user.email", "test@example.invalid")
	git(clone, "commit", "--allow-empty", "-m", "initial")
	git(clone, "push", "origin", "main")

	baseStore := NewGitStore(clone)
	var anyStore Store = NewForkIndexedStore(clone, "alice", 1, 100)
	if _, ok := anyStore.(PublishingStore); ok {
		t.Fatal("ForkIndexedStore must never implement PublishingStore")
	}

	rev1, err := baseStore.CompareAndSwap(ctx, 1, "", []byte(`{"lease":{"issue_number":1,"status":"draft"}}`))
	if err != nil {
		t.Fatal(err)
	}
	rev2, err := baseStore.CompareAndSwap(ctx, 2, "", []byte(`{"lease":{"issue_number":2,"status":"draft"}}`))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour).Format(time.RFC3339)

	// Acquire issue #1 as alice with request ID 101
	s1 := NewForkIndexedStore(clone, "alice", 1, 101)
	s1.SetNowFunc(func() time.Time { return now })
	active1 := fmt.Sprintf(`{"lease":{"issue_number":1,"claim_id":"c1","owner":"alice","status":"active","expires_at":%q}}`, exp)
	rev1After, err := s1.CompareAndSwap(ctx, 1, rev1, []byte(active1))
	if err != nil {
		t.Fatalf("acquire issue 1 failed: %v", err)
	}

	// Replaying request ID 101 (or older ID 100) must fail with ErrReplay
	sReplay := NewForkIndexedStore(clone, "alice", 1, 101)
	sReplay.SetNowFunc(func() time.Time { return now })
	if _, err := sReplay.CompareAndSwap(ctx, 1, rev1After, []byte(active1)); !errors.Is(err, ErrReplay) {
		t.Fatalf("expected ErrReplay for duplicate request ID, got %v", err)
	}

	// Acquiring issue #2 while issue #1 is active exceeds maxActive=1
	s2 := NewForkIndexedStore(clone, "alice", 1, 102)
	s2.SetNowFunc(func() time.Time { return now })
	active2 := fmt.Sprintf(`{"lease":{"issue_number":2,"claim_id":"c2","owner":"alice","status":"active","expires_at":%q}}`, exp)
	if _, err := s2.CompareAndSwap(ctx, 2, rev2, []byte(active2)); !errors.Is(err, ErrForkClaimLimit) {
		t.Fatalf("expected ErrForkClaimLimit on second active claim, got %v", err)
	}

	// Advance clock past issue #1 expiry: revalidation prunes issue #1 and admits issue #2
	later := now.Add(2 * time.Hour)
	exp2 := later.Add(time.Hour).Format(time.RFC3339)
	s2Later := NewForkIndexedStore(clone, "alice", 1, 103)
	s2Later.SetNowFunc(func() time.Time { return later })
	active2Later := fmt.Sprintf(`{"lease":{"issue_number":2,"claim_id":"c2","owner":"alice","status":"active","expires_at":%q}}`, exp2)
	if _, err := s2Later.CompareAndSwap(ctx, 2, rev2, []byte(active2Later)); err != nil {
		t.Fatalf("acquire issue 2 after issue 1 expiry failed: %v", err)
	}
}

func TestGitCASRetryClassifier(t *testing.T) {
	ref := BranchLayout.StateRef(306)
	positives := []string{"remote: fatal error in commit_refs", "! [remote rejected] " + ref + " -> " + ref + " (fatal error in commit_refs)"}
	for _, positive := range positives {
		if !commitRefsTransient(" \t"+positive+"\n", ref) {
			t.Fatal(positive)
		}
		for _, denial := range []string{"fatal: Authentication failed", "fatal: could not read Username", "remote: Permission denied", "remote: access denied", "remote: authorization failed", "remote: not authorized", "fatal: HTTP 403", "fatal: HTTP 401", "remote: policy rejected", "remote: protected branch update failed", "remote: GH006", "remote: GH013", "remote: pre-receive hook declined", "! [rejected] " + ref + " -> " + ref + " (stale info)", "remote: non-fast-forward"} {
			if commitRefsTransient(positive+"\n"+strings.ToUpper(denial), ref) {
				t.Fatal(denial)
			}
		}
	}
	for _, negative := range []string{"fatal error in commit_refs", "remote: fatal error in commit_refs extra", "remote: fatal error in commit_refs_extra", "remote rejected", "! [remote rejected] refs/heads/other -> refs/heads/other (fatal error in commit_refs)", "! [remote rejected] " + ref + " -> other (fatal error in commit_refs)"} {
		if commitRefsTransient(negative, ref) {
			t.Fatal(negative)
		}
	}
	for _, n := range []int{401, 403, 1403} {
		r := BranchLayout.StateRef(n)
		if commitRefsTransient("! [remote rejected] "+r+" -> "+r+" (fatal error in commit_refs)", r) {
			t.Fatal(r)
		}
	}
}

func TestGitCASRetryTransport(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		for _, existing := range []bool{false, true} {
			for _, mode := range []string{"retry", "lost", "retry-lost", "exhaust", "deny", "ok-old", "successor", "ack-successor", "malformed", "malformed-sha", "read-error", "namespace", "pre-malformed", "pre-desired", "pre-successor", "disappear", "cancel", "deadline", "final-lost", "deny-lost", "backoff", "rejected", "race-successor", "failed-delete", "pre-delete"} {
				t.Run(fmt.Sprintf("%s/%t/%s", layout.Name(), existing, mode), func(t *testing.T) {
					f := newRetentionFixture(t)
					f.git(f.clone, "remote", "rename", "origin", "upstream")
					store := NewGitStoreWithLayout(f.clone, "upstream", layout)
					ctx := context.Background()
					old := ""
					var err error
					if existing {
						old, err = store.CompareAndSwap(ctx, 306, "", []byte("old"))
						if err != nil {
							t.Fatal(err)
						}
					}
					real, err := exec.LookPath("git")
					if err != nil {
						t.Fatal(err)
					}
					dir := t.TempDir()
					script := `#!/bin/sh
case "$*" in
 *hash-object*|*mktree*|*commit-tree*) echo "$*" >> "$CAS_LOG.prep";;
esac
if [ "$1" = push ]; then
 echo "$*" >> "$CAS_LOG"
 count=$(wc -l < "$CAS_LOG")
 case "$CAS_MODE" in
 rejected) if [ "$count" = 1 ]; then ref=${4#*:}; echo " ! [remote rejected] $ref -> $ref (fatal error in commit_refs)" >&2; exit 1; fi;;
 failed-delete) if [ "$count" = 1 ]; then ref=${4#*:}; "$CAS_REAL" push upstream ":$ref" >/dev/null 2>&1; echo 'remote: fatal error in commit_refs' >&2; exit 1; fi;;
 pre-delete) if [ "$count" = 1 ]; then touch "$CAS_LOG.bad"; echo 'remote: fatal error in commit_refs' >&2; exit 1; fi;;
 race-successor)
 if [ "$count" = 1 ]; then echo 'remote: fatal error in commit_refs' >&2; exit 1; fi
 desired=${4%%:*}; ref=${4#*:}
 tree=$("$CAS_REAL" rev-parse "$desired^{tree}")
 successor=$(echo successor | "$CAS_REAL" -c user.name=Test -c user.email=test@example.invalid commit-tree "$tree" -p "$desired")
 "$CAS_REAL" push upstream "$successor:$ref" >/dev/null 2>&1 || exit 2;;
 final-lost) if [ "$count" != 3 ]; then echo 'remote: fatal error in commit_refs' >&2; exit 1; fi;;
 backoff) touch "$CAS_LOG.backoff"; echo 'remote: fatal error in commit_refs' >&2; exit 1;;
 retry|retry-lost) if [ "$count" = 1 ]; then echo 'remote: fatal error in commit_refs' >&2; exit 1; fi;;
 exhaust) echo 'remote: fatal error in commit_refs' >&2; exit 1;;
 deny) echo 'remote: fatal error in commit_refs' >&2; echo 'remote: policy rejected' >&2; exit 1;;
 ok-old) exit 0;;
 read-error|namespace|pre-malformed|pre-desired|pre-successor) touch "$CAS_LOG.bad"; echo 'remote: fatal error in commit_refs' >&2; exit 1;;
 malformed-sha) touch "$CAS_LOG.bad"; echo 'remote: fatal error in commit_refs' >&2; exit 1;;
 malformed) touch "$CAS_LOG.bad"; echo 'remote: fatal error in commit_refs' >&2; exit 1;;
 esac
 "$CAS_REAL" "$@" || exit $?
 case "$CAS_MODE" in
 successor|ack-successor|disappear)
 spec="$4"; desired=${spec%%:*}; ref=${spec#*:}
 if [ "$CAS_MODE" = disappear ]; then "$CAS_REAL" push upstream ":$ref" >/dev/null 2>&1; exit 0; fi
 tree=$("$CAS_REAL" rev-parse "$desired^{tree}")
 successor=$(echo successor | "$CAS_REAL" -c user.name=Test -c user.email=test@example.invalid commit-tree "$tree" -p "$desired")
 "$CAS_REAL" push upstream "$successor:$ref" >/dev/null 2>&1 || exit 2
 [ "$CAS_MODE" = ack-successor ] && exit 0
 echo 'remote: fatal error in commit_refs' >&2; exit 1;;
 cancel|deadline) touch "$CAS_LOG.landed"; while :; do :; done;;
 deny-lost) echo 'remote: policy rejected' >&2; exit 1;;
 lost|retry-lost|final-lost) echo 'remote: fatal error in commit_refs' >&2; exit 1;;
 esac
 exit 0
fi
if [ "$1" = ls-remote ] && [ -f "$CAS_LOG.backoff" ]; then
 "$CAS_REAL" "$@"; touch "$CAS_LOG.waiting"; exit 0
fi
if [ "$1" = ls-remote ] && [ -f "$CAS_LOG.bad" ]; then
 case "$CAS_MODE" in
 malformed-sha) spec=$(tail -n 1 "$CAS_LOG"); spec=${spec##* }; ref=${spec#*:}; echo "not-a-sha $ref"; exit 0;;
 read-error) echo inspection-failed >&2; exit 1;;
 namespace) "$CAS_REAL" "$@"; echo "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/swarm-state/issue-306"; echo "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/swarm/state/issue-306"; exit 0;;
 pre-*)
 if [ ! -f "$CAS_LOG.read" ]; then touch "$CAS_LOG.read"; exec "$CAS_REAL" "$@"; fi
 case "$CAS_MODE" in
 pre-delete)
 spec=$(tail -n 1 "$CAS_LOG"); spec=${spec##* }; ref=${spec#*:}
 "$CAS_REAL" push upstream ":$ref" >/dev/null 2>&1
 rm "$CAS_LOG.bad"
 exec "$CAS_REAL" "$@";;
 pre-malformed) echo malformed; exit 0;;
 pre-desired|pre-successor)
 spec=$(tail -n 1 "$CAS_LOG"); spec=${spec##* }; desired=${spec%%:*}; ref=${spec#*:}
 "$CAS_REAL" push upstream "$desired:$ref" >/dev/null 2>&1 || exit 2
 if [ "$CAS_MODE" = pre-successor ]; then
 tree=$("$CAS_REAL" rev-parse "$desired^{tree}")
 successor=$(echo successor | "$CAS_REAL" -c user.name=Test -c user.email=test@example.invalid commit-tree "$tree" -p "$desired")
 "$CAS_REAL" push upstream "$successor:$ref" >/dev/null 2>&1 || exit 2
 fi
 exec "$CAS_REAL" "$@";;
 esac;;
 esac
 echo malformed; exit 0
fi
exec "$CAS_REAL" "$@"
`
					if err := testutil.WriteExecutable(filepath.Join(dir, "git"), []byte(script), 0755); err != nil {
						t.Fatal(err)
					}
					log := filepath.Join(dir, "calls")
					t.Setenv("CAS_REAL", real)
					t.Setenv("CAS_MODE", mode)
					t.Setenv("CAS_LOG", log)
					t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
					var stop context.CancelFunc
					if mode == "cancel" || mode == "deadline" || mode == "backoff" {
						if mode == "deadline" {
							ctx, stop = context.WithTimeout(ctx, 500*time.Millisecond)
						} else {
							ctx, stop = context.WithCancel(ctx)
						}
						defer stop()
						if mode == "cancel" || mode == "backoff" {
							done := make(chan struct{})
							go func() {
								defer close(done)
								ticker := time.NewTicker(time.Millisecond)
								defer ticker.Stop()
								for {
									select {
									case <-ctx.Done():
										return
									case <-ticker.C:
										marker := log + ".landed"
										if mode == "backoff" {
											marker = log + ".waiting"
										}
										if _, e := os.Stat(marker); e == nil {
											if mode == "backoff" {
												time.Sleep(20 * time.Millisecond)
											}
											stop()
											return
										}
									}
								}
							}()
							defer func() { stop(); <-done }()
						}
					}
					start := time.Now()
					desired, err := store.CompareAndSwap(ctx, 306, old, []byte("next"))
					calls, _ := os.ReadFile(log)
					lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
					want := 1
					if mode == "retry" || mode == "retry-lost" || mode == "rejected" || mode == "race-successor" || ((mode == "failed-delete" || mode == "pre-delete") && !existing) {
						want = 2
					}
					if mode == "exhaust" || mode == "final-lost" {
						want = 3
					}
					if len(lines) != want {
						t.Fatalf("pushes %d want %d: %s", len(lines), want, calls)
					}
					for _, line := range lines {
						if line != lines[0] {
							t.Fatalf("changed push: %s", calls)
						}
					}
					argv := strings.Fields(lines[0])
					if len(argv) != 4 || argv[1] != "upstream" || argv[2] != "--force-with-lease="+layout.StateRef(306)+":"+old || !strings.HasSuffix(argv[3], ":"+layout.StateRef(306)) {
						t.Fatalf("original CAS fence changed: %s", lines[0])
					}
					if mode == "successor" || mode == "pre-successor" || mode == "race-successor" || mode == "ack-successor" {
						prepared, _, _ := strings.Cut(argv[3], ":")
						if parent := f.git(f.remote, "rev-parse", layout.StateRef(306)+"^"); parent != prepared {
							t.Fatalf("successor lost: parent %s, desired %s", parent, prepared)
						}
					}
					prep, _ := os.ReadFile(log + ".prep")
					if len(strings.Split(strings.TrimSpace(string(prep)), "\n")) != 3 {
						t.Fatalf("preparation replayed: %s", prep)
					}
					switch mode {
					case "retry", "lost", "retry-lost", "pre-desired", "final-lost", "deny-lost", "rejected":
						if err != nil {
							t.Fatal(err)
						}
						actual := f.git(f.remote, "rev-parse", layout.StateRef(306))
						if actual != desired {
							t.Fatal("unverified success")
						}
					case "failed-delete", "pre-delete":
						if existing {
							if !errors.Is(err, ErrConflict) {
								t.Fatal(err)
							}
						} else if err != nil {
							t.Fatal(err)
						}
					case "successor", "pre-successor", "race-successor":
						if !errors.Is(err, ErrConflict) {
							t.Fatalf("expected conflict: %v", err)
						}
					case "exhaust", "ok-old", "ack-successor", "malformed", "malformed-sha", "read-error", "namespace", "pre-malformed", "disappear", "cancel", "deadline", "backoff":
						if !errors.Is(err, ErrPublicationUnknown) || errors.Is(err, ErrConflict) {
							t.Fatalf("expected non-conflict unknown: %v", err)
						}
					case "deny":
						if err == nil || errors.Is(err, ErrConflict) || errors.Is(err, ErrPublicationUnknown) {
							t.Fatal(err)
						}
					}
					if mode == "cancel" || mode == "deadline" || mode == "backoff" {
						cause := context.Canceled
						if mode == "deadline" {
							cause = context.DeadlineExceeded
						}
						var exit *exec.ExitError
						if !errors.Is(err, cause) || (mode != "backoff" && !errors.As(err, &exit)) {
							t.Fatalf("context/exit cause lost: %v", err)
						}
					}
					if mode == "exhaust" && time.Since(start) < 300*time.Millisecond {
						t.Fatal("missing backoff")
					}
				})
			}
		}
	}
}

func TestGitCASRetryCancelledBeforePublication(t *testing.T) {
	f := newRetentionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.store.CompareAndSwap(ctx, 306, "", []byte("next"))
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrConflict) || errors.Is(err, ErrPublicationUnknown) {
		t.Fatalf("preparation cancellation: %v", err)
	}
	if out := f.git(f.remote, "for-each-ref", "--format=%(refname)", BranchLayout.StateRef(306)); out != "" {
		t.Fatalf("cancelled publication created %s", out)
	}
}

func TestGitCASRetryRedaction(t *testing.T) {
	if os.Getenv("CAS_REDACTION_CHILD") == "" {
		for _, token := range []string{"remote: fatal error in commit_refs", "remote: policy rejected"} {
			cmd := exec.Command(os.Args[0], "-test.run=^TestGitCASRetryRedaction$")
			cmd.Env = append(os.Environ(), "CAS_REDACTION_CHILD=1", "SWARM_GITHUB_TOKEN="+token, "SWARM_GITHUB_TOKEN_COMMAND=")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("redaction child: %v: %s", err, out)
			}
		}
		return
	}
	f := newRetentionFixture(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	token := os.Getenv("SWARM_GITHUB_TOKEN")
	diagnostic := token
	want := 1
	if token == "remote: policy rejected" {
		diagnostic = "remote: fatal error in commit_refs\n" + token
		want = 3
	}
	script := "#!/bin/sh\nif [ \"$1\" = push ]; then\n echo push >> \"$REDACT_LOG\"\n printf '%s\\n' \"$REDACT_OUTPUT\" >&2\n exit 1\nfi\nexec \"$REDACT_REAL\" \"$@\"\n"
	if err := testutil.WriteExecutable(filepath.Join(dir, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REDACT_REAL", real)
	t.Setenv("REDACT_LOG", filepath.Join(dir, "calls"))
	t.Setenv("REDACT_OUTPUT", diagnostic)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = f.store.CompareAndSwap(context.Background(), 306, "", []byte("next"))
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[REDACTED]") || errors.Is(err, ErrConflict) {
		t.Fatalf("redaction failed: %v", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Count(string(calls), "push") != want {
		t.Fatalf("classification used raw diagnostic: %s", calls)
	}
}
