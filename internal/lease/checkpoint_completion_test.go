package lease

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/safefs"
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
