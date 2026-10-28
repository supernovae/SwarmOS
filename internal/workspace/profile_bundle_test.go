package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

func TestProfileCannotChangeGateOrChecks(t *testing.T) {
	t.Parallel()
	wt, base := budgetRepo(t, "lease:\n  duration: 30m\n  renewable: false\n  max_renewals: 0\n")
	cfg, err := harness.LoadConfigAt(wt, base)
	if err != nil {
		t.Fatal(err)
	}
	validation, err := showHarnessFile(wt, base, ".swarm/validation/required-checks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wu := &contract.WorkUnit{ID: "ISSUE-7", IssueNumber: 7, Title: "Profile", Objective: "Implement profiles", Workflow: "feature", Gate: contract.GateHuman}
	wf := &contract.WorkflowSpec{Name: "feature", DefaultGate: contract.GateAgent}
	if err := CompileHarnessBundleAt(wt, wu, wf, nil, "planner", base); err != nil {
		t.Fatal(err)
	}
	baseline, err := os.ReadFile(filepath.Join(wt, ".swarm/active/SYSTEM_PROMPT.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range cfg.Profiles {
		wu.Size = p.Match.Size
		wu.Uncertainty = p.Match.Uncertainty
		wu.Risk = p.Match.Risk
		if got := contract.SelectProfile(wu, cfg.Profiles); got == nil || got.Name != p.Name {
			t.Fatalf("select %s: %+v", p.Name, got)
		}
		gate, err := contract.ResolveGate(wu.Gate, wf.DefaultGate, cfg.DefaultGate)
		if err != nil || gate != contract.GateHuman {
			t.Fatalf("gate %s %v", gate, err)
		}
		if err := CompileHarnessBundleAt(wt, wu, wf, nil, "planner", base); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(wt, ".swarm/active/SYSTEM_PROMPT.md"))
		if err != nil {
			t.Fatal(err)
		}
		start := strings.Index(string(raw), "## Execution profile: ")
		end := strings.Index(string(raw), "## Claim budget")
		if start < 0 || end <= start {
			t.Fatalf("profile section boundaries for %s", p.Name)
		}
		without := string(raw[:start]) + string(raw[end:])
		if without != string(baseline) {
			t.Fatalf("profile %s changed content outside its section", p.Name)
		}
		if !strings.Contains(string(raw), "## Execution profile: "+p.Name) || !strings.Contains(string(raw), strings.TrimSpace(validation)) {
			t.Fatalf("profile/checks missing: %s", p.Name)
		}
	}
}

func TestPlannerCannotChangeProfile(t *testing.T) {
	t.Parallel()
	wt, base := budgetRepo(t, "lease:\n  duration: 30m\n  renewable: false\n  max_renewals: 0\n")
	for path, content := range map[string]string{".swarm/config.yaml": "profiles: [{name: compact}]\n", ".swarm/prompts/variants/planner.sensitive.md": "CANDIDATE VARIANT", ".swarm/active/PLAN.md": "Profile: compact"} {
		full := filepath.Join(wt, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	wu := &contract.WorkUnit{ID: "ISSUE-7", IssueNumber: 7, Title: "Profile", Objective: "Implement profiles", Workflow: "feature", Size: "small", Risk: "sensitive"}
	if err := CompileHarnessBundleAt(wt, wu, &contract.WorkflowSpec{Name: "feature"}, nil, "planner", base); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(wt, ".swarm/active/SYSTEM_PROMPT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "## Execution profile: sensitive") || strings.Contains(string(raw), "CANDIDATE VARIANT") {
		t.Fatalf("untrusted selection: %s", raw)
	}
	before := contract.DigestWorkUnit(wu)
	wu.Risk = "standard"
	if before == contract.DigestWorkUnit(wu) {
		t.Fatal("dimension did not affect approval digest")
	}
}

func TestProfileMissingConfigNonPlanner(t *testing.T) {
	t.Parallel()
	wt, _ := budgetRepo(t, "lease:\n  duration: 30m\n  renewable: false\n  max_renewals: 0\n")
	runWorkspaceGit(t, wt, "rm", "-f", ".swarm/config.yaml")
	runWorkspaceGit(t, wt, "commit", "-m", "legacy without config")
	base := workspaceGitOutput(t, wt, "rev-parse", "HEAD")
	sys, err := budgetBundle(t, wt, base, "developer", 0)
	if err != nil || strings.Contains(sys, "## Execution profile:") {
		t.Fatalf("missing config: %v", err)
	}
}
