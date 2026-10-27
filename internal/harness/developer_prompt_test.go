package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeveloperOutputInstructionsAndPromptSync(t *testing.T) {
	template, err := embeddedTemplates.ReadFile("templates/swarm/prompts/developer.md")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := os.ReadFile("../../.swarm/prompts/developer.md")
	if err != nil {
		t.Fatal(err)
	}
	// Normalize line endings so Windows checkouts compare equal.
	normalize := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
	if !bytes.Equal(normalize(repo), normalize(template)) {
		t.Fatal(".swarm/prompts/developer.md differs from internal/harness/templates/swarm/prompts/developer.md")
	}
	root := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	scaffolded, err := os.ReadFile(filepath.Join(root, ".swarm/prompts/developer.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"notes, test results, and any blocked or failing checks",
		"`.swarm/active/IMPLEMENTATION.md`",
		"`.swarm/active/LESSONS.md` is also allowed",
		"Changing any other `.swarm/active/` file",
		"`VALIDATION.md`",
		"discards your entire run",
	} {
		if !strings.Contains(string(template), text) || !strings.Contains(string(scaffolded), text) {
			t.Fatalf("developer prompt lacks %q", text)
		}
	}
}
