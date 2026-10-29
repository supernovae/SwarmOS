package harness

import (
	"strings"
	"testing"
)

func TestIssue102ConfigAndForms(t *testing.T) {
	b, err := embeddedTemplates.ReadFile("templates/swarm/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decodeConfig(b)
	if err != nil || cfg.BaseBranch != "" {
		t.Fatalf("default: %v", err)
	}
	cfg, err = decodeConfig(append([]byte("base_branch: release/1.x\n"), b...))
	if err != nil || cfg.BaseBranch != "release/1.x" {
		t.Fatalf("configured: %v", err)
	}
	for _, name := range []string{"HEAD", "refs/tags/v1", "-release", "a~1"} {
		if _, err := decodeConfig(append([]byte("base_branch: "+name+"\n"), b...)); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	for _, name := range []string{"workunit", "feature", "bugfix", "refactor"} {
		b, err := embeddedTemplates.ReadFile("templates/github/ISSUE_TEMPLATE/swarm-" + name + ".yml")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "id: base_branch") || !strings.Contains(string(b), "label: Base Branch") {
			t.Fatal("missing form input")
		}
	}
}
