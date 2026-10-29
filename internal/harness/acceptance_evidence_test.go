package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcceptanceEvidenceTesterPrompt(t *testing.T) {
	template, err := embeddedTemplates.ReadFile("templates/swarm/prompts/tester.md")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := os.ReadFile("../../.swarm/prompts/tester.md")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err = InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	scaffold, err := os.ReadFile(filepath.Join(root, ".swarm/prompts/tester.md"))
	if err != nil {
		t.Fatal(err)
	}
	normalize := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
	if !bytes.Equal(normalize(repo), normalize(template)) || !bytes.Equal(normalize(scaffold), normalize(template)) {
		t.Fatal("prompt drift")
	}
	for _, want := range []string{"IMPLEMENTATION.md", "VALIDATION.md", "unmet or unverified", "one-based indices", "without Markdown checkbox prefixes", "<!-- swarm:acceptance-evidence:start -->", "<!-- swarm:acceptance-evidence:end -->", `"version":1`, `"evidence":`, "64 KiB"} {
		if !strings.Contains(string(template), want) {
			t.Fatal(want)
		}
	}
}
