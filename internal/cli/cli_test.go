package cli

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIEndToEndLifecycle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SWARM_REGISTRY_PATH", filepath.Join(t.TempDir(), "repos.yaml"))

	if out, err := exec.Command("git", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}

	runCLI := func(args ...string) string {
		t.Helper()
		root := NewRootCmd()
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		fullArgs := append([]string{"--root", tmp, "--backend", "local"}, args...)
		root.SetArgs(fullArgs)
		if err := root.Execute(); err != nil {
			t.Fatalf("swarm %v failed: %v\nOutput:\n%s", args, err, buf.String())
		}
		return buf.String()
	}

	// 1. swarm init --sync-labels
	out := runCLI("init", "--sync-labels")
	if !strings.Contains(out, "Initialized SwarmOS harness") {
		t.Fatalf("unexpected init output: %s", out)
	}

	// 2. swarm doctor
	out = runCLI("doctor")
	if !strings.Contains(out, ".swarm/config.yaml") {
		t.Fatalf("unexpected doctor output: %s", out)
	}

	// 3. swarm issue create
	out = runCLI("issue", "create",
		"--title", "Add version subcommand",
		"--objective", "Add swarm version command printing JSON/text build info",
		"--constraint", "Stdlib only",
		"--acceptance", "swarm version --json outputs valid JSON",
		"--workflow", "feature",
	)
	if !strings.Contains(out, "Created issue #1") {
		t.Fatalf("unexpected issue create output: %s", out)
	}

	// 4. swarm issue lint 1
	out = runCLI("issue", "lint", "1")
	if !strings.Contains(out, "valid WorkUnit") {
		t.Fatalf("unexpected issue lint output: %s", out)
	}

	// 5. swarm claim 1
	out = runCLI("claim", "1", "--agent", "mock")
	if !strings.Contains(out, "Claimed issue #1") {
		t.Fatalf("unexpected claim output: %s", out)
	}

	// 6. swarm work 1 --gate agent --headless --agent mock
	out = runCLI("work", "1", "--gate", "agent", "--headless", "--agent", "mock")
	if !strings.Contains(out, "Plan Review Verdict: APPROVED") || !strings.Contains(out, "Validation: PASS") {
		t.Fatalf("unexpected work output: %s", out)
	}

	// 7. swarm validate 1
	out = runCLI("validate", "1")
	if !strings.Contains(out, "Validation PASSED") {
		t.Fatalf("unexpected validate output: %s", out)
	}

	// 8. swarm status 1
	out = runCLI("status", "1")
	if !strings.Contains(out, "Issue #1: Add version subcommand") {
		t.Fatalf("unexpected status output: %s", out)
	}

	// 9. swarm submit 1
	out = runCLI("submit", "1")
	if !strings.Contains(out, "Submitted Draft PR #1 for issue #1") {
		t.Fatalf("unexpected submit output: %s", out)
	}
}

func TestMultiRepoPullAndWorkNext(t *testing.T) {
	t.Setenv("SWARM_REGISTRY_PATH", filepath.Join(t.TempDir(), "repos.yaml"))

	repoAlpha := t.TempDir()
	repoBeta := t.TempDir()
	for _, r := range []string{repoAlpha, repoBeta} {
		if out, err := exec.Command("git", "init", "-b", "main", r).CombinedOutput(); err != nil {
			t.Fatalf("git init failed: %v (%s)", err, string(out))
		}
	}

	runGlobal := func(args ...string) string {
		t.Helper()
		root := NewRootCmd()
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("swarm %v failed: %v\nOutput:\n%s", args, err, buf.String())
		}
		return buf.String()
	}

	// Register both repositories
	runGlobal("--backend", "local", "repo", "add", "local/alpha", repoAlpha)
	runGlobal("--backend", "local", "repo", "add", "local/beta", repoBeta)

	listOut := runGlobal("repo", "list")
	if !strings.Contains(listOut, "local/alpha") || !strings.Contains(listOut, "local/beta") {
		t.Fatalf("expected both repos in repo list:\n%s", listOut)
	}

	// Create ready issues in both repos using only --repo (without --root!)
	runGlobal("--repo", "local/alpha", "issue", "create",
		"--title", "Alpha Task 1",
		"--objective", "Implement Alpha feature",
		"--constraint", "Keep minimal",
		"--acceptance", "Tests pass",
		"--workflow", "feature",
	)
	runGlobal("--repo", "local/beta", "issue", "create",
		"--title", "Beta Task 1",
		"--objective", "Implement Beta feature",
		"--constraint", "Keep minimal",
		"--acceptance", "Tests pass",
		"--workflow", "feature",
	)

	// Check unified status across all registered repos
	statusAll := runGlobal("status", "--all")
	if !strings.Contains(statusAll, "Alpha Task 1") || !strings.Contains(statusAll, "Beta Task 1") {
		t.Fatalf("expected both tasks in status --all:\n%s", statusAll)
	}

	// Pull first ready issue across all repos (should claim local/alpha #1)
	pullOut := runGlobal("pull", "--all", "--agent", "mock")
	if !strings.Contains(pullOut, "Pulled & claimed [local/alpha] issue #1") {
		t.Fatalf("expected pull --all to claim local/alpha #1, got:\n%s", pullOut)
	}

	// Now run `swarm work --next --all` — since local/alpha #1 is already claimed, it should automatically pick local/beta #1!
	workNextOut := runGlobal("work", "--next", "--all", "--agent", "mock", "--headless")
	if !strings.Contains(workNextOut, "Auto-selected [local/beta] issue #1: Beta Task 1") || !strings.Contains(workNextOut, "Validation: PASS") {
		t.Fatalf("expected work --next --all to select and complete local/beta #1, got:\n%s", workNextOut)
	}
}

func TestNodePoolAndDecomposeCLI(t *testing.T) {
	t.Setenv("SWARM_REGISTRY_PATH", filepath.Join(t.TempDir(), "repos.yaml"))
	t.Setenv("SWARM_NODE_CONFIG", filepath.Join(t.TempDir(), "node.yaml"))

	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}

	runGlobal := func(args ...string) string {
		t.Helper()
		root := NewRootCmd()
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("swarm %v failed: %v\nOutput:\n%s", args, err, buf.String())
		}
		return buf.String()
	}

	// 1. Register repository and initialize Volunteer Node profile
	runGlobal("--backend", "local", "repo", "add", "local/gamma", repo)
	nodeInitOut := runGlobal("node", "init", "--id", "volunteer-node-01", "--capabilities", "coding,testing,review", "--ram", "64GB")
	if !strings.Contains(nodeInitOut, "volunteer-node-01") || !strings.Contains(nodeInitOut, "64GB") {
		t.Fatalf("unexpected node init output:\n%s", nodeInitOut)
	}

	// 2. Create a parent epic with 2 acceptance items and decompose it into parallel subtasks (--sequential=false)
	runGlobal("--repo", "local/gamma", "issue", "create",
		"--title", "Platform Telemetry Epic",
		"--objective", "Add structured metrics and health endpoint",
		"--constraint", "Stdlib only",
		"--acceptance", "Metrics counter endpoint works",
		"--acceptance", "Healthz endpoint returns 200 OK",
		"--workflow", "feature",
		"--ready=false",
	)
	decompOut := runGlobal("--repo", "local/gamma", "decompose", "1", "--sequential=false")
	if !strings.Contains(decompOut, "Decomposed parent issue #1 into 2 child WorkUnit issue(s)") {
		t.Fatalf("unexpected decompose output:\n%s", decompOut)
	}

	// 3. Run volunteer node pool with --concurrency 2 --all --once --auto-submit
	nodeRunOut := runGlobal("node", "run", "--all", "--concurrency", "2", "--once", "--agent", "mock", "--auto-submit")
	if !strings.Contains(nodeRunOut, "2 work unit(s) processed") {
		t.Fatalf("expected node run to process both child work units, got:\n%s", nodeRunOut)
	}
}
