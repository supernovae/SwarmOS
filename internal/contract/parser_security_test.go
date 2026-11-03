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

func TestExternalWorkUnitRejectsSymlinkEscape(t *testing.T) {
	for _, parent := range []bool{false, true} {
		root, outside := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "unit.yaml"), []byte("objective: Outside contract\nworkflow: feature\nacceptance:\n  - Preserve boundary\n"), 0600); err != nil {
			t.Fatal(err)
		}
		target, name := filepath.Join(outside, "unit.yaml"), "link"
		if parent {
			target = outside
			name = "link/unit.yaml"
		}
		if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
			t.Skip(err)
		}
		if _, err := ParseWorkUnitFromIssueWithRoot(1, "title", "swarm-workunit-path: "+name, root); err == nil {
			t.Fatal("external symlink accepted")
		}
	}
}

func TestCommitMessageConfigPreservesLegacySerializationAndDigest(t *testing.T) {
	cfg := &SwarmConfig{
		Version:      1,
		DefaultAgent: "codex",
		DefaultGate:  GateAgent,
		Lease:        LeaseConfig{Duration: "4h"},
		Security:     SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"},
		Agents:       map[string]AgentConfig{"codex": {Binary: "codex"}},
	}
	// Match the pre-extension JSON representation rather than pinning an entire
	// serialized snapshot that would obscure unrelated contract changes.
	legacy := struct {
		Execution    ExecutionConfig        `json:"execution,omitempty"`
		Version      int                    `json:"version"`
		DefaultAgent string                 `json:"default_agent"`
		DefaultGate  GateMode               `json:"default_gate"`
		PlanMode     string                 `json:"plan_mode,omitempty"`
		Roles        map[string]string      `json:"roles,omitempty"`
		Lease        LeaseConfig            `json:"lease"`
		Security     SecurityConfig         `json:"security"`
		Agents       map[string]AgentConfig `json:"agents"`
		Fork         *ForkConfig            `json:"fork,omitempty"`
	}{
		Execution: cfg.Execution, Version: cfg.Version, DefaultAgent: cfg.DefaultAgent,
		DefaultGate: cfg.DefaultGate, PlanMode: cfg.PlanMode, Roles: cfg.Roles,
		Lease: cfg.Lease, Security: cfg.Security, Agents: cfg.Agents, Fork: cfg.Fork,
	}
	got, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("omitted commit_message altered legacy JSON: got %s, want %s", got, want)
	}
	sum := sha256.Sum256(want)
	if digest := DigestConfig(cfg); digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("legacy digest changed: got %s, want %x", digest, sum)
	}
	if got := cfg.CommitMessageDetail(); got != "concise" || cfg.CommitMessage != nil {
		t.Fatalf("effective default mutated legacy config: %q, %+v", got, cfg.CommitMessage)
	}
	if got := (*SwarmConfig)(nil).CommitMessageDetail(); got != "concise" {
		t.Fatalf("nil config effective detail = %q", got)
	}
}

func TestCommandOnlyTestPhase(t *testing.T) {
	if err := ValidateWorkPhase("test"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePhase("test"); err == nil || !strings.Contains(err.Error(), "valid: all, plan, review, implement)") {
		t.Fatal("test admitted to coordination")
	}
	if PersistedWorkPhase("test") != "implement" {
		t.Fatal("test not normalized")
	}
	for _, p := range []string{"all", "plan", "review", "implement"} {
		if ValidateWorkPhase(p) != nil || ValidatePhase(p) != nil || PersistedWorkPhase(p) != p {
			t.Fatal(p)
		}
	}
}

func TestPlanFindingsStrictParser(t *testing.T) {
	wrap := func(raw string) string { return PlanFindingsStart + raw + PlanFindingsEnd }
	valid := `{"version":1,"findings":[{"id":"F1","blocking":true,"disposition":"open","criterion":"Fence","trigger":"Stale write","expected":"Reject","actual":"Accept","check":"Unit test"}]}`
	for name, raw := range map[string]string{
		"valid": valid, "empty": `{"version":1,"findings":[]}`,
		"duplicate key":    `{"version":1,"version":1,"findings":[]}`,
		"null list":        `{"version":1,"findings":null}`,
		"version":          `{"version":2,"findings":[]}`,
		"case alias":       strings.Replace(valid, `"version"`, `"Version"`, 1),
		"null blocking":    strings.Replace(valid, `"blocking":true`, `"blocking":null`, 1),
		"unknown":          `{"version":1,"findings":[],"extra":0}`,
		"trailing":         valid + `{}`,
		"blank criterion":  strings.Replace(valid, `"Fence"`, `" "`, 1),
		"missing check":    strings.Replace(valid, `,"check":"Unit test"`, "", 1),
		"bad disposition":  strings.Replace(valid, `"open"`, `"closed"`, 1),
		"bad id":           strings.Replace(valid, `"F1"`, `"F0"`, 1),
		"missing blocking": strings.Replace(valid, `"blocking":true,`, "", 1),
		"oversize":         strings.Repeat(" ", 64<<10) + valid,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePlanFindings(wrap(raw))
			want := name == "valid" || name == "empty"
			if (err == nil) != want {
				t.Fatalf("err=%v", err)
			}
		})
	}
	for _, body := range []string{"", PlanFindingsEnd + valid + PlanFindingsStart, wrap(valid) + wrap(valid), PlanFindingsStart + valid} {
		if _, err := ParsePlanFindings(body); err == nil {
			t.Fatal("accepted invalid delimiters")
		}
	}
	for _, field := range []string{"criterion", "trigger", "expected", "actual", "check"} {
		var r map[string]interface{}
		_ = json.Unmarshal([]byte(valid), &r)
		f := r["findings"].([]interface{})[0].(map[string]interface{})
		f[field] = ""
		raw, _ := json.Marshal(r)
		if _, err := ParsePlanFindings(wrap(string(raw))); err == nil {
			t.Fatalf("accepted blank %s", field)
		}
	}
}

func TestPlanFindingContinuity(t *testing.T) {
	f := PlanFinding{ID: "F1", Blocking: true, Disposition: "resolved", Criterion: "Fence", Trigger: "Stale", Expected: "Reject", Actual: "Accept", Check: "Test"}
	prior := PlanFindings{Version: 1, Findings: []PlanFinding{f}}
	if ValidatePlanContinuity(PlanFindings{Version: 1, Findings: []PlanFinding{}}, []PlanFindings{prior}) == nil {
		t.Fatal("omission accepted")
	}
	f.Disposition = "open"
	current := PlanFindings{Version: 1, Findings: []PlanFinding{f}}
	if ValidatePlanContinuity(current, []PlanFindings{prior}) == nil {
		t.Fatal("reversal accepted without evidence")
	}
	current.Findings[0].ReversalNote = "New stale write counterexample in UpdateArtifact; test fails"
	if err := ValidatePlanContinuity(current, []PlanFindings{prior}); err != nil {
		t.Fatal(err)
	}
	prior.Findings[0].ReversalNote = current.Findings[0].ReversalNote
	if ValidatePlanContinuity(current, []PlanFindings{prior}) == nil {
		t.Fatal("stale reversal evidence accepted")
	}

	f.Disposition = "superseded"
	f.SupersededBy = "F2"
	second := f
	second.ID = "F2"
	second.SupersededBy = "F3"
	third := f
	third.ID = "F3"
	third.Disposition = "open"
	third.SupersededBy = ""
	ledger := PlanFindings{Version: 1, Findings: []PlanFinding{f, second, third}}
	raw, _ := json.Marshal(ledger)
	if _, err := ParsePlanFindings(PlanFindingsStart + string(raw) + PlanFindingsEnd); err != nil {
		t.Fatal(err)
	}
	ledger.Findings[2].Disposition = "superseded"
	ledger.Findings[2].SupersededBy = "F1"
	raw, _ = json.Marshal(ledger)
	if _, err := ParsePlanFindings(PlanFindingsStart + string(raw) + PlanFindingsEnd); err == nil {
		t.Fatal("cycle accepted")
	}
}

func TestPlanFindingsInvalidReferences(t *testing.T) {
	base := PlanFinding{ID: "F1", Disposition: "superseded", SupersededBy: "F2"}
	for name, findings := range map[string][]PlanFinding{
		"missing target": {base},
		"self":           {{ID: "F1", Disposition: "superseded", SupersededBy: "F1"}},
		"duplicate":      {{ID: "F1", Disposition: "open"}, {ID: "F1", Disposition: "resolved"}},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(PlanFindings{Version: 1, Findings: findings})
			if _, err := ParsePlanFindings(PlanFindingsStart + string(raw) + PlanFindingsEnd); err == nil {
				t.Fatal("invalid references accepted")
			}
		})
	}
	old := PlanFindings{Version: 1, Findings: []PlanFinding{{ID: "F2", Disposition: "withdrawn"}}}
	next := PlanFindings{Version: 1, Findings: []PlanFinding{{ID: "F2", Disposition: "withdrawn"}, {ID: "F1", Disposition: "open"}}}
	if ValidatePlanContinuity(next, []PlanFindings{old}) == nil {
		t.Fatal("recycled earlier ID accepted")
	}
	for _, disposition := range []string{"resolved", "withdrawn", "superseded"} {
		next := PlanFindings{Version: 1, Findings: []PlanFinding{{ID: "F2", Disposition: disposition}}}
		if err := ValidatePlanContinuity(next, []PlanFindings{old}); err != nil {
			t.Fatal(err)
		}
	}
}
