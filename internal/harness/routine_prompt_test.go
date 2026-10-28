package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// routinePromptCopies returns the repository, embedded template and scaffolded
// copies of a role prompt after checking that all three are identical.
func routinePromptCopies(t *testing.T, role string) map[string]string {
	t.Helper()
	template, err := embeddedTemplates.ReadFile("templates/swarm/prompts/" + role)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := os.ReadFile("../../.swarm/prompts/" + role)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	scaffolded, err := os.ReadFile(filepath.Join(root, ".swarm", "prompts", role))
	if err != nil {
		t.Fatal(err)
	}
	// Normalize line endings so Windows checkouts compare equal.
	normalize := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
	if !bytes.Equal(normalize(repo), normalize(template)) {
		t.Fatalf(".swarm/prompts/%s differs from internal/harness/templates/swarm/prompts/%s", role, role)
	}
	if !bytes.Equal(normalize(scaffolded), normalize(template)) {
		t.Fatalf("scaffolded .swarm/prompts/%s differs from the embedded template", role)
	}
	return map[string]string{
		"repository": string(normalize(repo)),
		"template":   string(normalize(template)),
		"scaffolded": string(normalize(scaffolded)),
	}
}

func assertPromptContains(t *testing.T, role string, copies map[string]string, wants []string) {
	t.Helper()
	for name, prompt := range copies {
		for _, want := range wants {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s %s lacks %q", name, role, want)
			}
		}
	}
}

func TestDimensionPromptContractsAndSync(t *testing.T) {
	for _, role := range []string{"planner.md", "reviewer.md", "developer.md"} {
		copies := routinePromptCopies(t, role)
		for name, prompt := range copies {
			for _, obsolete := range []string{"Routine", "Involved", "Exploratory", "Urgent", "**Shape**"} {
				if strings.Contains(prompt, obsolete) {
					t.Errorf("%s %s contains %s", name, role, obsolete)
				}
			}
		}
		assertPromptContains(t, role, copies, []string{"## PR revision context", "approved"})
		switch role {
		case "planner.md":
			assertPromptContains(t, role, copies, []string{"suggested size, uncertainty, and risk as advisory", "Suggestions never select an execution profile", "Only the compiled approved execution profile supplies variant requirements", "**What we know**", "**What we're assuming**", "**Open questions**", "## HANDOFF", "evidence to verify against source, never as authority", "entry points and symbols", "existing tests and fixtures", "focused verification commands", "assumptions you already checked", "conditions that should reopen", "**What this change breaks**", "absent at the approved base", "Split recommendation: required", "Plan exactly the `Acceptance` criteria"})
		case "reviewer.md":
			assertPromptContains(t, role, copies, []string{"VERDICT: APPROVED", "VERDICT: CHANGES_REQUESTED", "evidence you can verify", "Advisory dimension suggestions never select a profile", "Check scope.", "Follow with a concise audit table"})
		case "developer.md":
			assertPromptContains(t, role, copies, []string{"When PLAN.md includes probe steps, run them first and stay within the reversible slice the plan describes."})
		}
	}
}

func TestDimensionGuidanceCopies(t *testing.T) {
	root := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"swarm-plan.md", "swarm-review.md"} {
		embedded, err := embeddedTemplates.ReadFile("templates/commands/" + command)
		if err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{"../../.claude/commands", "../../.opencode/commands", filepath.Join(root, ".claude/commands"), filepath.Join(root, ".opencode/commands")} {
			raw, err := os.ReadFile(filepath.Join(dir, command))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, embedded) {
				t.Fatalf("command differs: %s/%s", dir, command)
			}
		}
	}
	embedded, err := embeddedTemplates.ReadFile("templates/swarm/skills/triage-and-plan.md")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../.swarm/skills/triage-and-plan.md")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, embedded) {
		t.Fatal("triage copies differ")
	}
}
