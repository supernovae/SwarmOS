package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/workspace"
)

var errCompletionPublication = errors.New("injected checkpoint publication failure")

type completionPublicationStore struct {
	coordination.Store
	root      string
	ambiguous bool
	failed    atomic.Bool
}

func (s *completionPublicationStore) CompareAndSwap(ctx context.Context, n int, rev string, raw []byte) (string, error) {
	var st lease.IssueState
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", err
	}
	receipt := filepath.Join(s.root, ".git", "swarm-checkpoint-completion", fmt.Sprintf("issue-%d.json", n))
	if _, err := os.Stat(receipt); err == nil && st.HeadSHA != "" && !s.failed.Swap(true) {
		if s.ambiguous {
			if _, err = s.Store.CompareAndSwap(ctx, n, rev, raw); err != nil {
				return "", err
			}
		}
		return "", errCompletionPublication
	}
	return s.Store.CompareAndSwap(ctx, n, rev, raw)
}
func completionGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func completionEngineFixture(t *testing.T, ambiguous bool) (*Engine, WorkOptions, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf finished > finished.txt; printf 'Implemented and tested.\n' > .swarm/active/IMPLEMENTATION.md")
	root, e, tr := setupProjectPlanTestRepo(t)
	configureValidation(t, root)
	completionGit(t, root, "add", ".")
	completionGit(t, root, "commit", "-m", "completion validation")
	wu := &contract.WorkUnit{Objective: "Resume finished implementation", Constraints: []string{"Scoped"}, Acceptance: []string{"Completed"}, Workflow: "feature", Gate: contract.GateAgent}
	issue, err := tr.CreateIssue(t.Context(), "Completion", contract.FormatIssueMarkdown(wu), []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.LeaseManager.ApproveTask(t.Context(), issue.Number, "maintainer"); err != nil {
		t.Fatal(err)
	}
	dev, tester := new(atomic.Int32), new(atomic.Int32)
	runner := func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			if req.Role == "developer" {
				dev.Add(1)
			}
			if req.Role == "tester" {
				tester.Add(1)
			}
			return (&agent.MockRunner{}).Run(ctx, req)
		}}, nil
	}
	e.NewRunner = runner
	opts := WorkOptions{RepoRoot: root, IssueNumber: issue.Number, AgentName: "mock", Phase: "plan", Headless: true}
	planned, err := e.ExecuteWork(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.ClaimID = planned.State.Lease.ClaimID
	_, branch, err := workspace.ProvisionAt(root, issue.Number, issue.Title, planned.State.BaseSHA, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.LeaseManager.RecordCheckpoint(t.Context(), issue.Number, "maintainer", branch, planned.State.BaseSHA); err != nil {
		t.Fatal(err)
	}
	opts.Phase = "all"
	e.LeaseManager.SetStore(root, &completionPublicationStore{Store: e.LeaseManager.Store(), root: root, ambiguous: ambiguous})
	_, err = e.ExecuteWork(t.Context(), opts)
	if !errors.Is(err, errCompletionPublication) {
		t.Fatalf("wanted injected publication failure: %v", err)
	}
	st, loadErr := e.LeaseManager.GetCurrentState(t.Context(), issue.Number)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	opts.ClaimID = st.Lease.ClaimID
	c, loadErr := e.LeaseManager.InspectCheckpointCompletion(t.Context(), issue.Number)
	if loadErr != nil || c == nil {
		t.Fatalf("missing receipt %v", loadErr)
	}
	want := fmt.Sprintf("swarm issue reconcile %d --checkpoint --expected-head %s --candidate-head %s", issue.Number, c.Predecessor, c.Candidate)
	if !strings.Contains(err.Error(), want) {
		t.Fatal(err)
	}
	path := workspace.WorktreePath(root, issue.Number)
	if completionGit(t, path, "status", "--porcelain") != "" {
		t.Fatal("failed publication left dirty tree")
	}
	if completionGit(t, path, "rev-parse", "HEAD") != c.Candidate {
		t.Fatal("checkpoint lost")
	}
	if _, err = os.ReadFile(filepath.Join(path, ".swarm/active/IMPLEMENTATION.md")); err != nil {
		t.Fatal(err)
	}
	if dev.Load() != 1 || tester.Load() != 0 {
		t.Fatal("unexpected role calls")
	}
	fresh, err := NewEngine(root, tr)
	if err != nil {
		t.Fatal(err)
	}
	fresh.NewRunner = runner
	// A fresh FileStore restores the authoritative same-claim state. The injected
	// transport failure is deliberately absent after the process restart.
	fresh.LeaseManager.SetStore(root, coordination.NewFileStore(root))
	return fresh, opts, dev, tester
}
func TestCheckpointCompletionFreshEngineResume(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint(ambiguous), func(t *testing.T) {
			e, opts, dev, tester := completionEngineFixture(t, ambiguous)
			result, err := e.ExecuteWork(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			tree, err := workspace.CheckpointTree(t.Context(), result.WorktreePath)
			if err != nil {
				t.Fatal(err)
			}
			if err = lease.VerifyTrustedImplementation(result.State, result.HeadSHA, tree, result.BranchName); err != nil {
				t.Fatal(err)
			}
			if !result.ValidationPassed || dev.Load() != 1 || tester.Load() != 1 {
				t.Fatalf("validation=%v dev=%d tester=%d", result.ValidationPassed, dev.Load(), tester.Load())
			}
		})
	}
}
func TestCheckpointCompletionEarlyRejection(t *testing.T) {
	for _, name := range []string{"report", "dirty", "candidate", "branch", "receipt", "missing-receipt", "worktree-forgery", "instructions"} {
		t.Run(name, func(t *testing.T) {
			e, opts, dev, tester := completionEngineFixture(t, false)
			path := workspace.WorktreePath(e.repoRoot, opts.IssueNumber)
			switch name {
			case "report":
				os.WriteFile(filepath.Join(path, ".swarm/active/IMPLEMENTATION.md"), []byte("edited"), 0600)
			case "dirty":
				os.WriteFile(filepath.Join(path, "untracked"), []byte("dirty"), 0600)
			case "candidate":
				completionGit(t, path, "commit", "--allow-empty", "-m", "changed")
			case "branch":
				completionGit(t, path, "checkout", "-b", "other")
			case "receipt":
				os.WriteFile(filepath.Join(e.repoRoot, ".git/swarm-checkpoint-completion", fmt.Sprintf("issue-%d.json", opts.IssueNumber)), []byte("forged"), 0600)
			case "missing-receipt":
				os.Remove(filepath.Join(e.repoRoot, ".git/swarm-checkpoint-completion", fmt.Sprintf("issue-%d.json", opts.IssueNumber)))
			case "worktree-forgery":
				private := filepath.Join(e.repoRoot, ".git/swarm-checkpoint-completion", fmt.Sprintf("issue-%d.json", opts.IssueNumber))
				data, err := os.ReadFile(private)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(path, ".swarm/active/COMPLETION.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(private); err != nil {
					t.Fatal(err)
				}
			case "instructions":
				opts.DeveloperInstructions = "change more"
			}
			head := completionGit(t, path, "rev-parse", "HEAD")
			status := completionGit(t, path, "status", "--porcelain")
			report, _ := os.ReadFile(filepath.Join(path, ".swarm/active/IMPLEMENTATION.md"))
			if _, err := e.ExecuteWork(t.Context(), opts); err == nil {
				t.Fatal("recovery accepted changed input")
			}
			if dev.Load() != 1 || tester.Load() != 0 {
				t.Fatal("rejection launched an agent")
			}
			if head != completionGit(t, path, "rev-parse", "HEAD") || status != completionGit(t, path, "status", "--porcelain") {
				t.Fatal("rejection changed work")
			}
			after, _ := os.ReadFile(filepath.Join(path, ".swarm/active/IMPLEMENTATION.md"))
			if string(after) != string(report) {
				t.Fatal("rejection replaced report")
			}
		})
	}
}
