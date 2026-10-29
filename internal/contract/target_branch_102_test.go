package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIssue102BranchBindingAndLegacy(t *testing.T) {
	legacyUnit, err := json.Marshal(WorkUnit{})
	if err != nil {
		t.Fatal(err)
	}
	if string(legacyUnit) != `{"id":"","objective":"","constraints":null,"acceptance":null,"workflow":""}` {
		t.Fatalf("legacy WorkUnit encoding changed: %s", legacyUnit)
	}
	cfg := &SwarmConfig{}
	h := sha256.New()
	h.Write([]byte("base"))
	h.Write([]byte{0})
	h.Write([]byte(DigestConfig(cfg)))
	legacy := hex.EncodeToString(h.Sum(nil))
	if DigestApprovedBranchPolicy("base", cfg, "") != legacy {
		t.Fatal("legacy digest changed")
	}
	seen := map[string]bool{legacy: true}
	for _, branch := range []string{"main", "release/1.x", "lts"} {
		d := DigestApprovedBranchPolicy("base", cfg, branch)
		if seen[d] {
			t.Fatal("target identity not bound")
		}
		seen[d] = true
	}
	for _, v := range []any{WorkUnit{}, SwarmConfig{}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "base_branch") {
			t.Fatal("unset encoding changed")
		}
	}
	wu := &WorkUnit{ID: "ISSUE-102", Objective: "target release", Constraints: []string{"keep fences"}, Acceptance: []string{"correct target"}, Workflow: "feature"}
	before := DigestWorkUnit(wu)
	wu.BaseBranch = "release/1.x"
	if DigestWorkUnit(wu) == before {
		t.Fatal("override absent from task digest")
	}
	body := FormatIssueMarkdown(wu)
	parsed, err := ParseWorkUnitFromIssue(102, "", body)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.BaseBranch != wu.BaseBranch {
		t.Fatal("lost Markdown branch")
	}
	objective := "updated objective"
	_, body, err = EditWorkUnitIssue(102, "", body, WorkUnitEdit{Objective: &objective})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = ParseWorkUnitFromIssue(102, "", body)
	if err != nil || parsed.BaseBranch != wu.BaseBranch {
		t.Fatalf("edit lost branch: %v", err)
	}
}

func TestIssue102BranchSyntax(t *testing.T) {
	for _, name := range []string{"", "main", "release/1.x", "feature/a-b"} {
		if err := ValidateBaseBranch(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"HEAD", "-release", "refs/tags/v1", "a..b", "a@{1}", "a~1", "a^", "a:b", "a b", "a\nb", "/a", "a/", "a//b", ".a", "a.lock", "a\\b", "a?b", "a[b", "a*b", "@"} {
		if ValidateBaseBranch(name) == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestIssue102OptionalFormAndYAML(t *testing.T) {
	wu := &WorkUnit{ID: "ISSUE-102", Objective: "target release", Constraints: []string{"keep fences"}, Acceptance: []string{"correct target"}, Workflow: "feature"}
	body := FormatIssueMarkdown(wu) + "\n### Base Branch\n\n_No response_\n"
	parsed, err := ParseWorkUnitFromIssue(102, "", body)
	if err != nil || parsed.BaseBranch != "" {
		t.Fatalf("blank form: %v", err)
	}
	body = "```yaml swarm-workunit\nid: ISSUE-102\nobjective: target release\nconstraints: [keep fences]\nacceptance: [correct target]\nworkflow: feature\nbase_branch: release/1.x\n```"
	parsed, err = ParseWorkUnitFromIssue(102, "", body)
	if err != nil || parsed.BaseBranch != "release/1.x" {
		t.Fatalf("YAML: %v", err)
	}
	objective := "updated"
	_, edited, err := EditWorkUnitIssue(102, "", body, WorkUnitEdit{Objective: &objective})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = ParseWorkUnitFromIssue(102, "", edited)
	if err != nil || parsed.BaseBranch != "release/1.x" {
		t.Fatalf("YAML edit: %v", err)
	}
}

func TestIssue102ExternalWorkUnit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unit.yaml"), []byte("id: ISSUE-102\nobjective: release target\nconstraints: [keep fences]\nacceptance: [target branch]\nworkflow: feature\nbase_branch: release/1.x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wu, err := ParseWorkUnitFromIssueWithRoot(102, "", "swarm-workunit-path: unit.yaml", root)
	if err != nil || wu.BaseBranch != "release/1.x" {
		t.Fatalf("external field lost: %v", err)
	}
}

// Constants follow the pre-target field layout and SHA-256(base + NUL +
// SHA-256(config JSON)); they do not call branch-aware helpers to derive wants.
func TestIssue102LegacyFixedGoldens(t *testing.T) {
	cfg := &SwarmConfig{Version: 1, DefaultAgent: "mock", DefaultGate: GateNone, Lease: LeaseConfig{Duration: "1h"}}
	const configJSON = `{"execution":{},"version":1,"default_agent":"mock","default_gate":"none","lease":{"duration":"1h","renewable":false,"max_renewals":0},"security":{"require_ready_label":false,"ready_label":""},"agents":null}`
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != configJSON {
		t.Fatalf("config bytes changed: %s", b)
	}
	if got := DigestConfig(cfg); got != "f1b5861838491a76788485964150f409c347aa739928326e700b9965b514af04" {
		t.Fatalf("config digest %s", got)
	}
	if got := DigestApprovedBranchPolicy("base", cfg, ""); got != "98f0a91e6c51d224e6e61b0fcd3c95d5c337d22c6fba54970201c52ad372f9c4" {
		t.Fatalf("legacy policy digest %s", got)
	}
	wu := &WorkUnit{ID: "ISSUE-102", Objective: "target release", Constraints: []string{"keep fences"}, Acceptance: []string{"correct target"}, Workflow: "feature"}
	b, err = json.Marshal(wu)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"id":"ISSUE-102","objective":"target release","constraints":["keep fences"],"acceptance":["correct target"],"workflow":"feature"}` {
		t.Fatalf("task bytes changed: %s", b)
	}
	if got := DigestWorkUnit(wu); got != "421ab5e9fe8c00e2eb2339f0794cb5cdf9d59d17f81de2387484bea74eac6c8b" {
		t.Fatalf("task digest %s", got)
	}
	if DigestApprovedBranchPolicy("base", cfg, "main") == "98f0a91e6c51d224e6e61b0fcd3c95d5c337d22c6fba54970201c52ad372f9c4" {
		t.Fatal("explicit default lost binding")
	}
}
