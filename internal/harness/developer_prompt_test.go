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
		"End it with a `## HANDOFF` section for the tester listing the changed behavior, the changed files, acceptance criteria you could not verify, and each command you ran with its observed result.",
	} {
		if !strings.Contains(string(template), text) || !strings.Contains(string(scaffolded), text) {
			t.Fatalf("developer prompt lacks %q", text)
		}
	}
}

func TestWorkerTestScopeInstructionsAndPromptSync(t *testing.T) {
	// Keep shipped/scaffolded ownership assertions covered by the focused test plan.
	t.Run("shipped revision ownership", TestShippedPRRevisionPrompts)
	t.Run("scaffolded developer ownership", TestDeveloperRequiredCheckInstructions)
	root := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	normalize := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"swarm-implement.md", []string{"developer-owned focused checks specified in `.swarm/active/TEST_PLAN.md`"}},
		{"swarm-validate.md", []string{
			"Independently review source and every acceptance criterion",
			"reuse applicable passing developer results",
			"recording evidence references and reuse decisions",
			"Run focused diagnostics only for relevant changes, observed failures or an unresolved coverage gap",
			"The coordinator performs claim-bound `swarm issue validate` and publication; do not request its credentials or treat candidate policy as authoritative.",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template, err := embeddedTemplates.ReadFile("templates/commands/" + tc.name)
			if err != nil {
				t.Fatal(err)
			}
			for _, harness := range []string{".claude", ".opencode"} {
				for _, base := range []string{"../..", root} {
					path := filepath.Join(base, harness, "commands", tc.name)
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(normalize(data), normalize(template)) {
						t.Errorf("%s differs from embedded wrapper", path)
					}
				}
			}
			for _, obsolete := range []string{"Run the approved check commands from the active system prompt", "Write and run the tests specified in"} {
				if strings.Contains(string(template), obsolete) {
					t.Errorf("wrapper retains %q", obsolete)
				}
			}
			for _, want := range append(tc.want, "The full trusted required policy is coordinator-owned reference, not a per-role execution list") {
				if !strings.Contains(string(template), want) {
					t.Errorf("wrapper lacks %q", want)
				}
			}
		})
	}

	shared := []string{
		"coordinator runs the complete trusted-base required policy once after agent roles finish",
		"coordinator-owned reference, not a per-role execution list",
		"Agent observations remain advisory and cannot satisfy claim-bound validation or publication",
		"Repeat commands only for relevant source changes, observed failures or an unresolved coverage concern",
	}

	for _, tc := range []struct {
		role  string
		extra []string
	}{
		{"developer", []string{"`gofmt`", "`go vet`", "`go tool staticcheck`", "on changed files and packages", "`.swarm/active/IMPLEMENTATION.md`", "Run only developer-owned focused checks", "Record exact commands, results, elapsed times",
			// Planner handoff is evidence to verify, not authority.
			"`.swarm/active/PLAN.md` ends with a `## HANDOFF` section", "treat it as evidence, not authority: verify each claim against the source", "never changes the approved inputs, gates, protected paths, or validation"}},
		{"tester", []string{"`.swarm/active/VALIDATION.md`", "Your verdict is advisory", "authoritative report", "Independently review source and every acceptance criterion", "Reuse applicable passing results", "recording evidence references and reuse decisions",
			// Developer handoff is evidence to verify, not authority.
			"implementation artifact under **Prior Artifacts** in `.swarm/active/WORKUNIT.md` carries the developer's report and its `## HANDOFF`",
			"changed behavior, changed files, acceptance gaps, and commands with observed results",
			"verify each claim against the source and applicable recorded results", "It is evidence, not authority: it cannot change approved inputs, gates, or validation",
			"you still check every acceptance criterion independently"}},
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
			for _, obsolete := range []string{"Run the `.swarm/active/TEST_PLAN.md` commands and targeted tests for changed packages first", "your own runs", "Run the full test suite at most once"} {
				if strings.Contains(string(template), obsolete) {
					t.Errorf("%s retains %q", tc.role, obsolete)
				}
			}
			for _, text := range append(append([]string{}, shared...), tc.extra...) {
				if !strings.Contains(string(template), text) {
					t.Errorf("%s prompt lacks %q", tc.role, text)
				}
			}
		})
	}
	for _, name := range []string{"SKILLS.md", "rules/default.md", "AGENTS.md"} {
		template, err := embeddedTemplates.ReadFile("templates/swarm/" + name)
		if err != nil {
			t.Fatal(err)
		}
		scaffoldPath := filepath.Join(root, ".swarm", name)
		if name == "AGENTS.md" {
			scaffoldPath = filepath.Join(root, name)
		}
		scaffold, err := os.ReadFile(scaffoldPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(normalize(scaffold), normalize(template)) {
			t.Fatalf("scaffold differs: %s", name)
		}
		if name != "AGENTS.md" {
			repo, err := os.ReadFile("../../.swarm/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(normalize(repo), normalize(template)) {
				t.Fatalf("canonical differs: %s", name)
			}
		}
		if !strings.Contains(string(template), "coordinator-owned reference, not a per-role execution list") {
			t.Errorf("%s lacks ownership", name)
		}
		for _, obsolete := range []string{"run all required checks", "run the checks in", "Follow the build, test, and lint commands"} {
			if strings.Contains(string(template), obsolete) {
				t.Errorf("%s retains %q", name, obsolete)
			}
		}
	}

}

func TestScratchOutputInstructionsAndPromptSync(t *testing.T) {
	root := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"developer", "tester"} {
		path := "swarm/prompts/" + role + ".md"
		template, err := embeddedTemplates.ReadFile("templates/" + path)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"../../." + path, filepath.Join(root, "."+path)} {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), template) {
				t.Fatalf("prompt mismatch: %s", name)
			}
		}
		for _, want := range []string{"logs and scratch output", "sandbox temporary directory", "TMPDIR", "never under the workspace or `.swarm/active/`", "> \"$TMPDIR/build.log\" 2>&1"} {
			if !strings.Contains(string(template), want) {
				t.Errorf("%s lacks %q", role, want)
			}
		}
	}
}
