package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

func TestProvisionAndCompileHarnessBundle(t *testing.T) {
	tmp := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: tmp}); err != nil {
		t.Fatalf("InitRepository failed: %v", err)
	}

	wtPath, branch, err := Provision(tmp, 12, "[Swarm] Add Version Command")
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	if branch != "swarm/issue-12-add-version-command" {
		t.Fatalf("unexpected branch name: %q", branch)
	}

	wu := &contract.WorkUnit{
		ID:          "ISSUE-12",
		IssueNumber: 12,
		Title:       "Add Version Command",
		Objective:   "Add swarm version subcommand",
		Constraints: []string{"Stdlib only"},
		Acceptance:  []string{"swarm version --json outputs valid JSON"},
		Workflow:    "feature",
		Gate:        contract.GateAgent,
	}
	wf, err := harness.LoadWorkflow(tmp, "feature")
	if err != nil {
		t.Fatal(err)
	}

	if err := CompileHarnessBundle(wtPath, wu, wf, nil, "planner"); err != nil {
		t.Fatalf("CompileHarnessBundle failed: %v", err)
	}

	for _, rel := range []string{
		".swarm/active/WORKUNIT.md",
		".swarm/active/SYSTEM_PROMPT.md",
		"AGENTS.md",
		"CLAUDE.md",
		".claude/commands/swarm-plan.md",
		".opencode/command/swarm-plan.md",
	} {
		full := filepath.Join(wtPath, rel)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected %s to exist in worktree: %v", rel, err)
		}
	}

	agentsBytes, _ := os.ReadFile(filepath.Join(wtPath, "AGENTS.md"))
	if !strings.Contains(string(agentsBytes), ActiveMarkerStart) {
		t.Fatalf("expected AGENTS.md to contain active marker block")
	}

	if err := CleanEphemeralHarnessMarkers(wtPath); err != nil {
		t.Fatalf("CleanEphemeralHarnessMarkers failed: %v", err)
	}
}
