package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/progress"
)

func TestDoctorSelectedFailuresAndOptionalHarnesses(t *testing.T) {
	for _, mode := range []string{"isolated", "trusted-local"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SWARM_EXECUTION_MODE", mode)
			cfg := &contract.SwarmConfig{DefaultAgent: "codex", Roles: map[string]string{"tester": "limited"}, Agents: map[string]contract.AgentConfig{
				"codex": {}, "limited": {Roles: []string{"developer"}}, "claude": {},
			}}
			checks := doctorAgentChecks(cfg, false, func(name string, _ *contract.SwarmConfig, _ bool, role string) (string, error) {
				if name == "codex" {
					return "verified", nil
				}
				return "", fmt.Errorf("unavailable for %s", role)
			})
			statuses := map[string]string{}
			for _, c := range checks {
				statuses[c.Name] = c.Status
			}
			for name, want := range map[string]string{"codex/planner": "OK", "limited/tester": "FAIL", "limited/developer": "OPTIONAL", "claude/reviewer": "OPTIONAL"} {
				if statuses[name] != want {
					t.Errorf("%s = %s; want %s", name, statuses[name], want)
				}
			}
		})
	}
}

func TestDoctorRejectsUnavailableSelectedAgent(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	cfg := &contract.SwarmConfig{DefaultAgent: "codex", Agents: map[string]contract.AgentConfig{"codex": {Binary: "/no-such-swarmos-test-binary"}}}
	checks := doctorAgentChecks(cfg, false, agent.Preflight)
	if len(checks) != 4 {
		t.Fatalf("checks: %+v", checks)
	}
	for _, c := range checks {
		if c.Status != "FAIL" || !strings.Contains(c.Detail, "unavailable") {
			t.Fatalf("missed selected failure: %+v", c)
		}
	}
}

func TestDoctorRejectsExcludedSelectedRole(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	cfg := &contract.SwarmConfig{DefaultAgent: "limited", Agents: map[string]contract.AgentConfig{
		"limited": {Binary: "git", Harness: "codex", Roles: []string{"developer"}},
	}}
	checks := doctorAgentChecks(cfg, false, agent.Preflight)
	if len(checks) != 4 {
		t.Fatalf("checks: %+v", checks)
	}
	for _, c := range checks {
		if c.Name == "limited/developer" {
			if c.Status != "OK" {
				t.Fatalf("allowed role: %+v", c)
			}
		} else if c.Status != "FAIL" || !strings.Contains(c.Detail, "not permitted for role") {
			t.Fatalf("excluded selected role: %+v", c)
		}
	}
}

func TestDoctorReportsCIDriftAsWarningOnly(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".swarm/validation/required-checks.yaml": "required_checks:\n  - name: lint\n    command: go vet ./...\n    timeout: 5m\nprotected_paths:\n  - .swarm/validation/**\n",
		".github/workflows/ci.yml":               "jobs:\n  lint:\n    steps:\n      - run: go vet ./...\n      - run: go tool staticcheck ./...\n",
	}
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const drift = "ci lint/format coverage"
	for _, jsonMode := range []bool{false, true} {
		var ran bool
		// Run the policy and drift checks for real; report the environment-
		// dependent checks as healthy so only drift can change the outcome.
		factory := func(context.Context, func(progress.Event)) harness.DoctorRunner {
			return func(category, name string, check func() harness.DoctorCheck) harness.DoctorCheck {
				if name == ".swarm/validation/required-checks.yaml" || name == drift {
					ran = ran || name == drift
					return check()
				}
				if name == ".swarm/config.yaml" {
					check()
				}
				return harness.DoctorCheck{Category: category, Name: name, Status: "OK", Detail: "fixture"}
			}
		}
		var stdout bytes.Buffer
		cmd := newDoctorCmdWithRunner(&RootOptions{RepoRoot: root, Backend: "local"}, factory)
		cmd.SetContext(context.Background())
		cmd.SetOut(&stdout)
		cmd.SetErr(io.Discard)
		if jsonMode {
			if err := cmd.Flags().Set("json", "true"); err != nil {
				t.Fatal(err)
			}
		}
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("json=%v: drift failed doctor: %v\n%s", jsonMode, err, stdout.String())
		}
		if !ran {
			t.Fatalf("json=%v: drift check did not run", jsonMode)
		}
		if jsonMode {
			var result struct {
				Status string                `json:"status"`
				Checks []harness.DoctorCheck `json:"checks"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			var warnings []harness.DoctorCheck
			for _, check := range result.Checks {
				if check.Name == drift {
					warnings = append(warnings, check)
				}
			}
			if result.Status != "passed" || len(warnings) != 1 || warnings[0].Status != "WARN" || !strings.Contains(warnings[0].Detail, "go tool staticcheck ./...") {
				t.Fatalf("JSON doctor = %+v", result)
			}
		} else if out := stdout.String(); !strings.Contains(out, drift) || !strings.Contains(out, "WARN     .github/workflows/ci.yml runs \"go tool staticcheck ./...\"") || strings.Contains(out, "go vet ./...\" (go vet)") {
			t.Fatalf("text doctor:\n%s", out)
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, ".swarm/validation/required-checks.yaml")); err != nil || string(data) != files[".swarm/validation/required-checks.yaml"] {
		t.Fatalf("doctor edited policy: %q, %v", data, err)
	}
}
