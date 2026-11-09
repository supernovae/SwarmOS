package harness

import (
	"os"
	"strings"
	"testing"
)

// AGENTS.md is read by people and interactive agents; headless roles see only
// the compiled prompt built from .swarm/. The codebase map and security
// invariants live in .swarm/ and AGENTS.md mirrors them verbatim, so a change
// to either copy fails here until both agree.
func TestAgentsGuideMirrorsCompiledGuidance(t *testing.T) {
	agents, err := os.ReadFile("../../AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	guide := strings.ReplaceAll(string(agents), "\r\n", "\n")
	for _, source := range []string{"../../.swarm/architecture.md", "../../.swarm/rules/security.md"} {
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		_, body, ok := strings.Cut(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
		if !ok || strings.TrimSpace(body) == "" {
			t.Fatalf("%s has no body after its heading", source)
		}
		if !strings.Contains(guide, strings.TrimSpace(body)) {
			t.Fatalf("AGENTS.md does not mirror %s; edit the .swarm file and copy its body into AGENTS.md", source)
		}
	}
	if !strings.Contains(guide, "## Who reads what") {
		t.Fatal("AGENTS.md lacks the reader table")
	}
}
