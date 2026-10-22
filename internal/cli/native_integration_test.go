package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

// This opt-in test uses the operator's existing Codex login and model quota.
// Coordination and Draft PR creation stay in a disposable local fixture.
func TestNativeCodexAuthenticatedWorkUnit(t *testing.T) {
	if os.Getenv("SWARM_TEST_CODEX_AUTHENTICATED") != "1" {
		t.Skip("set SWARM_TEST_CODEX_AUTHENTICATED=1 to use your Codex login")
	}
	t.Setenv("SWARM_EXECUTION_MODE", "isolated")
	t.Setenv("SWARM_USER", "native-worker")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Native Integration")
	git("config", "user.email", "native@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	policy := "required_checks:\n  - name: greeting\n    command: test \"$(cat hello.txt)\" = 'hello from SwarmOS'\n    timeout: 30s\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "approved native workflow fixture")
	tr := tracker.NewLocalTracker(root)
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := lease.NewManager(tr, cfg.Security)
	if err := manager.ConfigureStore(root); err != nil {
		t.Fatal(err)
	}
	f := &cliFixture{root: root, tracker: tr, manager: manager}
	f.must(t, "doctor")
	f.must(t, "issue", "create", "--title", "Native greeting", "--objective", "Create hello.txt containing exactly hello from SwarmOS followed by a newline.", "--constraint", "Keep repository policy unchanged. Make only this small text-file change.", "--acceptance", "The greeting required check passes.", "--gate", "human")
	issues, err := tr.ListIssues(context.Background(), tracker.IssueFilter{State: "open"})
	if err != nil || len(issues) != 1 {
		t.Fatal("missing fixture issue", err)
	}
	number := issues[0].Number
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	// Exercise the same streamed interactive phase used by --headless=false.
	// Empty input accepts the completed phase without widening tool permissions.
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = oldStdin })
	f.must(t, "work", n, "--phase", "plan", "--headless=false")
	if _, err := f.run("submit", n); err == nil {
		t.Fatal("submission bypassed human review")
	}
	f.must(t, "review", n)
	if _, err := f.run("work", n, "--phase", "implement"); err == nil {
		t.Fatal("agent review satisfied the human gate")
	}
	t.Setenv("SWARM_USER", "native-maintainer")
	f.must(t, "review", n, "--approve", "--notes", "Verified the bounded greeting plan")
	t.Setenv("SWARM_USER", "native-worker")
	f.must(t, "work", n, "--headless")
	f.must(t, "submit", n)
	state := f.state(t, number)
	if state.PRNumber == 0 || state.Lease.Status != "completed" {
		t.Fatalf("incomplete native workflow: %+v", state)
	}
	pr, err := tr.FindDraftPR(context.Background(), state.Branch, "main")
	if err != nil || pr == nil || pr.Number != state.PRNumber {
		t.Fatal("missing local Draft PR", err)
	}
	for _, artifact := range state.Artifacts {
		if artifact.Type == "design" || artifact.Type == "test-plan" || artifact.Type == "implementation" || artifact.Type == "validation" {
			if artifact.Metadata["sandbox"] != "codex-native" {
				t.Fatalf("missing native provenance: %+v", artifact)
			}
		}
	}
	t.Log("Codex login: planner (interactive), reviewer, human gate, developer, tester, native validation and local Draft PR completed")
}
