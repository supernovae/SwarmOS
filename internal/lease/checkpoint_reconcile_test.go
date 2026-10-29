package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

type reconciliationFixture struct {
	t                                *testing.T
	m                                *Manager
	tr                               *tracker.LocalTracker
	n                                int
	path, branch, base, a, b, remote string
}

func (f *reconciliationFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := fixtureGitCommand(f.t, append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func (f *reconciliationFixture) write(dir, name, body string) {
	f.t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0644); err != nil {
		f.t.Fatal(err)
	}
}
func newReconciliationFixture(t *testing.T, command string, local ...bool) *reconciliationFixture {
	t.Helper()
	m, tr, issue := testManager(t)
	f := &reconciliationFixture{t: t, m: m, tr: tr, n: issue.Number}
	cfgPath := filepath.Join(m.repoRoot, ".swarm/config.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	f.write(m.repoRoot, ".swarm/config.yaml", string(cfg)+"\nexecution:\n  mode: trusted-local\n")
	f.write(m.repoRoot, ".swarm/validation/required-checks.yaml", fmt.Sprintf("required_checks:\n  - name: check\n    command: %q\n    timeout: 1m\nprotected_paths:\n  - .swarm/validation/**\n  - .swarm/config.yaml\n", command))
	f.git(m.repoRoot, "add", ".swarm")
	f.git(m.repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "validation baseline")
	f.remote = filepath.Join(t.TempDir(), "remote.git")
	f.git(m.repoRoot, "init", "--bare", "--initial-branch=main", f.remote)
	f.git(m.repoRoot, "remote", "add", "origin", f.remote)
	f.git(m.repoRoot, "push", "origin", "HEAD:refs/heads/main")
	if len(local) != 0 && local[0] {
		m.SetStore(m.repoRoot, coordination.NewFileStore(m.repoRoot))
	} else {
		m.SetStore(m.repoRoot, coordination.NewGitStore(m.repoRoot))
	}
	if _, err = m.ApproveTask(context.Background(), f.n, "local-dev"); err != nil {
		t.Fatal(err)
	}
	st, err := m.Acquire(context.Background(), f.n, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.base = st.BaseSHA
	f.path, f.branch, err = workspace.ProvisionAt(m.repoRoot, f.n, issue.Title, f.base, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f.git(f.path, "config", "user.name", "Test")
	f.git(f.path, "config", "user.email", "test@example.invalid")
	f.write(f.path, "feature.txt", "feature\n")
	f.git(f.path, "add", "feature.txt")
	f.git(f.path, "commit", "-m", "A")
	f.a = f.git(f.path, "rev-parse", "HEAD")
	f.review("original approved plan")
	if _, err = m.RecordCheckpoint(context.Background(), f.n, "local-dev", f.branch, f.a); err != nil {
		t.Fatal(err)
	}
	// A metadata-only rewrite retains precisely the same patch but a new commit.
	f.git(f.path, "commit", "--amend", "-m", "B")
	f.b = f.git(f.path, "rev-parse", "HEAD")
	return f
}
func (f *reconciliationFixture) review(plan string) {
	f.t.Helper()
	ctx := context.Background()
	for _, a := range []contract.Artifact{{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: plan}, {Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "tests"}} {
		if _, err := f.m.UpdateArtifact(ctx, f.n, "local-dev", a); err != nil {
			f.t.Fatal(err)
		}
	}
	if _, err := f.m.RecordHumanReview(ctx, f.n, "APPROVED", "reviewed replacement"); err != nil {
		f.t.Fatal(err)
	}
}
func (f *reconciliationFixture) state() *IssueState {
	f.t.Helper()
	st, err := f.m.GetCurrentState(context.Background(), f.n)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}
func TestReconcileCheckpointThenResume(t *testing.T) {
	t.Parallel()
	f := newReconciliationFixture(t, "true")
	ctx := context.Background()
	if _, err := f.m.UpdateArtifact(ctx, f.n, "local-dev", contract.Artifact{Type: contract.ArtifactValidation, Producer: "swarm:validator", Verdict: "PASS", Summary: "old validation", Body: "validated A only", Metadata: map[string]string{"head_sha": f.a, "base_sha": f.base, "harness": "mock", "role": "tester"}}); err != nil {
		t.Fatal(err)
	}
	before := f.state()
	if _, _, err := workspace.ProvisionAt(f.m.repoRoot, f.n, "test", f.base, f.branch, f.a); err == nil || !strings.Contains(err.Error(), f.b) || !strings.Contains(err.Error(), f.a) {
		t.Fatalf("resume didn't report full divergence: %v", err)
	}
	dry, err := f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, true)
	if err != nil || dry.ReviewRequired {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	if !reflect.DeepEqual(before, f.state()) {
		t.Fatal("dry run modified authoritative state")
	}
	got, err := f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false)
	if err != nil {
		t.Fatal(err)
	}
	st := got.State
	if st.BaseSHA != before.BaseSHA || st.TaskDigest != before.TaskDigest || st.PolicyDigest != before.PolicyDigest || st.HeadSHA != f.b {
		t.Fatal("lost pinned approval or candidate")
	}
	if st.LastRebase == nil || st.LastRebase.OldHead != f.a || st.LastRebase.NewHead != f.b || st.LastRebase.Status != "completed" || st.LastRebase.PatchIdentity != "unchanged" {
		t.Fatalf("missing lineage: %+v", st.LastRebase)
	}
	found := false
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactValidation {
			found = true
			if a.Metadata["head_sha"] != f.b || a.Metadata["base_sha"] != f.base || a.Body == "validated A only" {
				t.Fatal("stale validation")
			}
		}
	}
	if !found {
		t.Fatal("validation missing")
	}
	for _, a := range st.Attestations {
		if a.Role == "tester" {
			t.Fatal("old tester attestation carried to new candidate")
		}
	}
	if _, _, err = workspace.ProvisionAt(f.m.repoRoot, f.n, "test", f.base, f.branch, f.b); err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{f.a, f.b} {
		if f.git(f.remote, "rev-parse", coordination.BranchLayout.CheckpointRef(f.n, sha)) != sha {
			t.Fatal("checkpoint not retained")
		}
	}
	// Repeating the old transition cannot append another lineage event.
	if _, err = f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("replay: %v", err)
	}
}
func TestCheckpointRecordingAdditiveAndRewrite(t *testing.T) {
	t.Parallel()
	f := newReconciliationFixture(t, "true")
	ctx := context.Background()
	if _, err := f.m.RecordCheckpoint(ctx, f.n, "local-dev", f.branch, f.b); err == nil {
		t.Fatal("ordinary recording accepted rewrite")
	}
	f.git(f.path, "reset", "--hard", f.a)
	f.git(f.path, "commit", "--allow-empty", "-m", "additive")
	c := f.git(f.path, "rev-parse", "HEAD")
	if _, err := f.m.RecordCheckpoint(ctx, f.n, "local-dev", f.branch, c); err != nil {
		t.Fatalf("ordinary additive checkpoint failed: %v", err)
	}
}
func TestCheckpointReconciliationRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	scenarios := []string{"wrong owner", "stale token", "missing token", "expired", "wrong expected", "missing candidate", "wrong branch", "detached", "untracked", "tracked", "staged", "conflicts", "rebasing", "remote moved", "PR pending", "PR submitted", "unsupported store", "fork", "failed validation", "candidate policy", "snapshot modified", "no base ancestry", "changed patch", "stale validation", "stale review", "stale task", "stale policy", "stale plan", "stale test plan", "untrusted review"}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			command := "true"
			if scenario == "failed validation" {
				command = "false"
			}
			if scenario == "snapshot modified" {
				command = "printf changed > feature.txt"
			}
			f := newReconciliationFixture(t, command)
			ctx := context.Background()
			owner := "local-dev"
			expected, candidate := f.a, f.b
			mutateState := func(fn func(*IssueState)) {
				st, rev, err := f.m.load(ctx, f.n)
				if err != nil {
					t.Fatal(err)
				}
				fn(st)
				raw, _ := json.Marshal(st)
				if _, err = f.m.store.CompareAndSwap(ctx, f.n, rev, raw); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "wrong owner":
				owner = "other"
			case "stale token":
				f.m.BindClaim(f.n, "stale")
			case "missing token":
				f.m.BindClaim(f.n, "")
			case "expired":
				f.m.SetNowFunc(func() time.Time { return time.Now().Add(24 * time.Hour) })
			case "wrong expected":
				expected = f.base
			case "missing candidate":
				candidate = strings.Repeat("f", 40)
			case "wrong branch":
				f.git(f.path, "checkout", "-b", "other")
			case "detached":
				f.git(f.path, "checkout", "--detach")
			case "untracked":
				f.write(f.path, "keep.txt", "untracked")
			case "tracked":
				f.write(f.path, "feature.txt", "dirty")
			case "staged":
				f.write(f.path, "feature.txt", "staged")
				f.git(f.path, "add", "feature.txt")
			case "conflicts":
				// Seed real unmerged index entries.
				blob := f.git(f.path, "rev-parse", "HEAD:feature.txt")
				cmd := fixtureGitCommand(t, "-C", f.path, "update-index", "--index-info")
				cmd.Stdin = strings.NewReader(fmt.Sprintf("0 %040d\tfeature.txt\n100644 %s 1\tfeature.txt\n100644 %s 2\tfeature.txt\n", 0, blob, blob))
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("conflict: %v %s", err, out)
				}
			case "rebasing":
				p := f.git(f.path, "rev-parse", "--git-path", "rebase-merge")
				if !filepath.IsAbs(p) {
					p = filepath.Join(f.path, p)
				}
				if err := os.Mkdir(p, 0755); err != nil {
					t.Fatal(err)
				}
			case "remote moved":
				f.git(f.path, "push", "--force", "origin", f.b+":refs/heads/"+f.branch)
			case "PR pending":
				mutateState(func(st *IssueState) { st.PRRevision = &PRRevision{Number: 1} })
			case "PR submitted":
				mutateState(func(st *IssueState) { st.PRNumber = 1 })
			case "unsupported store":
				f.m.SetStore(f.m.repoRoot, struct{ coordination.Store }{f.m.store})
				_, err := f.m.ReconcileCheckpoint(ctx, f.n, owner, expected, candidate, false)
				if err == nil || !strings.Contains(err.Error(), "requires an upstream Git store or local FileStore") {
					t.Fatalf("unsupported store: %v", err)
				}
				return
			case "fork":
				mutateState(func(st *IssueState) { st.ForkRepo = "alice/repo" })
			case "candidate policy":
				f.write(f.path, ".swarm/validation/required-checks.yaml", "required_checks: []\nprotected_paths: []\n")
				f.git(f.path, "add", ".swarm")
				f.git(f.path, "commit", "-m", "tamper")
				candidate = f.git(f.path, "rev-parse", "HEAD")
				f.review("reviewed plan for " + candidate)
			case "no base ancestry":
				f.git(f.path, "checkout", "--orphan", "unrelated")
				f.git(f.path, "commit", "-m", "unrelated")
				candidate = f.git(f.path, "rev-parse", "HEAD")
				f.git(f.path, "branch", "-f", f.branch, candidate)
				f.git(f.path, "checkout", f.branch)
			case "changed patch":
				f.write(f.path, "feature.txt", "unrelated replacement")
				f.git(f.path, "add", "feature.txt")
				f.git(f.path, "commit", "--amend", "-m", "changed patch")
				candidate = f.git(f.path, "rev-parse", "HEAD")
			case "stale validation":
				mutateState(func(st *IssueState) {
					st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactValidation, Verdict: "PASS", Body: "old", Metadata: map[string]string{"head_sha": f.a}})
					st.Artifacts[2].Metadata["plan_digest"] = "stale"
				})
			case "stale review", "stale task", "stale policy", "stale plan", "stale test plan", "untrusted review":
				mutateState(func(st *IssueState) {
					for i := range st.Artifacts {
						a := &st.Artifacts[i]
						if a.Type != contract.ArtifactReview {
							continue
						}
						key := map[string]string{"stale review": "content_sha256", "stale task": "task_digest", "stale policy": "policy_digest", "stale plan": "plan_digest", "stale test plan": "test_plan_digest", "untrusted review": "review_source"}[scenario]
						a.Metadata[key] = "stale"
					}
				})
			}
			before := f.state()
			refs := f.git(f.remote, "show-ref")
			local := f.git(f.path, "status", "--porcelain")
			_, err := f.m.ReconcileCheckpoint(ctx, f.n, owner, expected, candidate, false)
			if err == nil {
				t.Fatal("invalid reconciliation succeeded")
			}
			if !reflect.DeepEqual(before, f.state()) || f.git(f.remote, "show-ref") != refs || f.git(f.path, "status", "--porcelain") != local {
				t.Fatal("rejection mutated state, refs or working files")
			}
		})
	}
}

func TestChangedCheckpointRequiresFreshCandidatePlan(t *testing.T) {
	t.Parallel()
	f := newReconciliationFixture(t, "true")
	ctx := context.Background()
	f.write(f.path, "feature.txt", "replacement patch\n")
	f.git(f.path, "add", "feature.txt")
	f.git(f.path, "commit", "--amend", "-m", "changed")
	f.b = f.git(f.path, "rev-parse", "HEAD")
	dry, err := f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, true)
	if err != nil || !dry.ReviewRequired {
		t.Fatalf("missing review guidance: %+v %v", dry, err)
	}
	if _, err = f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("stale review authorized changed patch: %v", err)
	}
	f.review("Replacement plan for candidate " + f.b)
	if _, err = f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileCheckpointValidationRaces(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("requires named-pipe check barrier")
	}
	for _, scenario := range []string{"publication from another clone", "remote changed", "claim changed", "plan changed", "candidate changed", "candidate dirtied"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			pipes := t.TempDir()
			ready := filepath.Join(pipes, "ready")
			release := filepath.Join(pipes, "release")
			if out, err := exec.Command("mkfifo", ready, release).CombinedOutput(); err != nil {
				t.Fatalf("mkfifo: %v %s", err, out)
			}
			command := fmt.Sprintf("printf ready > '%s'; read signal < '%s'", ready, release)
			f := newReconciliationFixture(t, command)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			before := f.state()
			done := make(chan error, 1)
			go func() { _, err := f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false); done <- err }()
			input, err := os.Open(ready)
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 5)
			if _, err = input.Read(buf); err != nil {
				t.Fatal(err)
			}
			input.Close()
			switch scenario {
			case "publication from another clone":
				second := filepath.Join(t.TempDir(), "second")
				f.git(f.m.repoRoot, "clone", f.remote, second)
				f.git(second, "checkout", "--detach", f.a)
				f.git(second, "-c", "user.name=Worker C", "-c", "user.email=c@example.invalid", "commit", "--allow-empty", "-m", "C")
				c := f.git(second, "rev-parse", "HEAD")
				store := coordination.NewGitStore(second)
				raw, rev, err := store.Load(ctx, f.n)
				if err != nil {
					t.Fatal(err)
				}
				var state IssueState
				if err = json.Unmarshal(raw, &state); err != nil {
					t.Fatal(err)
				}
				state.HeadSHA = c
				raw, _ = json.Marshal(state)
				if _, err = store.CompareAndSwapAndPublishExpected(ctx, f.n, rev, raw, f.branch, c, f.a); err != nil {
					t.Fatal(err)
				}
			case "remote changed":
				f.git(f.path, "push", "--force", "origin", f.b+":refs/heads/"+f.branch)
			case "claim changed", "plan changed":
				state, rev, err := f.m.load(ctx, f.n)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "claim changed" {
					state.Lease.ClaimID = "successor"
				} else {
					state.Artifacts[0].Body = "new plan"
				}
				raw, _ := json.Marshal(state)
				if _, err = f.m.store.CompareAndSwap(ctx, f.n, rev, raw); err != nil {
					t.Fatal(err)
				}
			case "candidate changed":
				f.git(f.path, "commit", "--allow-empty", "-m", "candidate moved")
			case "candidate dirtied":
				f.write(f.path, "feature.txt", "keep these uncommitted edits")
			}
			afterRace := f.state()
			refs := f.git(f.remote, "show-ref")
			output, err := os.OpenFile(release, os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = output.WriteString("continue\n"); err != nil {
				t.Fatal(err)
			}
			output.Close()
			if err = <-done; !errors.Is(err, ErrClaimFence) {
				t.Fatalf("race escaped detectable fence: %v", err)
			}
			if !reflect.DeepEqual(afterRace, f.state()) || f.git(f.remote, "show-ref") != refs {
				t.Fatal("loser changed authoritative evidence or remote")
			}
			if scenario == "publication from another clone" && f.state().HeadSHA == before.HeadSHA {
				t.Fatal("worker C didn't win")
			}
		})
	}
}

type initialCheckpointRaceStore struct {
	*coordination.GitStore
	move func()
}

func (s *initialCheckpointRaceStore) CompareAndSwapAndPublishExpected(ctx context.Context, n int, old string, next []byte, branch, head, expected string) (string, error) {
	s.move()
	return s.GitStore.CompareAndSwapAndPublishExpected(ctx, n, old, next, branch, head, expected)
}
func TestFirstCheckpointDoesNotAdoptRemoteHead(t *testing.T) {
	t.Parallel()
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			t.Parallel()
			f := newReconciliationFixture(t, "true")
			ctx := context.Background()
			st, rev, err := f.m.load(ctx, f.n)
			if err != nil {
				t.Fatal(err)
			}
			st.HeadSHA = ""
			raw, _ := json.Marshal(st)
			if _, err = f.m.store.CompareAndSwap(ctx, f.n, rev, raw); err != nil {
				t.Fatal(err)
			}
			if race {
				f.git(f.path, "push", "origin", ":refs/heads/"+f.branch)
				f.m.SetStore(f.m.repoRoot, &initialCheckpointRaceStore{GitStore: coordination.NewGitStore(f.m.repoRoot), move: func() { f.git(f.path, "push", "origin", f.a+":refs/heads/"+f.branch) }})
			}
			if _, err = f.m.RecordCheckpoint(ctx, f.n, "local-dev", f.branch, f.b); !errors.Is(err, ErrClaimFence) {
				t.Fatalf("first publication overwrote foreign remote: %v", err)
			}
			if f.state().HeadSHA != "" || f.git(f.remote, "rev-parse", "refs/heads/"+f.branch) != f.a {
				t.Fatal("loser advanced state or branch")
			}
		})
	}
}

type failingCheckpointProjection struct{ tracker.Tracker }

func (failingCheckpointProjection) CreateComment(context.Context, int, string) (*tracker.Comment, error) {
	return nil, fmt.Errorf("projection unavailable")
}
func (failingCheckpointProjection) UpdateComment(context.Context, int64, string) (*tracker.Comment, error) {
	return nil, fmt.Errorf("projection unavailable")
}
func TestCheckpointProjectionFailureDoesNotRepeatLineage(t *testing.T) {
	t.Parallel()
	f := newReconciliationFixture(t, "true")
	ctx := context.Background()
	f.m.tracker = failingCheckpointProjection{f.tr}
	if _, err := f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false); err != nil {
		t.Fatal(err)
	}
	_, before, err := f.m.load(ctx, f.n)
	if err != nil {
		t.Fatal(err)
	}
	f.m.tracker = f.tr
	f.m.project(ctx, f.n, f.state())
	if _, err = f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("repeat transition: %v", err)
	}
	_, after, err := f.m.load(ctx, f.n)
	if err != nil || before != after {
		t.Fatal("projection retry changed ledger lineage")
	}
}

func TestReconciledCheckpointIsNotRetryEligible(t *testing.T) {
	t.Parallel()
	f := newReconciliationFixture(t, "true")
	ctx := context.Background()
	got, err := f.m.ReconcileCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false)
	if err != nil {
		t.Fatal(err)
	}
	reconciled := false
	for _, a := range got.State.Artifacts {
		reconciled = reconciled || (a.Type == contract.ArtifactImplementation && a.Producer == "swarm:checkpoint-reconciler" && a.Verdict == "PASS" && a.Metadata["head_sha"] == f.b)
	}
	if !reconciled {
		t.Fatalf("no reconciler PASS: %+v", got.State.Artifacts)
	}
	tree := f.git(f.path, "rev-parse", f.b+"^{tree}")
	if err := VerifyTrustedImplementation(got.State, f.b, tree, f.branch); err == nil || !strings.Contains(err.Error(), "lacks trusted completion") {
		t.Fatalf("reconciler PASS eligibility: %v", err)
	}
	if _, err := f.m.CheckImplementationRetry(ctx, f.n, "local-dev", f.b, tree, f.branch); err == nil || !strings.Contains(err.Error(), "lacks trusted completion") {
		t.Fatalf("reconciler PASS retry: %v", err)
	}
}

func TestCheckpointFreshClaimValidationFailurePreservesWork(t *testing.T) {
	t.Parallel()
	for _, local := range []bool{false, true} {
		for _, command := range []string{"false", "printf modified > feature.txt"} {
			t.Run(fmt.Sprintf("local=%t/%s", local, command), func(t *testing.T) {
				t.Parallel()
				f := newReconciliationFixture(t, command, local)
				ctx := context.Background()
				f.git(f.path, "merge", "--no-edit", f.a)
				f.b = f.git(f.path, "rev-parse", "HEAD")
				if _, err := f.m.Release(ctx, f.n, "local-dev", "fixture released"); err != nil {
					t.Fatal(err)
				}
				p, err := f.m.PrepareCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false)
				if err != nil || !p.ClaimRequired {
					t.Fatalf("prepare: %+v %v", p, err)
				}
				acquired, err := f.m.AcquirePreparedCheckpoint(ctx, f.n, p)
				if err != nil {
					t.Fatal(err)
				}
				if err = SaveClaimToken(f.m.repoRoot, f.n, acquired.Lease.ClaimID, acquired.BaseSHA); err != nil {
					t.Fatal(err)
				}
				refs := f.git(f.remote, "show-ref")
				if _, err = f.m.ReconcilePreparedCheckpoint(ctx, f.n, p); err == nil {
					t.Fatal("validation unexpectedly passed")
				}
				current := f.state()
				token, err := LoadClaimToken(f.m.repoRoot, f.n)
				if err != nil || token != acquired.Lease.ClaimID || current.Lease.ClaimID != token || current.Lease.Status != "active" || current.HeadSHA != f.a {
					t.Fatalf("failure lost authorized retry fence: %+v %v", current, err)
				}
				if refs != f.git(f.remote, "show-ref") || f.git(f.path, "rev-parse", "HEAD") != f.b || f.git(f.path, "status", "--porcelain") != "" {
					t.Fatal("failed validation published or modified local work")
				}
				for _, art := range current.Artifacts {
					if art.Type == contract.ArtifactValidation || art.Type == contract.ArtifactImplementation {
						t.Fatal("failed validation recorded PASS evidence")
					}
				}
			})
		}
	}
}

type checkpointAdmissionRaceStore struct {
	*coordination.GitStore
	hook func()
}

func (s *checkpointAdmissionRaceStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	if s.hook != nil {
		hook := s.hook
		s.hook = nil
		hook()
	}
	return s.GitStore.Load(ctx, n)
}

func TestCheckpointAdmissionRacesUseNormalFences(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"successor", "record then release", "reapproval", "base refresh"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			f := newReconciliationFixture(t, "true")
			ctx := context.Background()
			f.git(f.path, "merge", "--no-edit", f.a)
			f.b = f.git(f.path, "rev-parse", "HEAD")
			if _, err := f.m.Release(ctx, f.n, "local-dev", "fixture released"); err != nil {
				t.Fatal(err)
			}
			p, err := f.m.PrepareCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false)
			if err != nil {
				t.Fatal(err)
			}
			original := f.m.store.(*coordination.GitStore)
			other := NewManager(f.tr, f.m.security)
			other.SetStore(f.m.repoRoot, original)
			var competing *IssueState
			racing := &checkpointAdmissionRaceStore{GitStore: original}
			racing.hook = func() {
				switch scenario {
				case "successor", "record then release":
					var err error
					competing, err = other.Acquire(ctx, f.n, "local-dev", "mock", "plan", time.Hour, 1)
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "record then release" {
						if _, err = other.RecordCheckpoint(ctx, f.n, "local-dev", f.branch, f.b); err != nil {
							t.Fatal(err)
						}
						if _, err = other.Release(ctx, f.n, "local-dev", "fixture competing worker finished"); err != nil {
							t.Fatal(err)
						}
					}
				case "reapproval":
					issue, err := f.tr.GetIssue(ctx, f.n)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = f.tr.UpdateIssueBody(ctx, f.n, strings.Replace(issue.Body, "objective:", "objective: amended ", 1)); err != nil {
						t.Fatal(err)
					}
					// A new approval is itself a revision even when a fixture's objective
					// differs from the replace string.
					if _, err = other.ApproveTask(ctx, f.n, "local-dev"); err != nil {
						t.Fatal(err)
					}
				case "base refresh":
					f.git(f.m.repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "new approved base")
					f.git(f.m.repoRoot, "push", "origin", "HEAD:refs/heads/main")
					if _, err := other.ApproveTask(ctx, f.n, "local-dev"); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.m.SetStore(f.m.repoRoot, racing)
			if _, err = f.m.AcquirePreparedCheckpoint(ctx, f.n, p); err == nil {
				t.Fatal("admission accepted changed observation")
			}
			current := f.state()
			if scenario == "successor" && (current.Lease.ClaimID != competing.Lease.ClaimID || current.Lease.Status != "active") {
				t.Fatal("admission released or replaced successor")
			}
			if scenario == "record then release" && current.HeadSHA != f.b {
				t.Fatal("lost concurrent checkpoint")
			}
			if f.git(f.path, "rev-parse", "HEAD") != f.b || f.git(f.path, "status", "--porcelain") != "" {
				t.Fatal("admission race modified candidate")
			}
		})
	}
}

func TestCheckpointPreparationWithoutClaimRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"other owner", "successor", "missing live token", "stale task", "dirty", "wrong branch", "no predecessor ancestry", "historical token", "absent token"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			f := newReconciliationFixture(t, "true")
			ctx := context.Background()
			historical := f.state().Lease.ClaimID
			if _, err := f.m.Release(ctx, f.n, "local-dev", "fixture released"); err != nil {
				t.Fatal(err)
			}
			if scenario != "no predecessor ancestry" {
				f.git(f.path, "merge", "--no-edit", f.a)
				f.b = f.git(f.path, "rev-parse", "HEAD")
			}
			switch scenario {
			case "other owner", "successor", "missing live token":
				actor := "local-dev"
				if scenario == "other owner" {
					actor = "other"
				}
				other := NewManager(f.tr.WithUser(actor, "OWNER"), f.m.security)
				other.SetStore(f.m.repoRoot, f.m.store)
				if _, err := other.Acquire(ctx, f.n, actor, "mock", "plan", time.Hour, 1); err != nil {
					t.Fatal(err)
				}
				if scenario == "missing live token" {
					f.m.BindClaim(f.n, "")
				}
			case "stale task":
				issue, err := f.tr.GetIssue(ctx, f.n)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.tr.UpdateIssueBody(ctx, f.n, strings.Replace(issue.Body, "objective:", "objective: amended ", 1)); err != nil {
					t.Fatal(err)
				}
			case "dirty":
				f.write(f.path, "untracked", "preserve")
			case "wrong branch":
				f.git(f.path, "checkout", "-b", "foreign")
			case "historical token":
				f.m.BindClaim(f.n, historical)
			case "absent token":
				f.m.BindClaim(f.n, "")
			}
			before := f.state()
			refs := f.git(f.remote, "show-ref")
			prepared, err := f.m.PrepareCheckpoint(ctx, f.n, "local-dev", f.a, f.b, false)
			if scenario == "historical token" || scenario == "absent token" {
				if err != nil || !prepared.ClaimRequired {
					t.Fatalf("no-live-claim admission unavailable: %+v %v", prepared, err)
				}
			} else if err == nil {
				t.Fatal("invalid precondition accepted")
			}
			if !reflect.DeepEqual(before, f.state()) || refs != f.git(f.remote, "show-ref") {
				t.Fatal("preparation wrote authority")
			}
		})
	}
}
