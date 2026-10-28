package harness

import (
	"os"
	"strings"
	"testing"
)

func TestDecodeConfigRejectsProfileGateFields(t *testing.T) {
	raw, err := embeddedTemplates.ReadFile("templates/swarm/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"gate", "default_gate", "required_checks", "protected_paths", "security"} {
		data := strings.Replace(string(raw), "  - name: urgent", "  - "+key+": none\n    name: urgent", 1)
		if _, err := decodeConfig([]byte(data)); err == nil {
			t.Fatalf("accepted profile %s", key)
		}
	}
}

func TestProfilePromptSync(t *testing.T) {
	for _, role := range []string{"planner", "reviewer"} {
		copies := routinePromptCopies(t, role+".md")
		assertPromptContains(t, role, copies, []string{"do not choose or change a profile in PLAN.md"})
		for _, variant := range []string{"compact", "sensitive", "detailed", "investigation", "urgent"} {
			routinePromptCopies(t, "variants/"+role+"."+variant+".md")
		}
	}
	raw, err := os.ReadFile("../../.swarm/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decodeConfig(raw)
	if err != nil || len(cfg.Profiles) != 5 {
		t.Fatalf("repo profiles: %+v %v", cfg, err)
	}
}
