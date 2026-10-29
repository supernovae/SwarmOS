package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"gopkg.in/yaml.v3"
)

type checkpointCLITracker struct{ tracker.Tracker }

func (checkpointCLITracker) BackendName() string { return "github-checkpoint-fixture" }

func TestCLICheckpointReconciliationAndStatus(t *testing.T) {
	// Serial: checkpoint reconciliation validates through internal/lease,
	// which reads SWARM_EXECUTION_MODE from the process environment.
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	ctx := context.Background()
	number := strconv.Itoa(n)
	remote := filepath.Join(t.TempDir(), "remote.git")
	prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
	prGit(t, f.root, "remote", "add", "origin", remote)
	prGit(t, f.root, "push", "origin", "main")
	tr := checkpointCLITracker{f.tracker}
	cfg, err := os.ReadFile(filepath.Join(f.root, ".swarm/config.yaml"))
	if err != nil || len(cfg) == 0 {
		t.Fatal(err)
	}
	f.manager.SetStore(f.root, trustedLedgerStore(t, f.root, ""))
	if _, err = f.manager.ApproveTask(ctx, n, "alice"); err != nil {
		t.Fatal(err)
	}
	st, err := f.manager.Acquire(ctx, n, "alice", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.SaveClaimToken(f.root, n, st.Lease.ClaimID, st.BaseSHA); err != nil {
		t.Fatal(err)
	}
	path, branch, err := workspace.ProvisionAt(f.root, n, "test", st.BaseSHA, st.Branch, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "feature.txt"), []byte("feature"), 0644); err != nil {
		t.Fatal(err)
	}
	prGit(t, path, "add", "feature.txt")
	prGit(t, path, "commit", "-m", "A")
	a := prGit(t, path, "rev-parse", "HEAD")
	for _, art := range []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan", Producer: "mock", Summary: "plan"}, {Type: contract.ArtifactTestPlan, Body: "tests", Producer: "mock", Summary: "tests"}} {
		if _, err = f.manager.UpdateArtifact(ctx, n, "alice", art); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.manager.RecordHumanReview(ctx, n, "APPROVED", "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.RecordCheckpoint(ctx, n, "alice", branch, a); err != nil {
		t.Fatal(err)
	}
	prGit(t, path, "commit", "--amend", "-m", "B")
	b := prGit(t, path, "rev-parse", "HEAD")
	run := func(args ...string) (string, error) {
		opts := &RootOptions{RepoRoot: f.root, Repo: "test/repo", Backend: "github", trackerOverride: tr}
		cmd := f.command(opts)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(ctx)
		return out.String(), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("swarm %v: %v %s", args, err, out)
		}
		return out
	}
	raw := must("issue", "status", number, "--json")
	var status struct {
		Issues []struct {
			Checkpoint *workspace.CheckpointState `json:"checkpoint"`
		} `json:"issues"`
	}
	if err = json.Unmarshal([]byte(raw), &status); err != nil || len(status.Issues) != 1 || status.Issues[0].Checkpoint.Worktree.SHA != b || status.Issues[0].Checkpoint.Recorded.SHA != a {
		t.Fatalf("status omitted divergence: %s %v", raw, err)
	}
	text := must("issue", "status", number)
	if !strings.Contains(text, a) || !strings.Contains(text, b) {
		t.Fatal("text status omits exact heads")
	}
	args := []string{"issue", "reconcile", number, "--checkpoint", "--expected-head", a, "--candidate-head", b}
	refs := prGit(t, remote, "show-ref")
	must(append(append([]string{}, args...), "--dry-run", "--json")...)
	if prGit(t, remote, "show-ref") != refs {
		t.Fatal("dry run changed refs")
	}
	must(append(append([]string{}, args...), "--json")...)
	current, err := f.manager.GetCurrentState(ctx, n)
	if err != nil || current.HeadSHA != b {
		t.Fatalf("not reconciled: %+v %v", current, err)
	}
	if _, _, err = workspace.ProvisionAt(f.root, n, "test", st.BaseSHA, branch, b); err != nil {
		t.Fatal(err)
	}
	if prGit(t, remote, "rev-parse", f.manager.LedgerLayout().CheckpointRef(n, a)) != a {
		t.Fatal("lost predecessor")
	}
	// Missing local worktrees do not hide authoritative status.
	prGit(t, f.root, "worktree", "remove", path)
	raw = must("issue", "status", number, "--json")
	if !strings.Contains(raw, `"missing":true`) || !strings.Contains(raw, b) {
		t.Fatal("missing worktree hides state")
	}
}

func TestCheckpointReconcileFlags(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	help := f.must(t, "issue", "reconcile", "--help")
	for _, flag := range []string{"--checkpoint", "--expected-head", "--candidate-head", "--dry-run"} {
		if !strings.Contains(help, flag) {
			t.Fatalf("missing flag %s", flag)
		}
	}
	for _, flags := range [][]string{{"--dry-run"}, {"--expected-head", strings.Repeat("a", 40)}, {"--checkpoint"}, {"--candidate-head", strings.Repeat("b", 40)}} {
		if _, err := f.run(append([]string{"issue", "reconcile", "1"}, flags...)...); err == nil {
			t.Fatalf("accepted invalid flags %v", flags)
		}
	}
}

// Both backends use normal CLI approval, claim and release transitions. The
// GitHub case uses the real tracker HTTP client and a real remote ledger.
type checkpointTokenFailureTracker struct {
	tracker.Tracker
	afterClaim func()
}

func (t *checkpointTokenFailureTracker) AddLabels(ctx context.Context, n int, labels []string) error {
	err := t.Tracker.AddLabels(ctx, n, labels)
	if err == nil && t.afterClaim != nil && slices.Contains(labels, "swarm:claimed") {
		hook := t.afterClaim
		t.afterClaim = nil
		hook()
	}
	return err
}

func TestCheckpointReleasedReapprovedRecovery(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) {
			f := newCLIFixture(t)
			cfgPath := filepath.Join(f.root, ".swarm/config.yaml")
			cfg, err := harness.LoadConfig(f.root)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Agents["mock"] = contract.AgentConfig{Binary: "true"}
			rawCfg, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(cfgPath, rawCfg, 0644); err != nil {
				t.Fatal(err)
			}
			prGit(t, f.root, "add", ".swarm/config.yaml")
			prGit(t, f.root, "commit", "-m", "allow fixture mock")
			developerCalls := filepath.Join(t.TempDir(), "developer-calls")
			developer := filepath.Join(t.TempDir(), "developer")
			if err = testutil.WriteExecutable(developer, []byte("#!/bin/sh\nprintf invoked >> "+strconv.Quote(developerCalls)+"\nexit 73\n"), 0700); err != nil {
				t.Fatal(err)
			}
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", strconv.Quote(developer))
			f.setenv("SWARM_EXECUTION_MODE", "trusted-local")
			n := f.create(t, "none")
			number := strconv.Itoa(n)
			ctx := context.Background()
			var tr tracker.Tracker = f.tracker
			var fake *fakeGitHubIssues
			if backend == "github" {
				issue, err := f.tracker.GetIssue(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				fake, tr = newFakeGitHubTracker(t, issue)
				addTestOrigin(t, f)
				cfg, err := harness.LoadConfig(f.root)
				if err != nil {
					t.Fatal(err)
				}
				f.manager = lease.NewManager(tr, cfg.Security)
				if err = f.manager.ConfigureStore(f.root); err != nil {
					t.Fatal(err)
				}
			}
			controlled := &checkpointTokenFailureTracker{Tracker: tr}
			tr = controlled
			run := func(args ...string) (string, error) {
				opts := &RootOptions{RepoRoot: f.root, Repo: "octo/swarm", Backend: backend, trackerOverride: tr}
				cmd := f.command(opts)
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				cmd.SetArgs(args)
				err := cmd.ExecuteContext(ctx)
				return out.String(), err
			}
			must := func(args ...string) string {
				t.Helper()
				out, err := run(args...)
				if err != nil {
					t.Fatalf("swarm %v: %v %s", args, err, out)
				}
				return out
			}
			state := func() *lease.IssueState {
				t.Helper()
				st, err := f.manager.GetCurrentState(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			must("issue", "approve", number)
			must("issue", "claim", number, "--agent", "mock")
			st := state()
			path := workspace.WorktreePath(f.root, n)
			if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("preserved\n"), 0644); err != nil {
				t.Fatal(err)
			}
			prGit(t, path, "add", "feature.txt")
			prGit(t, path, "commit", "-m", "preserved predecessor")
			predecessor := prGit(t, path, "rev-parse", "HEAD")
			f.manager.BindClaim(n, st.Lease.ClaimID)
			if _, err := f.manager.RecordCheckpoint(ctx, n, "alice", st.Branch, predecessor); err != nil {
				t.Fatal(err)
			}
			must("issue", "release", number)
			prGit(t, f.root, "commit", "--allow-empty", "-m", "new approved base")
			base := prGit(t, f.root, "rev-parse", "HEAD")
			if backend == "github" {
				prGit(t, f.root, "push", "origin", "main")
			}
			issue, err := tr.GetIssue(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tr.UpdateIssueBody(ctx, n, strings.Replace(issue.Body, "Exercise the lifecycle", "Exercise the amended lifecycle", 1)); err != nil {
				t.Fatal(err)
			}
			must("issue", "approve", number)
			prGit(t, path, "merge", "--no-edit", base)
			candidate := prGit(t, path, "rev-parse", "HEAD")
			if _, err = run("issue", "work", number, "--agent", "mock", "--headless"); err == nil {
				t.Fatal("work accepted divergent candidate")
			}
			if _, err = run("issue", "claim", number, "--agent", "mock"); err == nil {
				t.Fatal("claim accepted divergent candidate")
			}
			current := state()
			if current.Lease.Status == "active" || current.HeadSHA != predecessor || prGit(t, path, "rev-parse", "HEAD") != candidate {
				t.Fatalf("recovery changed work or retained active claim: %+v", current)
			}
			args := []string{"issue", "reconcile", number, "--checkpoint", "--expected-head", predecessor, "--candidate-head", candidate}
			// Invalid observations must not acquire, replace tokens, or mutate either
			// backend, even when no current claim exists.
			for _, scenario := range []string{"stale expected", "short SHA", "wrong candidate", "untracked", "tracked", "staged", "detached", "wrong branch", "successor"} {
				t.Run(scenario, func(t *testing.T) {
					invalid := append([]string{}, args...)
					switch scenario {
					case "stale expected":
						invalid[5] = base
					case "short SHA":
						invalid[5] = predecessor[:12]
					case "wrong candidate":
						invalid[7] = predecessor
					case "untracked":
						if err := os.WriteFile(filepath.Join(path, "untracked"), []byte("keep"), 0644); err != nil {
							t.Fatal(err)
						}
						defer os.Remove(filepath.Join(path, "untracked"))
					case "tracked", "staged":
						if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("dirty\n"), 0644); err != nil {
							t.Fatal(err)
						}
						if scenario == "staged" {
							prGit(t, path, "add", "feature.txt")
						}
						defer func() {
							if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("preserved\n"), 0644); err != nil {
								t.Fatal(err)
							}
							prGit(t, path, "add", "feature.txt")
						}()
					case "detached":
						prGit(t, path, "checkout", "--detach")
						defer prGit(t, path, "checkout", st.Branch)
					case "wrong branch":
						prGit(t, path, "checkout", "-b", "other-fixture-branch")
						defer prGit(t, path, "checkout", st.Branch)
					case "successor":
						successor, err := f.manager.Acquire(ctx, n, "alice", "mock", "plan", time.Hour, 1)
						if err != nil {
							t.Fatal(err)
						}
						defer func() {
							f.manager.BindClaim(n, successor.Lease.ClaimID)
							if _, err := f.manager.Release(ctx, n, "alice", "fixture successor done"); err != nil {
								t.Fatal(err)
							}
						}()
					}
					before := checkpointSnapshot(t, f.root)
					token, tokenErr := lease.LoadClaimToken(f.root, n)
					for _, dry := range []bool{true, false} {
						invocation := append([]string{}, invalid...)
						if dry {
							invocation = append(invocation, "--dry-run")
						}
						if out, err := run(invocation...); err == nil {
							t.Fatalf("accepted invalid precondition: %s", out)
						}
					}
					// Actual mode may create its machine claim lock; those bytes are covered
					// separately by the repeated preview snapshot below.
					if !reflect.DeepEqual(before, checkpointSnapshot(t, f.root)) {
						t.Fatal("refusal wrote repository bytes")
					}
					afterToken, err := lease.LoadClaimToken(f.root, n)
					if afterToken != token || (err == nil) != (tokenErr == nil) {
						t.Fatal("refusal replaced token")
					}
				})
			}
			// Read the latest released state after exercising a successor, without
			// changing the original exact predecessor/candidate expectation.
			current = state()
			before := checkpointSnapshot(t, f.root)
			requests := 0
			if fake != nil {
				requests = len(fake.requests)
			}
			for range 2 {
				raw := must(append(append([]string{}, args...), "--dry-run", "--json")...)
				if !strings.Contains(raw, `"claim_required":true`) || !strings.Contains(raw, `"validation_required":true`) {
					t.Fatalf("missing admission report: %s", raw)
				}
			}
			if !reflect.DeepEqual(before, checkpointSnapshot(t, f.root)) {
				t.Fatal("preview wrote repository bytes")
			}
			if fake != nil {
				for _, req := range fake.requests[requests:] {
					if !strings.HasPrefix(req, "GET ") {
						t.Fatalf("preview mutated API: %s", req)
					}
				}
			}
			// Preview also works without a persisted token and leaves its control
			// directory byte-identical, including the absence of that file.
			historicalToken, err := lease.LoadClaimToken(f.root, n)
			if err != nil {
				t.Fatal(err)
			}
			historicalBase, err := lease.LoadClaimBaseHint(f.root, n)
			if err != nil {
				t.Fatal(err)
			}
			config, err := os.UserConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			common, err := filepath.EvalSymlinks(filepath.Join(f.root, ".git"))
			if err != nil {
				t.Fatal(err)
			}
			identity := sha256.Sum256([]byte(common))
			tokenPath := filepath.Join(config, "swarm", "claims", fmt.Sprintf("%x", identity), number+".json")
			if err = os.Remove(tokenPath); err != nil {
				t.Fatal(err)
			}
			absent := checkpointSnapshot(t, f.root)
			must(append(append([]string{}, args...), "--dry-run")...)
			if !reflect.DeepEqual(absent, checkpointSnapshot(t, f.root)) {
				t.Fatal("absent-token preview wrote control files")
			}
			if err = lease.SaveClaimToken(f.root, n, historicalToken, historicalBase); err != nil {
				t.Fatal(err)
			}
			// Fail only token persistence, after normal acquisition has returned its
			// fenced claim. A directory cannot be atomically replaced by the token file.
			backup := tokenPath + ".fixture-backup"
			controlled.afterClaim = func() {
				if err := os.Rename(tokenPath, backup); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(tokenPath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := run(append(append([]string{}, args...), "--json")...); err == nil {
				t.Fatalf("token setup unexpectedly succeeded: %s", out)
			}
			if err = os.Remove(tokenPath); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(backup, tokenPath); err != nil {
				t.Fatal(err)
			}
			failedSetup := state()
			if failedSetup.Lease.Status == "active" || failedSetup.HeadSHA != predecessor || prGit(t, path, "rev-parse", "HEAD") != candidate {
				t.Fatal("token setup failure did not release its own claim while preserving work")
			}
			reviewOutput, reviewErr := run(append(append([]string{}, args...), "--json")...)
			if reviewErr == nil {
				t.Fatal("missing review was bypassed")
			}
			if state().Lease.Status != "active" {
				t.Fatalf("pending review lost admitted claim: %v %s", reviewErr, reviewOutput)
			}
			heldToken, err := lease.LoadClaimToken(f.root, n)
			if err != nil {
				t.Fatal(err)
			}
			f.manager.BindClaim(n, heldToken)
			for _, art := range []contract.Artifact{{Type: contract.ArtifactDesign, Body: "recovery plan", Summary: "plan", Producer: "mock"}, {Type: contract.ArtifactTestPlan, Body: "recovery tests", Summary: "tests", Producer: "mock"}} {
				if _, err = f.manager.UpdateArtifact(ctx, n, "alice", art); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = f.manager.RecordHumanReview(ctx, n, "APPROVED", "reviewed recovery"); err != nil {
				t.Fatal(err)
			}
			must(append(append([]string{}, args...), "--json")...)
			recovered := state()
			token, err := lease.LoadClaimToken(f.root, n)
			if err != nil || recovered.HeadSHA != candidate || recovered.Lease.Status != "active" || recovered.Lease.ClaimID == current.Lease.ClaimID || token != recovered.Lease.ClaimID || recovered.BaseSHA != base || recovered.Lease.Owner != "alice" {
				t.Fatalf("incorrect recovered claim: %+v token=%s err=%v", recovered, token, err)
			}
			if recovered.Lease.ExpiresAt.Sub(recovered.Lease.ClaimedAt) > 4*time.Hour {
				t.Fatal("unbounded lease")
			}
			if prGit(t, path, "rev-parse", "HEAD") != candidate || workspace.InspectCheckpoint(ctx, f.root, ".", n, recovered.Branch, candidate).Dirty {
				t.Fatal("candidate changed")
			}
			if _, _, err = workspace.ProvisionAt(f.root, n, "test", base, recovered.Branch, candidate); err != nil {
				t.Fatal(err)
			}
			for _, sha := range []string{predecessor, candidate} {
				if !slices.Contains(recovered.CheckpointHeads, sha) {
					t.Fatal("checkpoint history missing")
				}
				if backend == "github" && prGit(t, f.root, "ls-remote", "origin", f.manager.LedgerLayout().CheckpointRef(n, sha)) == "" {
					t.Fatal("checkpoint retention missing")
				}
			}
			// Continue through ordinary checkpoint recording; no developer harness is
			// invoked by explicit reconciliation.
			prGit(t, path, "commit", "--allow-empty", "-m", "continue recovered work")
			next := prGit(t, path, "rev-parse", "HEAD")
			f.manager.BindClaim(n, token)
			if _, err = f.manager.RecordCheckpoint(ctx, n, "alice", recovered.Branch, next); err != nil {
				t.Fatal(err)
			}
			if backend == "local" {
				// Additive local recovery retains both checkpoints through the merged
				// descendant, including after ordinary merged-branch cleanup and GC.
				prGit(t, f.root, "merge", "--no-edit", next)
				for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
					file := filepath.Join(path, name)
					raw, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					stripped := workspace.StripActivePointer(string(raw))
					if prGit(t, path, "ls-files", "--", name) == "" {
						if strings.TrimSpace(stripped) != "" {
							t.Fatal("untracked instruction content is not disposable")
						}
						if err = os.Remove(file); err != nil {
							t.Fatal(err)
						}
					} else if err = os.WriteFile(file, []byte(stripped), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if err = workspace.RetireCleanupWorktree(ctx, f.root, path, recovered.Branch, next); err != nil {
					t.Fatal(err)
				}
				if err = workspace.DeleteCleanupLocalBranch(ctx, f.root, recovered.Branch, next); err != nil {
					t.Fatal(err)
				}
				prGit(t, f.root, "reflog", "expire", "--expire=now", "--all")
				prGit(t, f.root, "gc", "--prune=now")
				for _, sha := range []string{predecessor, candidate} {
					prGit(t, f.root, "cat-file", "-e", sha+"^{commit}")
				}
			}
			if _, err = os.Stat(developerCalls); !os.IsNotExist(err) {
				t.Fatalf("developer executed during recovery: %v", err)
			}
		})
	}
}

func checkpointSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	capture := func(dir string) {
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				raw, e := os.ReadFile(path)
				if e != nil {
					return e
				}
				files[path] = string(raw)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	capture(root)
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	common, err := filepath.EvalSymlinks(filepath.Join(root, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	id := sha256.Sum256([]byte(common))
	capture(filepath.Join(config, "swarm", "claims", fmt.Sprintf("%x", id)))
	return files
}
