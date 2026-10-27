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

func TestWorkerTestScopeInstructionsAndPromptSync(t *testing.T) {
	root := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	normalize := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
	shared := []string{
		"coordinator independently reruns every required check",
		"Run the `.swarm/active/TEST_PLAN.md` commands and targeted tests for changed packages first",
		"Run the full test suite at most once, at the end, and only when `.swarm/active/TEST_PLAN.md` requires it",
		"Rerun a passing command only after a relevant change",
	}
	for _, tc := range []struct {
		role  string
		extra []string
	}{
		{"developer", []string{"`gofmt`", "`go vet`", "`go tool staticcheck`", "on changed files and packages", "`.swarm/active/IMPLEMENTATION.md`"}},
		{"tester", []string{"`.swarm/active/VALIDATION.md`", "Your verdict is advisory", "authoritative report"}},
	} {
		t.Run(tc.role, func(t *testing.T) {
			path := "swarm/prompts/" + tc.role + ".md"
			template, err := embeddedTemplates.ReadFile("templates/" + path)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := os.ReadFile("../../." + path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(normalize(repo), normalize(template)) {
				t.Fatalf(".%s differs from internal/harness/templates/%s", path, path)
			}
			scaffolded, err := os.ReadFile(filepath.Join(root, "."+path))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(normalize(scaffolded), normalize(template)) {
				t.Fatalf("scaffolded .%s differs from template", path)
			}
			for _, text := range append(append([]string{}, shared...), tc.extra...) {
				if !strings.Contains(string(template), text) {
					t.Errorf("%s prompt lacks %q", tc.role, text)
				}
			}
		})
	}
}
