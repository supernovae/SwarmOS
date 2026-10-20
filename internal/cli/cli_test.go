package cli

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestCLIEndToEndLifecycle(t *testing.T) {
	tmp := t.TempDir()
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
