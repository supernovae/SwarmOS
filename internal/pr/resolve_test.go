package pr

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workspace"
)

// failRunner fails the test if a resolver turn starts.
type failRunner struct {
	*agent.MockRunner
	t *testing.T
}

func (f failRunner) Run(context.Context, agent.RunRequest) (*agent.RunResult, error) {
	f.t.Fatal("resolver agent ran for an ineligible conflict")
	return nil, nil
}

func conflictedRepo(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil && args[0] != "rebase" {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	put := func(body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	put("base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "pr")
	put("pr\n")
	git("commit", "-qam", "pr")
	git("checkout", "-q", "main")
	put("main\n")
	git("commit", "-qam", "main")
	git("checkout", "-q", "pr")
	git("rebase", "main")
	return root
}

func TestResolveStopRejectsProtectedPathsBeforeTheAgent(t *testing.T) {
	root := conflictedRepo(t, ".github/workflows/ci.yml")
	s := &Service{}
	rv := &resolver{runner: failRunner{&agent.MockRunner{}, t}, agentName: "mock", unit: &contract.WorkUnit{IssueNumber: 1}, protected: []string{".github/**"}}
	if _, err := s.resolveStop(context.Background(), rv, root, 1, "alice", &lease.IssueState{}); !errors.Is(err, workspace.ErrConflictIneligible) || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected conflict: err = %v", err)
	}
	// An explicitly authorized harness change may be resolved.
	rv.unit.AllowHarnessChanges = true
	rv.runner = &agent.MockRunner{}
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "exit 9")
	if _, err := s.resolveStop(context.Background(), rv, root, 1, "alice", &lease.IssueState{}); !errors.Is(err, ErrResolveRejected) {
		t.Fatalf("allowed harness change did not reach the agent: %v", err)
	}
}

func TestImportResolvedRollsBackOnFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("conflicted a\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// b.txt became a directory, so writing its resolution must fail.
	if err := os.Mkdir(filepath.Join(root, "b.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	inputs := []workspace.ConflictInput{
		{Path: "a.txt", Worktree: []byte("conflicted a\n"), Mode: 0o640},
		{Path: "b.txt", Worktree: []byte("conflicted b\n"), Mode: 0o644},
	}
	err := importResolved(context.Background(), root, inputs, map[string][]byte{"a.txt": []byte("resolved a\n"), "b.txt": []byte("resolved b\n")})
	if err == nil {
		t.Fatal("import into a directory succeeded")
	}
	data, readErr := os.ReadFile(filepath.Join(root, "a.txt"))
	info, statErr := os.Stat(filepath.Join(root, "a.txt"))
	if readErr != nil || statErr != nil || string(data) != "conflicted a\n" || info.Mode().Perm() != 0o640 {
		t.Fatalf("a.txt not restored: %q mode=%v", data, info.Mode())
	}
}
