package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/workspace"
)

func completionFixture(t *testing.T, local bool, commands ...string) *reconciliationFixture {
	t.Helper()
	command := "true"
	if len(commands) > 0 {
		command = commands[0]
	}
	f := newReconciliationFixture(t, command, local)
	f.git(f.path, "reset", "--hard", f.a)
	f.write(f.path, "finished.txt", "finished\n")
	f.git(f.path, "add", "finished.txt")
	f.git(f.path, "commit", "-m", "finished implementation")
	f.b = f.git(f.path, "rev-parse", "HEAD")
	f.write(f.path, ".swarm/active/IMPLEMENTATION.md", "Implemented and tested.\n")
	st, err := f.m.GetCurrentState(t.Context(), f.n)
	if err != nil {
		t.Fatal(err)
	}
	md := ReviewBindings(st)
	md["head_sha"] = f.b
	art := contract.Artifact{Type: contract.ArtifactImplementation, Producer: "mock:developer", Verdict: "PASS", Summary: "Finished", Body: "Implemented and tested.", Metadata: md}
	if err = f.m.SaveCheckpointCompletion(t.Context(), f.n, "local-dev", f.a, f.b, art); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCheckpointCompletionRecovery(t *testing.T) {
	for _, local := range []bool{false, true} {
		name := "git"
		if local {
			name = "local"
		}
		t.Run(name, func(t *testing.T) {
			f := completionFixture(t, local)
			// Restore the manager from durable state, retaining the exact claim token.
			claim := f.m.token(f.n)
			f.m.BindClaim(f.n, claim)
			st, err := f.m.RecoverCheckpointCompletion(t.Context(), f.n, "local-dev")
			if err != nil {
				t.Fatal(err)
			}
			tree, err := f.m.implementationTree(t.Context(), f.b)
			if err != nil {
				t.Fatal(err)
			}
			if err = VerifyTrustedImplementation(st, f.b, tree, f.branch); err != nil {
				t.Fatal(err)
			}
			c, err := f.m.InspectCheckpointCompletion(t.Context(), f.n)
			if err != nil || c != nil {
				t.Fatalf("not retired: %+v %v", c, err)
			}
			if !local && !strings.HasPrefix(f.git(f.m.repoRoot, "ls-remote", "origin", "refs/heads/"+f.branch), f.b) {
				t.Fatal("not published")
			}
		})
	}
}

// Signed mutations model stale coordinator observations, separately from MAC
// corruption. Neither may be accepted as trusted completion.
func TestCheckpointCompletionRejectsChangedBindings(t *testing.T) {
	mutations := map[string]func(*CheckpointCompletion){
		"repository":  func(c *CheckpointCompletion) { c.Repository = "other" },
		"issue":       func(c *CheckpointCompletion) { c.Issue++ },
		"version":     func(c *CheckpointCompletion) { c.Version++ },
		"claim":       func(c *CheckpointCompletion) { c.Claim = "successor" },
		"owner":       func(c *CheckpointCompletion) { c.Owner = "other" },
		"predecessor": func(c *CheckpointCompletion) { c.Predecessor = c.Candidate },
		"candidate":   func(c *CheckpointCompletion) { c.Candidate = c.Predecessor },
		"tree":        func(c *CheckpointCompletion) { c.Tree = strings.Repeat("a", 40) },
		"branch":      func(c *CheckpointCompletion) { c.Branch = "other" },
		"report":      func(c *CheckpointCompletion) { c.Report = "other" },
		"approval":    func(c *CheckpointCompletion) { c.Approval = "other" },
		"artifact":    func(c *CheckpointCompletion) { c.Artifact.Body = "forged"; c.Artifact.Verdict = "FAIL" },
	}
	for _, key := range []string{"task_digest", "policy_digest", "policy_tree_digest", "base_sha", "plan_digest", "test_plan_digest", "review_claim_id"} {
		key := key
		mutations[key] = func(c *CheckpointCompletion) { c.Bindings[key] = "other" }
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f := completionFixture(t, true)
			c, err := f.m.InspectCheckpointCompletion(t.Context(), f.n)
			if err != nil {
				t.Fatal(err)
			}
			mutate(c)
			rewriteCompletion(t, f, c)
			assertCompletionRejected(t, f)
		})
	}
}

func rewriteCompletion(t *testing.T, f *reconciliationFixture, c *CheckpointCompletion) {
	t.Helper()
	root, _, err := f.m.completionRoot(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	key, err := root.ReadFile("key/material", 32)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(c)
	raw, _ := json.Marshal(completionEnvelope{payload, completionMAC(key, payload)})
	if err = root.WriteFile(completionName(f.n), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func assertCompletionRejected(t *testing.T, f *reconciliationFixture) {
	t.Helper()
	ctx := context.Background()
	before := f.git(f.path, "rev-parse", "HEAD")
	status := f.git(f.path, "status", "--porcelain", "--untracked-files=all")
	report, reportErr := os.ReadFile(filepath.Join(f.path, ".swarm/active/IMPLEMENTATION.md"))
	st, err := f.m.GetCurrentState(ctx, f.n)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	if _, err = f.m.RecoverCheckpointCompletion(ctx, f.n, "local-dev"); err == nil {
		t.Fatal("accepted invalid completion")
	}
	after, err := f.m.GetCurrentState(ctx, f.n)
	if err != nil {
		t.Fatal(err)
	}
	next, _ := json.Marshal(after)
	if string(next) != string(raw) || before != f.git(f.path, "rev-parse", "HEAD") || status != f.git(f.path, "status", "--porcelain", "--untracked-files=all") {
		t.Fatal("rejection changed work or state")
	}
	data, e := os.ReadFile(filepath.Join(f.path, ".swarm/active/IMPLEMENTATION.md"))
	if (e == nil) != (reportErr == nil) || string(data) != string(report) {
		t.Fatal("rejection changed report")
	}
}

func TestCheckpointCompletionRejectsCorruption(t *testing.T) {
	for _, name := range []string{"mac", "trailing", "oversized", "missing-key", "malformed", "symlink", "directory"} {
		t.Run(name, func(t *testing.T) {
			f := completionFixture(t, true)
			root, _, err := f.m.completionRoot(t.Context(), false)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			switch name {
			case "mac":
				raw, _ := root.ReadFile(completionName(f.n), completionLimit)
				var e completionEnvelope
				json.Unmarshal(raw, &e)
				e.MAC = strings.Repeat("0", 64)
				raw, _ = json.Marshal(e)
				err = root.WriteFile(completionName(f.n), raw, 0600)
			case "trailing":
				raw, _ := root.ReadFile(completionName(f.n), completionLimit)
				err = root.WriteFile(completionName(f.n), append(raw, []byte(" {}")...), 0600)
			case "oversized":
				err = root.WriteFile(completionName(f.n), []byte(strings.Repeat("x", completionLimit+1)), 0600)
			case "missing-key":
				err = root.Remove("key/material")
			case "malformed":
				err = root.WriteFile(completionName(f.n), []byte("{"), 0600)
			case "symlink":
				root.Remove(completionName(f.n))
				err = os.Symlink(filepath.Join(f.path, ".swarm/active/IMPLEMENTATION.md"), filepath.Join(f.m.repoRoot, ".git", completionDirectory, completionName(f.n)))
			case "directory":
				root.Remove(completionName(f.n))
				err = root.Mkdir(completionName(f.n), 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertCompletionRejected(t, f)
		})
	}
}

func TestCheckpointCompletionRejectsChangedWork(t *testing.T) {
	for _, name := range []string{"tracked", "untracked", "report", "branch", "candidate", "expired", "successor", "rebase", "fork", "pr"} {
		t.Run(name, func(t *testing.T) {
			f := completionFixture(t, true)
			switch name {
			case "tracked":
				f.write(f.path, "finished.txt", "edited")
			case "untracked":
				f.write(f.path, "untracked.txt", "edited")
			case "report":
				f.write(f.path, ".swarm/active/IMPLEMENTATION.md", "edited")
			case "branch":
				f.git(f.path, "checkout", "-b", "other")
			case "candidate":
				f.git(f.path, "commit", "--allow-empty", "-m", "new candidate")
			case "rebase":
				p := f.git(f.path, "rev-parse", "--git-path", "rebase-merge")
				if !filepath.IsAbs(p) {
					p = filepath.Join(f.path, p)
				}
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				_, err := f.m.mutate(t.Context(), f.n, "local-dev", func(st *IssueState) error {
					switch name {
					case "expired":
						st.Lease.ExpiresAt = time.Now().Add(-time.Hour)
					case "successor":
						st.Lease.ClaimID = "new"
					case "fork":
						st.ForkRepo = "other/repo"
					case "pr":
						st.PRNumber = 10
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			assertCompletionRejected(t, f)
		})
	}
}

func TestCheckpointCompletionAlreadyPublished(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(map[bool]string{false: "after-publication", true: "after-artifact"}[trusted], func(t *testing.T) {
			f := completionFixture(t, true)
			c, _ := f.m.InspectCheckpointCompletion(t.Context(), f.n)
			if _, err := f.m.ReconcileCheckpoint(t.Context(), f.n, "local-dev", f.a, f.b, false); err != nil {
				t.Fatal(err)
			}
			if trusted {
				if _, err := f.m.RecordTrustedImplementation(t.Context(), f.n, "local-dev", c.Artifact); err != nil {
					t.Fatal(err)
				}
			}
			st, err := f.m.RecoverCheckpointCompletion(t.Context(), f.n, "local-dev")
			if err != nil {
				t.Fatal(err)
			}
			if err = VerifyTrustedImplementation(st, c.Candidate, c.Tree, c.Branch); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckpointCompletionMissingIsNotAuthority(t *testing.T) {
	f := completionFixture(t, true)
	root, _, _ := f.m.completionRoot(t.Context(), false)
	defer root.Close()
	root.Remove(completionName(f.n))
	c, err := f.m.InspectCheckpointCompletion(t.Context(), f.n)
	if c != nil || err != nil {
		t.Fatal(c, err)
	}
	_, err = f.m.RecoverCheckpointCompletion(t.Context(), f.n, "local-dev")
	if err == nil {
		t.Fatal("absence granted recovery")
	}
	if _, err = safefs.ReadFile(f.path, ".swarm/active/IMPLEMENTATION.md", 1<<20); errors.Is(err, os.ErrNotExist) {
		t.Fatal("report removed")
	}
}

func TestCheckpointCompletionRejectsRootAlias(t *testing.T) {
	f := completionFixture(t, true)
	alias := filepath.Join(t.TempDir(), "repo")
	if err := os.Symlink(f.m.repoRoot, alias); err != nil {
		t.Fatal(err)
	}
	f.m.repoRoot = alias
	if _, err := f.m.InspectCheckpointCompletion(t.Context(), f.n); err == nil {
		t.Fatal("accepted aliased receipt root")
	}
}

func completionShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func TestCheckpointCompletionPublicationRechecks(t *testing.T) {
	for _, kind := range []string{"report", "dirty", "candidate", "expiry", "receipt"} {
		t.Run(kind, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "check.sh")
			if err := os.WriteFile(script, []byte("true\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f := completionFixture(t, false, "sh "+completionShellQuote(script))
			command := ""
			switch kind {
			case "report":
				command = "printf edited > " + completionShellQuote(filepath.Join(f.path, ".swarm/active/IMPLEMENTATION.md"))
			case "dirty":
				command = "printf edited > " + completionShellQuote(filepath.Join(f.path, "finished.txt"))
			case "candidate":
				command = "git -C " + completionShellQuote(f.path) + " commit --allow-empty -m raced"
			case "receipt":
				command = "printf edited > " + completionShellQuote(filepath.Join(f.m.repoRoot, ".git", completionDirectory, completionName(f.n)))
			case "expiry":
				marker := filepath.Join(t.TempDir(), "expired")
				command = "touch " + completionShellQuote(marker)
				st, _ := f.m.GetCurrentState(t.Context(), f.n)
				now := time.Now()
				f.m.SetNowFunc(func() time.Time {
					if _, err := os.Stat(marker); err == nil {
						return st.Lease.ExpiresAt.Add(time.Second)
					}
					return now
				})
			}
			if err := os.WriteFile(script, []byte(command+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.m.RecoverCheckpointCompletion(t.Context(), f.n, "local-dev"); err == nil {
				t.Fatal("accepted mutation during validation")
			}
			st, err := f.m.GetCurrentState(t.Context(), f.n)
			if err != nil {
				t.Fatal(err)
			}
			if st.HeadSHA != f.a || !strings.HasPrefix(f.git(f.m.repoRoot, "ls-remote", "origin", "refs/heads/"+f.branch), f.a) {
				t.Fatal("race published checkpoint")
			}
		})
	}
}

type completionArtifactRaceStore struct {
	*coordination.GitStore
	after func()
}

func (s *completionArtifactRaceStore) CompareAndSwapAndReconcileCheckpoint(ctx context.Context, n int, rev string, raw []byte, branch, candidate, expected string) (string, error) {
	next, err := s.GitStore.CompareAndSwapAndReconcileCheckpoint(ctx, n, rev, raw, branch, candidate, expected)
	if err == nil {
		s.after()
	}
	return next, err
}
func TestCheckpointCompletionArtifactCASRechecks(t *testing.T) {
	for _, kind := range []string{"approval", "claim", "candidate", "report"} {
		t.Run(kind, func(t *testing.T) {
			f := completionFixture(t, false)
			store := &completionArtifactRaceStore{GitStore: coordination.NewGitStore(f.m.repoRoot)}
			store.after = func() {
				switch kind {
				case "candidate":
					f.git(f.path, "commit", "--allow-empty", "-m", "raced")
				case "report":
					f.write(f.path, ".swarm/active/IMPLEMENTATION.md", "raced")
				default:
					_, err := f.m.mutate(t.Context(), f.n, "local-dev", func(st *IssueState) error {
						if kind == "claim" {
							st.Lease.ClaimID = "successor"
						} else {
							st.PolicyTreeDigest = "changed"
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			f.m.SetStore(f.m.repoRoot, store)
			if _, err := f.m.RecoverCheckpointCompletion(t.Context(), f.n, "local-dev"); err == nil {
				t.Fatal("artifact trusted after race")
			}
			st, err := f.m.GetCurrentState(t.Context(), f.n)
			if err != nil {
				t.Fatal(err)
			}
			tree, _ := f.m.implementationTree(t.Context(), f.b)
			if VerifyTrustedImplementation(st, f.b, tree, f.branch) == nil {
				t.Fatal("stale completion gained trust")
			}
			if c, err := f.m.InspectCheckpointCompletion(t.Context(), f.n); err != nil || c == nil {
				t.Fatal("rejected receipt lost", err)
			}
		})
	}
}

func TestCheckpointCompletionDoesNotMintIncompleteOutcome(t *testing.T) {
	for _, kind := range []string{"fail", "missing-report", "empty-report", "unreadable-report", "missing-predecessor"} {
		t.Run(kind, func(t *testing.T) {
			f := completionFixture(t, true)
			c, err := f.m.InspectCheckpointCompletion(t.Context(), f.n)
			if err != nil {
				t.Fatal(err)
			}
			root, _, err := f.m.completionRoot(t.Context(), false)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err = root.Remove(completionName(f.n)); err != nil {
				t.Fatal(err)
			}
			report := filepath.Join(f.path, ".swarm/active/IMPLEMENTATION.md")
			switch kind {
			case "fail":
				c.Artifact.Verdict = "FAIL"
			case "missing-report":
				if err = os.Remove(report); err != nil {
					t.Fatal(err)
				}
			case "empty-report":
				f.write(f.path, ".swarm/active/IMPLEMENTATION.md", "")
			case "unreadable-report":
				if err = os.Remove(report); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(report, 0700); err != nil {
					t.Fatal(err)
				}
			case "missing-predecessor":
				c.Predecessor = ""
			}
			if err = f.m.SaveCheckpointCompletion(t.Context(), f.n, "local-dev", c.Predecessor, c.Candidate, c.Artifact); err == nil {
				t.Fatal("minted incomplete outcome")
			}
			if receipt, err := f.m.InspectCheckpointCompletion(t.Context(), f.n); err != nil || receipt != nil {
				t.Fatal("receipt minted", err)
			}
		})
	}
}

func TestCheckpointCompletionFirstCheckpoint(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, scenario := range []string{"recover", "recorded-head", "unrelated", "successor", "save-unrelated"} {
			t.Run(fmt.Sprintf("local=%v/%s", local, scenario), func(t *testing.T) {
				f := completionFixture(t, local)
				c, err := f.m.InspectCheckpointCompletion(t.Context(), f.n)
				if err != nil {
					t.Fatal(err)
				}
				root, _, err := f.m.completionRoot(t.Context(), false)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				if err = root.Remove(completionName(f.n)); err != nil {
					t.Fatal(err)
				}
				_, err = f.m.mutate(t.Context(), f.n, "local-dev", func(st *IssueState) error { st.HeadSHA = ""; return nil })
				if err != nil {
					t.Fatal(err)
				}
				st, err := f.m.GetCurrentState(t.Context(), f.n)
				if err != nil {
					t.Fatal(err)
				}
				if !local {
					f.git(f.m.repoRoot, "push", "origin", ":refs/heads/"+f.branch)
				}
				unrelated := func() {
					tree := f.git(f.path, "rev-parse", "HEAD^{tree}")
					head := f.git(f.path, "commit-tree", tree, "-m", "unrelated root")
					f.git(f.path, "reset", "--hard", head)
					c.Candidate = head
					c.Tree = tree
					c.Artifact.Metadata["head_sha"] = head
				}
				if scenario == "save-unrelated" {
					unrelated()
				}
				err = f.m.SaveCheckpointCompletion(t.Context(), f.n, "local-dev", st.BaseSHA, c.Candidate, c.Artifact)
				if scenario == "save-unrelated" {
					if err == nil {
						t.Fatal("saved unrelated candidate")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				c, err = f.m.InspectCheckpointCompletion(t.Context(), f.n)
				if err != nil || !c.NoRecordedHead || c.Predecessor != st.BaseSHA {
					t.Fatalf("first receipt: %+v %v", c, err)
				}
				switch scenario {
				case "recorded-head", "successor":
					_, err = f.m.mutate(t.Context(), f.n, "local-dev", func(st *IssueState) error {
						if scenario == "recorded-head" {
							st.HeadSHA = st.BaseSHA
						} else {
							st.Lease.ClaimID = "successor"
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				case "unrelated":
					unrelated()
					rewriteCompletion(t, f, c)
				}
				if scenario != "recover" {
					assertCompletionRejected(t, f)

					return
				}
				result, err := f.m.RecoverCheckpointCompletion(t.Context(), f.n, "local-dev")
				if err != nil {
					t.Fatal(err)
				}
				if err = VerifyTrustedImplementation(result, c.Candidate, c.Tree, c.Branch); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestPartialImplementationCheckpointCompletion(t *testing.T) {
	f := completionFixture(t, true)
	c, err := f.m.InspectCheckpointCompletion(t.Context(), f.n)
	if err != nil || c == nil {
		t.Fatalf("PASS control: %+v %v", c, err)
	}
	c.Artifact.Verdict = "FAIL"
	c.Artifact.Metadata["implementation_outcome"] = "partial"
	// Retire the valid control before testing save refusal independently.
	root, _, err := f.m.completionRoot(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = root.Remove(completionName(f.n)); err != nil {
		t.Fatal(err)
	}
	root.Close()
	if err = f.m.SaveCheckpointCompletion(t.Context(), f.n, "local-dev", f.a, f.b, c.Artifact); err == nil {
		t.Fatal("saved partial completion")
	}
	if saved, e := f.m.InspectCheckpointCompletion(t.Context(), f.n); e != nil || saved != nil {
		t.Fatalf("stored partial: %+v %v", saved, e)
	}
	// Inject a correctly signed completion to exercise recovery's independent checks.
	rewriteCompletion(t, f, c)
	assertCompletionRejected(t, f)
}

// Re-approval on a new base can retire a checkpoint the base already
// contains. The first new implementation must still preserve its completion.
func TestCheckpointCompletionAfterDroppedCheckpoint(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, scenario := range []string{"recover", "pending-not-contained", "recorded-head"} {
			t.Run(fmt.Sprintf("local=%v/%s", local, scenario), func(t *testing.T) {
				f := completionFixture(t, local)
				c, err := f.m.InspectCheckpointCompletion(t.Context(), f.n)
				if err != nil {
					t.Fatal(err)
				}
				root, _, err := f.m.completionRoot(t.Context(), false)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				if err = root.Remove(completionName(f.n)); err != nil {
					t.Fatal(err)
				}
				pending := ""
				if scenario == "pending-not-contained" {
					tree := f.git(f.path, "rev-parse", "HEAD^{tree}")
					pending = f.git(f.path, "commit-tree", tree, "-m", "outside the base")
				}
				_, err = f.m.mutate(t.Context(), f.n, "local-dev", func(st *IssueState) error {
					head := st.BaseSHA
					if pending != "" {
						head = pending
					}
					st.DroppedCheckpoint = &workspace.DroppedCheckpoint{Branch: st.Branch, HeadSHA: head}
					st.CheckpointDropped = true
					st.Branch, st.HeadSHA = "", ""
					if scenario == "recorded-head" {
						st.HeadSHA = st.BaseSHA
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				st, err := f.m.GetCurrentState(t.Context(), f.n)
				if err != nil {
					t.Fatal(err)
				}
				err = f.m.SaveCheckpointCompletion(t.Context(), f.n, "local-dev", checkpointPredecessor(st), c.Candidate, c.Artifact)
				if scenario != "recover" {
					if err == nil {
						t.Fatal("saved a completion that does not match the pending checkpoint recovery")
					}
					return
				}
				if err != nil {
					t.Fatalf("completion after a dropped checkpoint: %v", err)
				}
				saved, err := f.m.InspectCheckpointCompletion(t.Context(), f.n)
				if err != nil || saved.Branch != f.branch || !saved.NoRecordedHead {
					t.Fatalf("completion must name the pending branch: %+v %v", saved, err)
				}
				// As in issue work: record the checkpoint, then settle the completion.
				if !local {
					f.git(f.m.repoRoot, "push", "--force", "origin", st.DroppedCheckpoint.HeadSHA+":refs/heads/"+f.branch)
				}
				if _, err = f.m.RecordCheckpoint(t.Context(), f.n, "local-dev", f.branch, c.Candidate); err != nil {
					t.Fatal(err)
				}
				result, err := f.m.RecoverCheckpointCompletion(t.Context(), f.n, "local-dev")
				if err != nil {
					t.Fatal(err)
				}
				if result.DroppedCheckpoint != nil || result.Branch != f.branch || result.HeadSHA != c.Candidate {
					t.Fatalf("checkpoint not recorded on the pending branch: %+v", result)
				}
				if err = VerifyTrustedImplementation(result, c.Candidate, c.Tree, c.Branch); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestCheckpointCompletionArchive(t *testing.T) {
	t.Run("rejections", func(t *testing.T) {
		for _, scenario := range []string{"active", "expired", "wrong selector", "unauthorized", "gateway", "fork", "pr", "cleanup", "corrupt", "missing candidate", "bad ref", "archive failure"} {
			t.Run(scenario, func(t *testing.T) {
				f := completionFixture(t, true)
				ctx := t.Context()
				c, err := f.m.InspectCheckpointCompletion(ctx, f.n)
				if err != nil {
					t.Fatal(err)
				}
				root, _, err := f.m.completionRoot(ctx, false)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				selector := c.receiptID
				ref := fmt.Sprintf("refs/swarm/local-completions/issue-%d/%s", f.n, f.b)
				archive := fmt.Sprintf("archive-issue-%d-%s.json", f.n, selector)
				m := f.m
				want := ""
				if scenario != "active" && scenario != "expired" {
					if _, err = f.m.Release(ctx, f.n, "local-dev", "end original claim"); err != nil {
						t.Fatal(err)
					}
				}
				switch scenario {
				case "active":
					want = "swarm issue release"
					f.m.BindClaim(f.n, "old-caller-token")
					var changed *CompletionInputsChangedError
					if _, err := f.m.RecoverCheckpointCompletion(ctx, f.n, "local-dev"); !errors.Is(err, ErrClaimFence) || errors.As(err, &changed) {
						t.Fatalf("wrong token against current receipt: %v", err)
					}
					f.m.BindClaim(f.n, c.Claim)
					if _, err := f.m.mutate(ctx, f.n, "local-dev", func(st *IssueState) error {
						st.ApprovedBy = "another-maintainer"
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				case "expired":
					want = "establish a successor claim"
					expired := f.state().Lease.ExpiresAt.Add(time.Second)
					f.m.SetNowFunc(func() time.Time { return expired })
				case "wrong selector":
					want = "no matching pending"
					selector = strings.Repeat("0", 64)
				case "unauthorized":
					want = "not authorized"
					m = NewManager(f.tr.WithUser("outsider", "NONE"), f.m.security)
					m.SetStore(f.m.repoRoot, f.m.Store())
				case "gateway":
					want = "original direct coordinator checkout"
					m = NewManager(f.tr, f.m.security)
					m.SetStore(f.m.repoRoot, f.m.Store())
					m.SetWorkerGateway(refusingGateway{t: t})
				case "fork", "pr", "cleanup":
					raw, rev, err := f.m.Store().Load(ctx, f.n)
					if err != nil {
						t.Fatal(err)
					}
					var st IssueState
					if err = json.Unmarshal(raw, &st); err != nil {
						t.Fatal(err)
					}
					switch scenario {
					case "fork":
						st.ForkRepo = "elsewhere/repo"
					case "pr":
						st.SubmittedHeadSHA = f.a
					case "cleanup":
						st.Cleanup = &CleanupReservation{OperationID: "reserved"}
					}
					raw, err = json.Marshal(st)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = f.m.Store().CompareAndSwap(ctx, f.n, rev, raw); err != nil {
						t.Fatal(err)
					}
					want = "cleanup reservation"
				case "corrupt":
					want = "invalid completion envelope"
					err = root.WriteFile(completionName(f.n), []byte("{"), 0600)
				case "missing candidate":
					want = "unavailable as a local commit"
					c.Candidate = strings.Repeat("a", 40)
					rewriteCompletion(t, f, c)
					c, err = f.m.InspectCheckpointCompletion(ctx, f.n)
					if err == nil {
						selector = c.receiptID
					}
				case "bad ref":
					want = "could not pin"
					f.git(f.m.repoRoot, "update-ref", ref, f.a)
				case "archive failure":
					want = "existing completion archive differs"
					err = root.WriteFile(archive, []byte("different contents"), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				raw, err := root.ReadFile(completionName(f.n), completionLimit)
				if err != nil {
					t.Fatal(err)
				}
				_, beforeRev, err := m.Store().Load(ctx, f.n)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := m.ArchiveCheckpointCompletion(ctx, f.n, selector); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("wanted %q, got %v", want, err)
				}
				after, err := root.ReadFile(completionName(f.n), completionLimit)
				if err != nil || string(after) != string(raw) {
					t.Fatalf("rejection changed pending receipt: %v", err)
				}
				if _, rev, err := m.Store().Load(ctx, f.n); err != nil || rev != beforeRev {
					t.Fatalf("rejection changed state: %v", err)
				}
				if scenario == "archive failure" {
					if pinned := f.git(f.m.repoRoot, "rev-parse", ref); pinned != f.b {
						t.Fatalf("partial operation lost candidate: %s", pinned)
					}
					if err := root.WriteFileDurable(archive, raw, 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := m.ArchiveCheckpointCompletion(ctx, f.n, selector); err != nil {
						t.Fatalf("retry with existing pin and matching archive: %v", err)
					}
				}
			})
		}
	})
	for _, scenario := range []string{"released", "revoked", "successor-first-checkpoint", "successor-recorded"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			first := scenario == "successor-first-checkpoint"
			recorded := scenario == "successor-recorded"
			f := completionFixture(t, false)
			root, _, err := f.m.completionRoot(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			c, err := f.m.InspectCheckpointCompletion(ctx, f.n)
			if err != nil {
				t.Fatal(err)
			}
			if first {
				if err := root.Remove(completionName(f.n)); err != nil {
					t.Fatal(err)
				}
				if _, err := f.m.mutate(ctx, f.n, "local-dev", func(st *IssueState) error { st.HeadSHA = ""; return nil }); err != nil {
					t.Fatal(err)
				}
				f.git(f.m.repoRoot, "push", "origin", ":refs/heads/"+f.branch)
				if err := f.m.SaveCheckpointCompletion(ctx, f.n, "local-dev", f.base, f.b, c.Artifact); err != nil {
					t.Fatal(err)
				}
				c, err = f.m.InspectCheckpointCompletion(ctx, f.n)
				if err != nil || c == nil || !c.NoRecordedHead {
					t.Fatalf("first-checkpoint receipt: %+v %v", c, err)
				}
			}
			if recorded {
				if _, err := f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "revoked" {
				f.m.tracker = &revokeTracker{LocalTracker: f.tr}
				if _, projectionErr, err := f.m.Revoke(ctx, f.n, "end original claim"); err != nil || projectionErr != nil {
					t.Fatalf("revoke: %v %v", err, projectionErr)
				}
			} else if _, err := f.m.Release(ctx, f.n, "local-dev", "end original claim"); err != nil {
				t.Fatal(err)
			}
			m := NewManager(f.tr, f.m.security)
			m.SetStore(f.m.repoRoot, coordination.NewGitStore(f.m.repoRoot))
			if first || recorded {
				st, err := f.m.Acquire(ctx, f.n, "local-dev", "mock", "implement", time.Hour, 1)
				if err != nil || st.Lease.ClaimID == c.Claim {
					t.Fatalf("acquire successor: %v", err)
				}
				m.BindClaim(f.n, st.Lease.ClaimID)
			}
			raw, err := root.ReadFile(completionName(f.n), completionLimit)
			if err != nil {
				t.Fatal(err)
			}
			state := f.state()
			remote := f.git(f.remote, "for-each-ref", "refs/heads/"+f.branch)
			report, err := os.ReadFile(filepath.Join(f.path, ".swarm/active/IMPLEMENTATION.md"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := m.ArchiveCheckpointCompletion(ctx, f.n, c.receiptID)
			if err != nil || out.Candidate != f.b || out.AlreadyRecorded != recorded {
				t.Fatalf("archive: %+v %v", out, err)
			}
			if f.git(f.m.repoRoot, "rev-parse", out.RetentionRef) != f.b ||
				f.git(f.path, "rev-parse", "HEAD") != f.b || f.git(f.path, "status", "--porcelain") != "" ||
				f.git(f.m.repoRoot, "rev-parse", "refs/heads/"+f.branch) != f.b ||
				f.git(f.remote, "for-each-ref", "refs/heads/"+f.branch) != remote || !reflect.DeepEqual(state, f.state()) {
				t.Fatal("archive changed state, refs, or checkout")
			}
			archived, err := root.ReadFile(fmt.Sprintf("archive-issue-%d-%s.json", f.n, c.receiptID), completionLimit)
			if err != nil || string(archived) != string(raw) {
				t.Fatalf("archive bytes differ: %v", err)
			}
			afterReport, err := os.ReadFile(filepath.Join(f.path, ".swarm/active/IMPLEMENTATION.md"))
			if err != nil || string(afterReport) != string(report) {
				t.Fatalf("implementation report changed: %v", err)
			}
			if found, err := m.InspectCheckpointCompletion(ctx, f.n); err != nil || found != nil {
				t.Fatalf("pending completion survived: %+v %v", found, err)
			}
			if _, err := m.ArchiveCheckpointCompletion(ctx, f.n, c.receiptID); !errors.Is(err, ErrClaimFence) {
				t.Fatalf("archive replay: %v", err)
			}
			if first {
				m.BindClaim(f.n, c.Claim)
				if _, err := m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.base, f.b, false); !errors.Is(err, ErrClaimFence) {
					t.Fatalf("stale original claim could reconcile: %v", err)
				}
				m.BindClaim(f.n, state.Lease.ClaimID)
				f.review("freshly reviewed predecessor and candidate")
				preview, err := m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.base, f.b, true)
				if err != nil || !preview.ValidationRequired || preview.ReviewRequired {
					t.Fatalf("reconciliation preview: %+v %v", preview, err)
				}
				result, err := m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.base, f.b, false)
				if err != nil || result.State.HeadSHA != f.b || result.State.Lease.ClaimID == c.Claim {
					t.Fatalf("explicit reconcile: %+v %v", result, err)
				}
				tree, err := m.implementationTree(ctx, f.b)
				if err != nil || tree != c.Tree || VerifyTrustedImplementation(result.State, f.b, tree, f.branch) == nil {
					t.Fatalf("reconciliation changed the preserved tree or trusted stale implementation: %v", err)
				}
				for _, art := range result.State.Artifacts {
					if art.Type == contract.ArtifactValidation && art.Verdict == "PASS" && art.Metadata["head_sha"] == f.b && art.Metadata["validation_trusted_claim"] == result.State.Lease.ClaimID {
						return
					}
				}
				t.Fatal("fresh candidate validation missing")
			}
			if recorded {
				replacement := *c
				replacement.Claim = state.Lease.ClaimID
				rewriteCompletion(t, f, &replacement)
				if _, err := m.ArchiveCheckpointCompletion(ctx, f.n, c.receiptID); !errors.Is(err, ErrClaimFence) || !strings.Contains(err.Error(), "no matching pending") {
					t.Fatalf("old selector matched a replacement: %v", err)
				}
				if found, err := m.InspectCheckpointCompletion(ctx, f.n); err != nil || found == nil || found.Claim != replacement.Claim {
					t.Fatalf("old selector retired another receipt: %+v %v", found, err)
				}
			}
		})
	}
}
