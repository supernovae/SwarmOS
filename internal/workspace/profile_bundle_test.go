package workspace

import (
	"encoding/json"
	"fmt"
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
	if err := CompileHarnessBundleAt(wt, wu, wf, nil, "planner", contract.GateHuman, base); err != nil {
		t.Fatal(err)
	}
	baseline, err := os.ReadFile(filepath.Join(wt, ".swarm/active/SYSTEM_PROMPT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(baseline), "## Execution profile:") {
		t.Fatal("dimensionless WorkUnit selected a profile")
	}
	if err := os.WriteFile(filepath.Join(wt, ".swarm/active/PLAN.md"), []byte("Suggested dimensions: size=small uncertainty=known risk=standard\nProfile: compact"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := CompileHarnessBundleAt(wt, wu, wf, nil, "planner", contract.GateHuman, base); err != nil {
		t.Fatal(err)
	}
	suggested, err := os.ReadFile(filepath.Join(wt, ".swarm/active/SYSTEM_PROMPT.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(suggested) != string(baseline) {
		t.Fatal("PLAN suggestions selected a profile or changed the bundle")
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
		if err := CompileHarnessBundleAt(wt, wu, wf, nil, "planner", contract.GateHuman, base); err != nil {
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
	if err := CompileHarnessBundleAt(wt, wu, &contract.WorkflowSpec{Name: "feature"}, nil, "planner", contract.GateNone, base); err != nil {
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

// Variants exist only for the planner and reviewer. Other roles get the
// profile heading and evidence bullets without an empty body between them.
func TestProfileWithoutVariantHasNoEmptyBody(t *testing.T) {
	t.Parallel()
	wt, base := budgetRepo(t, "lease:\n  duration: 30m\n  renewable: false\n  max_renewals: 0\n")
	for _, role := range []string{"developer", "tester"} {
		wu := &contract.WorkUnit{ID: "ISSUE-7", IssueNumber: 7, Title: "Profile", Objective: "Implement profiles", Workflow: "feature", Size: "small"}
		if err := CompileHarnessBundleAt(wt, wu, &contract.WorkflowSpec{Name: "feature"}, nil, role, contract.GateNone, base); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(wt, ".swarm/active/SYSTEM_PROMPT.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "## Execution profile: compact\n\n- ") || strings.Contains(string(raw), "## Execution profile: compact\n\n\n") {
			t.Fatalf("%s profile section has an empty variant body:\n%s", role, raw)
		}
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

func TestBundleEffectiveGate(t *testing.T) {
	for _, pr := range []int{0, 31} {
		for _, gate := range []contract.GateMode{contract.GateHuman, contract.GateAgent, contract.GateNone} {
			t.Run(string(gate)+fmt.Sprint(pr), func(t *testing.T) {
				wt, base := budgetRepo(t, "lease:\n  duration: 30m\n  renewable: false\n  max_renewals: 0\n")
				wu := &contract.WorkUnit{ID: "ISSUE-7", IssueNumber: 7, Title: "Gate", Objective: "Display gate", Workflow: "feature"}
				wf := &contract.WorkflowSpec{Name: "feature", DefaultGate: contract.GateAgent}
				digest := contract.DigestWorkUnit(wu)
				cfg, err := harness.LoadConfigAt(wt, base)
				if err != nil {
					t.Fatal(err)
				}
				policy := contract.DigestApprovedPolicy(base, cfg)
				compile := func(g contract.GateMode) error {
					if pr != 0 {
						return CompilePRHarnessBundleAt(wt, wu, wf, nil, "developer", g, base, pr)
					}
					return CompileHarnessBundleAt(wt, wu, wf, nil, "developer", g, base)
				}
				for _, candidate := range []string{"none", "human"} {
					if err := os.WriteFile(filepath.Join(wt, ".swarm/config.yaml"), []byte("default_gate: "+candidate+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(wt, ".swarm/workflows/feature.yaml"), []byte("default_gate: "+candidate+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := compile(gate); err != nil {
						t.Fatal(err)
					}
					raw, err := os.ReadFile(filepath.Join(wt, ".swarm/active/WORKUNIT.md"))
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(raw), "**Plan Review Gate**: `"+string(gate)+"`") || !strings.Contains(string(raw), "**Task Plan Gate**: `none`") {
						t.Fatalf("wrong gates: %s", raw)
					}
					raw, err = os.ReadFile(filepath.Join(wt, ".swarm/active/workunit.json"))
					if err != nil {
						t.Fatal(err)
					}
					var got contract.WorkUnit
					if err := json.Unmarshal(raw, &got); err != nil {
						t.Fatal(err)
					}
					if contract.DigestWorkUnit(&got) != digest || contract.DigestWorkUnit(wu) != digest || contract.DigestApprovedPolicy(base, cfg) != policy {
						t.Fatal("display mutated authority")
					}
				}
			})
		}
	}
}

func TestBundleRejectsMissingEffectiveGate(t *testing.T) {
	for _, gate := range []contract.GateMode{"", "invalid"} {
		root := t.TempDir()
		err := compileHarnessBundle(root, &contract.WorkUnit{}, nil, nil, "developer", "", "", "", "", "", "", bundleGuidance{effectiveGate: gate})
		if err == nil {
			t.Fatal("accepted missing or invalid gate")
		}
		if gate == "" && !strings.Contains(err.Error(), "swarm issue approve") {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, ".swarm/active")); !os.IsNotExist(err) {
			t.Fatal("wrote bundle before validation")
		}
	}
}

func TestBundleGatePreservesConfigFailures(t *testing.T) {
	for _, tc := range []struct {
		name, role, config string
		absent             bool
	}{
		{name: "legacy developer", role: "developer", absent: true},
		{name: "planner absence", role: "planner", absent: true},
		{name: "developer malformed", role: "developer", config: "invalid: ["},
		{name: "planner malformed", role: "planner", config: "invalid: ["},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt, _ := budgetRepo(t, "lease:\n  duration: 30m\n  renewable: false\n  max_renewals: 0\n")
			if tc.absent {
				runWorkspaceGit(t, wt, "rm", "-f", ".swarm/config.yaml")
			} else {
				if err := os.WriteFile(filepath.Join(wt, ".swarm/config.yaml"), []byte(tc.config), 0600); err != nil {
					t.Fatal(err)
				}
				runWorkspaceGit(t, wt, "add", ".swarm/config.yaml")
			}
			runWorkspaceGit(t, wt, "commit", "-m", "config boundary")
			base := workspaceGitOutput(t, wt, "rev-parse", "HEAD")
			wu := &contract.WorkUnit{ID: "ISSUE-7", IssueNumber: 7, Title: "Gate", Objective: "Display"}
			err := CompileHarnessBundleAt(wt, wu, &contract.WorkflowSpec{Name: "in-memory"}, nil, tc.role, contract.GateHuman, base)
			if tc.absent && tc.role == "developer" {
				if err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(wt, ".swarm/active/WORKUNIT.md"))
				if err != nil || !strings.Contains(string(raw), "**Plan Review Gate**: `human`") {
					t.Fatalf("explicit gate lost: %s %v", raw, err)
				}
			} else if err == nil {
				t.Fatal("supplied gate masked config failure")
			}
		})
	}
}
