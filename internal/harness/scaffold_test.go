package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitRepositoryAndDoctor(t *testing.T) {
	tmp := t.TempDir()
	// Simulate a Go repository
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/test\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := InitRepository(InitOptions{RepoRoot: tmp})
	if err != nil {
		t.Fatalf("InitRepository failed: %v", err)
	}
	if res.Stack.Language != "Go" {
		t.Fatalf("expected Go stack, got %q", res.Stack.Language)
	}

	expectedPaths := []string{
		".swarm/config.yaml",
		".swarm/project.md",
		".swarm/architecture.md",
		".swarm/conventions.md",
		".swarm/workflows/feature.yaml",
		".swarm/workflows/bugfix.yaml",
		".swarm/workflows/refactor.yaml",
		".swarm/prompts/planner.md",
		".swarm/prompts/reviewer.md",
		".swarm/prompts/developer.md",
		".swarm/prompts/tester.md",
		".swarm/contracts/workunit.schema.json",
		".swarm/contracts/artifact.schema.json",
		".swarm/validation/required-checks.yaml",
		".github/ISSUE_TEMPLATE/swarm-workunit.yml",
		".github/workflows/swarm-validate.yml",
		".claude/commands/swarm-plan.md",
		".claude/commands/swarm-review.md",
		".claude/commands/swarm-implement.md",
		".claude/commands/swarm-validate.md",
		".opencode/command/swarm-plan.md",
		".gitignore",
	}
	for _, rel := range expectedPaths {
		if !fileExists(filepath.Join(tmp, rel)) {
			t.Errorf("expected file %s to exist after InitRepository", rel)
		}
	}

	checks := RunDoctor(tmp, false)
	if len(checks) == 0 {
		t.Fatal("expected doctor checks")
	}
	for _, c := range checks {
		if c.Category == "harness" && c.Status != "OK" {
			t.Errorf("expected harness check %s to be OK, got %s (%s)", c.Name, c.Status, c.Detail)
		}
	}
}
