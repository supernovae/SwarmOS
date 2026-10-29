package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
			// The approved policy names only alice and an isolated boundary that is
			// unavailable when selected: native Codex locally, a container on GitHub.
			cfg.Security.ApprovedCollaborators = []string{"alice"}
			codex := cfg.Agents["codex"]
			codex.Binary = "swarm-checkpoint-missing-codex-309"
			cfg.Agents["codex"] = codex
			boundaryErr := "swarm-checkpoint-missing-codex-309"
			if backend == "github" {
				cfg.Execution.Backend = contract.BackendOCI
				cfg.Execution.ContainerRuntime = "podman"
				cfg.Execution.ContainerImage = "registry.invalid/swarm-checkpoint-309:missing"
				boundaryErr = `"podman"`
			}
			rawCfg, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(cfgPath, rawCfg, 0644); err != nil {
				t.Fatal(err)
			}
			// Every approved check run is counted. An armed hook pauses one run
			// inside candidate validation so normal operations can race it.
			hookDir := t.TempDir()
			hook := filepath.Join(hookDir, "check-hook")
			script := "#!/bin/sh\nd=" + strconv.Quote(hookDir) + "\nprintf x >> \"$d/check-calls\"\nif mv \"$d/armed\" \"$d/started\" 2>/dev/null; then\n\ti=0\n\twhile [ ! -e \"$d/proceed\" ]; do\n\t\ti=$((i+1))\n\t\tif [ \"$i\" -gt 1200 ]; then exit 1; fi\n\t\tsleep 0.05\n\tdone\n\trm -f \"$d/proceed\"\nfi\n"
			if err = testutil.WriteExecutable(hook, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			policy := "required_checks:\n  - name: git-available\n    command: git --version\n    timeout: 1m\n  - name: checkpoint-hook\n    command: " + strconv.Quote(hook) + "\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
			if err = os.WriteFile(filepath.Join(f.root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0644); err != nil {
				t.Fatal(err)
			}
			checkCalls := func() string {
				raw, err := os.ReadFile(filepath.Join(hookDir, "check-calls"))
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				return string(raw)
			}
			prGit(t, f.root, "add", ".swarm/config.yaml", ".swarm/validation/required-checks.yaml")
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
			runAs := func(actor tracker.Tracker, args ...string) (string, error) {
				opts := &RootOptions{RepoRoot: f.root, Repo: "octo/swarm", Backend: backend, trackerOverride: actor}
				cmd := f.command(opts)
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				cmd.SetArgs(args)
				err := cmd.ExecuteContext(ctx)
				return out.String(), err
			}
			run := func(args ...string) (string, error) { return runAs(tr, args...) }
			mustIn := func(t *testing.T, args ...string) string {
				t.Helper()
				out, err := run(args...)
				if err != nil {
					t.Fatalf("swarm %v: %v %s", args, err, out)
				}
				return out
			}
			must := func(args ...string) string {
				t.Helper()
				return mustIn(t, args...)
			}
			stateIn := func(t *testing.T) *lease.IssueState {
				t.Helper()
				st, err := f.manager.GetCurrentState(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			state := func() *lease.IssueState {
				t.Helper()
				return stateIn(t)
			}
			// persisted serializes authority as stored; ordinary reads may refresh
			// local Git refs, so callers snapshot repository bytes afterwards.
			persisted := func(t *testing.T) string {
				t.Helper()
				raw, err := json.Marshal(stateIn(t))
				if err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			apiRequests := func() int {
				if fake == nil {
					return 0
				}
				fake.mu.Lock()
				defer fake.mu.Unlock()
				return len(fake.requests)
			}
			requireReadOnlyAPI := func(t *testing.T, since int) {
				t.Helper()
				if fake == nil {
					return
				}
				fake.mu.Lock()
				defer fake.mu.Unlock()
				for _, req := range fake.requests[since:] {
					if !strings.HasPrefix(req, "GET ") {
						t.Fatalf("refusal or preview mutated API: %s", req)
					}
				}
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
			amended := strings.Replace(issue.Body, "Exercise the lifecycle", "Exercise the amended lifecycle", 1)
			if _, err = tr.UpdateIssueBody(ctx, n, amended); err != nil {
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
			scenarios := []string{"stale expected", "short SHA", "non-hex SHA", "uppercase SHA", "unknown candidate", "wrong candidate", "untracked", "tracked", "staged", "detached", "wrong branch",
				"unregistered worktree", "foreign worktree", "symlink worktree", "successor", "unapproved collaborator", "unavailable boundary", "unmet dependency"}
			for _, scenario := range scenarios {
				t.Run(scenario, func(t *testing.T) {
					invalid := append([]string{}, args...)
					actor := tr
					wantErr := ""
					switch scenario {
					case "non-hex SHA":
						invalid[5] = strings.Repeat("g", 40)
						wantErr = "invalid commit SHA"
					case "uppercase SHA":
						if strings.ToUpper(candidate) == candidate {
							t.Skip("candidate has no hexadecimal letters")
						}
						invalid[7] = strings.ToUpper(candidate)
						wantErr = lease.ErrClaimFence.Error()
					case "unknown candidate":
						invalid[7] = "0123456789abcdef0123456789abcdef01234567"
						wantErr = lease.ErrClaimFence.Error()
					case "unregistered worktree", "foreign worktree", "symlink worktree":
						// The registered checkout moves aside; its path then holds an empty
						// directory, an independent clone at the exact candidate, or a link.
						moved := filepath.Join(t.TempDir(), "registered")
						if err := os.Rename(path, moved); err != nil {
							t.Fatal(err)
						}
						defer func() {
							if err := os.RemoveAll(path); err != nil {
								t.Fatal(err)
							}
							if err := os.Rename(moved, path); err != nil {
								t.Fatal(err)
							}
						}()
						switch scenario {
						case "unregistered worktree":
							if err := os.Mkdir(path, 0755); err != nil {
								t.Fatal(err)
							}
						case "foreign worktree":
							prGit(t, f.root, "clone", "--quiet", "--no-local", f.root, path)
							prGit(t, path, "checkout", "--quiet", "-B", st.Branch, candidate)
							if prGit(t, path, "rev-parse", "HEAD") != candidate {
								t.Fatal("foreign clone does not match candidate")
							}
						case "symlink worktree":
							if err := os.Symlink(moved, path); err != nil {
								t.Fatal(err)
							}
						}
						wantErr = "worktree"
					case "unapproved collaborator":
						// Only the approved base allowlist decides; the actor is otherwise
						// authenticated by its tracker.
						if backend == "github" {
							actor = checkpointActorTracker{Tracker: tr, actor: "mallory"}
						} else {
							actor = tracker.NewLocalTracker(f.root).WithUser("mallory", "")
						}
						wantErr = "approved_collaborators"
					case "unavailable boundary":
						// Select the approved isolated boundary on a host without its
						// executables. Git stays available for read-only inspection.
						git, err := exec.LookPath("git")
						if err != nil {
							t.Fatal(err)
						}
						bin := t.TempDir()
						if err = os.Symlink(git, filepath.Join(bin, "git")); err != nil {
							t.Fatal(err)
						}
						t.Setenv("PATH", bin)
						t.Setenv("SWARM_EXECUTION_MODE", "isolated")
						wantErr = boundaryErr
					case "unmet dependency":
						// A normally reapproved WorkUnit depending on an open issue.
						prerequisite, err := tr.CreateIssue(ctx, "Prerequisite", "Unfinished prerequisite.", []string{"prerequisite"})
						if err != nil {
							t.Fatal(err)
						}
						if _, err = tr.UpdateIssueBody(ctx, n, amended+fmt.Sprintf("\n### Depends On\n\n- #%d\n", prerequisite.Number)); err != nil {
							t.Fatal(err)
						}
						mustIn(t, "issue", "approve", number)
						defer func() {
							if _, err := tr.UpdateIssueBody(ctx, n, amended); err != nil {
								t.Fatal(err)
							}
							mustIn(t, "issue", "approve", number)
						}()
						wantErr = "depends_on"
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
					authority := persisted(t)
					requests := apiRequests()
					calls := checkCalls()
					before := checkpointSnapshot(t, f.root)
					token, tokenErr := lease.LoadClaimToken(f.root, n)
					for _, dry := range []bool{true, false} {
						invocation := append([]string{}, invalid...)
						if dry {
							invocation = append(invocation, "--dry-run")
						}
						out, err := runAs(actor, invocation...)
						if err == nil {
							t.Fatalf("accepted invalid precondition: %s", out)
						}
						if !strings.Contains(err.Error(), wantErr) {
							t.Fatalf("dry-run=%t refused for the wrong reason: %v", dry, err)
						}
					}
					// Actual mode may create its machine claim lock; those bytes are covered
					// separately by the repeated preview snapshot below.
					if diff := snapshotDiff(before, checkpointSnapshot(t, f.root)); diff != "" {
						t.Fatalf("refusal wrote repository bytes or metadata: %s", diff)
					}
					afterToken, err := lease.LoadClaimToken(f.root, n)
					if afterToken != token || (err == nil) != (tokenErr == nil) {
						t.Fatal("refusal replaced token")
					}
					requireReadOnlyAPI(t, requests)
					if checkCalls() != calls {
						t.Fatal("refusal executed a required check")
					}
					if persisted(t) != authority {
						t.Fatal("refusal changed authoritative state")
					}
				})
			}
			// Read the latest released state after exercising a successor, without
			// changing the original exact predecessor/candidate expectation.
			current = state()
			authority := persisted(t)
			requests := apiRequests()
			before := checkpointSnapshot(t, f.root)
			for range 2 {
				raw := must(append(append([]string{}, args...), "--dry-run", "--json")...)
				if !strings.Contains(raw, `"claim_required":true`) || !strings.Contains(raw, `"validation_required":true`) {
					t.Fatalf("missing admission report: %s", raw)
				}
			}
			if diff := snapshotDiff(before, checkpointSnapshot(t, f.root)); diff != "" {
				t.Fatalf("preview wrote repository bytes or metadata: %s", diff)
			}
			requireReadOnlyAPI(t, requests)
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
			// Repeat so a deferred write by the first preview is also detected.
			for range 2 {
				must(append(append([]string{}, args...), "--dry-run")...)
				if diff := snapshotDiff(absent, checkpointSnapshot(t, f.root)); diff != "" {
					t.Fatalf("absent-token preview wrote control files: %s", diff)
				}
			}
			requireReadOnlyAPI(t, requests)
			if _, err = os.Lstat(tokenPath); !os.IsNotExist(err) {
				t.Fatalf("absent-token preview created a token: %v", err)
			}
			if persisted(t) != authority {
				t.Fatal("preview changed authoritative state")
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
			recordReview := func(claim string) {
				t.Helper()
				f.manager.BindClaim(n, claim)
				for _, art := range []contract.Artifact{{Type: contract.ArtifactDesign, Body: "recovery plan", Summary: "plan", Producer: "mock"}, {Type: contract.ArtifactTestPlan, Body: "recovery tests", Summary: "tests", Producer: "mock"}} {
					if _, err := f.manager.UpdateArtifact(ctx, n, "alice", art); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.manager.RecordHumanReview(ctx, n, "APPROVED", "reviewed recovery"); err != nil {
					t.Fatal(err)
				}
			}
			recordReview(heldToken)
			leaseJSON := func(st *lease.IssueState) string {
				t.Helper()
				raw, err := json.Marshal(st.Lease)
				if err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			remoteHeads := func() string {
				if backend != "github" {
					return ""
				}
				return prGit(t, f.root, "ls-remote", "origin", "refs/heads/"+st.Branch, f.manager.LedgerLayout().CheckpointRef(n, candidate))
			}
			// raceDuringValidation pauses actual reconciliation inside its approved
			// check run, applies a normal competing operation, then resumes it.
			raceDuringValidation := func(race func()) error {
				t.Helper()
				if err := os.WriteFile(filepath.Join(hookDir, "armed"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					_, err := run(append(append([]string{}, args...), "--json")...)
					done <- err
				}()
				resumed := false
				resume := func() {
					resumed = true
					if err := os.WriteFile(filepath.Join(hookDir, "proceed"), nil, 0600); err != nil {
						t.Error(err)
					}
				}
				defer func() {
					if !resumed {
						resume()
						<-done
					}
				}()
				deadline := time.After(time.Minute)
				for {
					if _, err := os.Stat(filepath.Join(hookDir, "started")); err == nil {
						break
					}
					select {
					case err := <-done:
						resumed = true
						t.Fatalf("reconciliation finished before validation: %v", err)
					case <-deadline:
						t.Fatal("validation did not reach the approved check")
					case <-time.After(20 * time.Millisecond):
					}
				}
				if err := os.Remove(filepath.Join(hookDir, "started")); err != nil {
					t.Fatal(err)
				}
				race()
				resume()
				return <-done
			}
			heads := remoteHeads()

			// A matching live token is reused without renewal; the operator releases
			// it while checks run and a successor claims and persists its token.
			heldLease := leaseJSON(state())
			var successor *lease.IssueState
			var successorAuthority string
			err = raceDuringValidation(func() {
				during := state()
				if during.Lease.ClaimID != heldToken || leaseJSON(during) != heldLease {
					t.Fatalf("reused claim was renewed or replaced: %s != %s", leaseJSON(during), heldLease)
				}
				f.manager.BindClaim(n, heldToken)
				if _, err := f.manager.Release(ctx, n, "alice", "operator released during validation"); err != nil {
					t.Fatal(err)
				}
				var err error
				if successor, err = f.manager.Acquire(ctx, n, "alice", "mock", "plan", time.Hour, 1); err != nil {
					t.Fatal(err)
				}
				if err = lease.SaveClaimToken(f.root, n, successor.Lease.ClaimID, successor.BaseSHA); err != nil {
					t.Fatal(err)
				}
				successorAuthority = persisted(t)
			})
			if !errors.Is(err, lease.ErrClaimFence) || !strings.Contains(err.Error(), "changed during validation") {
				t.Fatalf("successor race not fenced at the validation boundary: %v", err)
			}
			if token, err := lease.LoadClaimToken(f.root, n); err != nil || token != successor.Lease.ClaimID {
				t.Fatalf("successor token overwritten: %s %v", token, err)
			}
			if persisted(t) != successorAuthority || remoteHeads() != heads || prGit(t, path, "rev-parse", "HEAD") != candidate {
				t.Fatal("successor race published, released the successor, or changed work")
			}
			f.manager.BindClaim(n, successor.Lease.ClaimID)
			if _, err = f.manager.Release(ctx, n, "alice", "fixture successor done"); err != nil {
				t.Fatal(err)
			}

			// A released historical token is not authority: reconciliation acquires
			// normally, then the WorkUnit is reapproved while checks run.
			var admitted, reapprovedAuthority string
			err = raceDuringValidation(func() {
				during := state()
				token, err := lease.LoadClaimToken(f.root, n)
				if err != nil || during.Lease.Status != "active" || during.Lease.ClaimID == successor.Lease.ClaimID || token != during.Lease.ClaimID {
					t.Fatalf("validation began without a fresh persisted claim: %+v token=%s %v", during.Lease, token, err)
				}
				admitted = token
				f.manager.BindClaim(n, admitted)
				if _, err = f.manager.Release(ctx, n, "alice", "reapproval during validation"); err != nil {
					t.Fatal(err)
				}
				if _, err = tr.UpdateIssueBody(ctx, n, strings.Replace(amended, "amended lifecycle", "reapproved lifecycle", 1)); err != nil {
					t.Fatal(err)
				}
				if _, err = f.manager.ApproveTask(ctx, n, "alice"); err != nil {
					t.Fatal(err)
				}
				reapprovedAuthority = persisted(t)
			})
			if !errors.Is(err, lease.ErrClaimFence) || !strings.Contains(err.Error(), "changed during validation") {
				t.Fatalf("reapproval race not fenced at the validation boundary: %v", err)
			}
			if token, err := lease.LoadClaimToken(f.root, n); err != nil || token != admitted {
				t.Fatalf("reapproval race replaced token: %s %v", token, err)
			}
			if persisted(t) != reapprovedAuthority || remoteHeads() != heads || prGit(t, path, "rev-parse", "HEAD") != candidate {
				t.Fatal("reapproval race published, reclaimed, or changed work")
			}
			if _, err = tr.UpdateIssueBody(ctx, n, amended); err != nil {
				t.Fatal(err)
			}
			must("issue", "approve", number)

			// An expired claim with its persisted token yields ordinary fresh
			// admission; the changed WorkUnit requires review again first.
			f.manager.SetNowFunc(func() time.Time { return time.Now().Add(-5 * time.Hour) })
			expired, err := f.manager.Acquire(ctx, n, "alice", "mock", "plan", time.Hour, 1)
			f.manager.SetNowFunc(time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if err = lease.SaveClaimToken(f.root, n, expired.Lease.ClaimID, expired.BaseSHA); err != nil {
				t.Fatal(err)
			}
			if raw := must(append(append([]string{}, args...), "--dry-run", "--json")...); !strings.Contains(raw, `"claim_required":true`) {
				t.Fatalf("expired token treated as live: %s", raw)
			}
			calls := checkCalls()
			if _, err = run(append(append([]string{}, args...), "--json")...); !errors.Is(err, lease.ErrApprovalRequired) {
				t.Fatalf("stale review after reapproval not required: %v", err)
			}
			fresh := state()
			freshToken, err := lease.LoadClaimToken(f.root, n)
			if err != nil || fresh.Lease.Status != "active" || fresh.Lease.ClaimID == expired.Lease.ClaimID || freshToken != fresh.Lease.ClaimID || !fresh.Lease.ExpiresAt.After(time.Now()) || checkCalls() != calls {
				t.Fatalf("expired token did not yield fresh admission: %+v token=%s %v", fresh.Lease, freshToken, err)
			}
			recordReview(freshToken)

			// The final reconciliation reuses the live claim byte-for-byte.
			heldLease = leaseJSON(state())
			must(append(append([]string{}, args...), "--json")...)
			recovered := state()
			if leaseJSON(recovered) != heldLease {
				t.Fatalf("reused claim was renewed or replaced: %s != %s", leaseJSON(recovered), heldLease)
			}
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

// checkpointActorTracker authenticates the same GitHub API session as another
// login, so only approved policy can refuse that actor.
type checkpointActorTracker struct {
	tracker.Tracker
	actor string
}

func (t checkpointActorTracker) CurrentUser(context.Context) (string, error) { return t.actor, nil }

// checkpointSnapshot records regular file bytes and modes, directories
// (including empty ones) and symlink targets without following links.
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
			info, err := entry.Info()
			if err != nil {
				return err
			}
			switch {
			case entry.Type().IsRegular():
				raw, e := os.ReadFile(path)
				if e != nil {
					return e
				}
				files[path] = info.Mode().String() + " " + string(raw)
			case entry.Type()&os.ModeSymlink != 0:
				target, e := os.Readlink(path)
				if e != nil {
					return e
				}
				files[path] = "symlink -> " + target
			default:
				files[path] = info.Mode().String()
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

// snapshotDiff names the first few changed, added or removed snapshot paths.
func snapshotDiff(before, after map[string]string) string {
	var changed []string
	for path, value := range before {
		if got, ok := after[path]; !ok {
			changed = append(changed, "removed "+path)
		} else if got != value {
			changed = append(changed, "changed "+path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			changed = append(changed, "added "+path)
		}
	}
	slices.Sort(changed)
	if len(changed) > 5 {
		changed = append(changed[:5], "...")
	}
	return strings.Join(changed, ", ")
}
