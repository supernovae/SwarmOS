package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/sandbox"
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
				for name, want := range map[string]string{
					"codex/planner":  "checkout policy; entry=codex, source=default_agent, harness=codex, model=harness default, effort=harness default",
					"limited/tester": "checkout policy; entry=limited, source=roles.tester",
				} {
					if c.Name == name && !strings.Contains(c.Detail, want) {
						t.Errorf("%s missing %q: %s", name, want, c.Detail)
					}
				}
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
		if !strings.Contains(c.Detail, "checkout policy") || !strings.Contains(c.Detail, "harness=codex") {
			t.Fatalf("missing selected entry metadata: %+v", c)
		}
		if c.Name == "limited/developer" {
			if c.Status != "OK" {
				t.Fatalf("allowed role: %+v", c)
			}
		} else if c.Status != "FAIL" || !strings.Contains(c.Detail, "not permitted for role") || !strings.Contains(c.Detail, "choose an eligible entry") {
			t.Fatalf("excluded selected role: %+v", c)
		}
	}
}

func TestDoctorReportsCIDriftAsWarningOnly(t *testing.T) {
	t.Parallel()
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
		cmd := newDoctorCmdWithRunner(&RootOptions{RepoRoot: root}, factory)
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

func TestDoctorCapabilitySummary(t *testing.T) {
	t.Parallel()
	for _, harnessName := range []string{"codex", "claude"} {
		for _, role := range []string{"planner", "developer"} {
			report := sandbox.ToolCapabilityReport{PolicyContext: "repository_inspection", Harness: harnessName, Role: role, Entries: []sandbox.ToolCapability{
				{ID: "workspace", Usable: true, Declared: true, Action: "must not appear"},
				{ID: "browser", Reason: "not_enabled"},
				{ID: "dependencies", Reason: "not_prepared"},
			}}
			want := fmt.Sprintf("Tool capabilities (repository_inspection; %s/%s): workspace=usable; browser=not_enabled; dependencies=not_prepared\n", harnessName, role)
			if got := formatDoctorToolCapabilitySummary(report); got != want {
				t.Fatalf("summary = %q; want %q", got, want)
			}
		}
	}
	for _, declared := range []bool{false, true} {
		for _, authorized := range []bool{false, true} {
			report := sandbox.ToolCapabilityReport{Entries: []sandbox.ToolCapability{}}
			var parts []string
			for i, reason := range []string{"canary_failed", "missing_dependency"} {
				entry := sandbox.ToolCapability{ID: fmt.Sprintf("tool%d", i), Reason: reason, Action: sandbox.CapabilityReason(reason), Declared: declared, Authorized: authorized}
				report.Entries = append(report.Entries, entry)
				part := entry.ID + "=" + reason
				if declared || authorized {
					part += " (" + entry.Action + ")"
				}
				parts = append(parts, part)
			}
			want := "Tool capabilities (; /): " + strings.Join(parts, "; ") + "\n"
			if got := formatDoctorToolCapabilitySummary(report); got != want {
				t.Fatalf("declared=%v authorized=%v: %q; want %q", declared, authorized, got, want)
			}
		}
	}
}

func doctorCapabilityFixture(t *testing.T) (string, []sandbox.ToolCapabilityReport) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".swarm"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "default_gate: none\nsecurity:\n  require_ready_label: true\n  ready_label: swarm:ready\nlease:\n  duration: 1h\nversion: 1\ntracker: local\nexecution:\n  mode: isolated\n  backend: native\ndefault_agent: codex\nagents:\n  codex:\n    binary: /no-such-swarmos-test-binary\n"
	if err := os.WriteFile(filepath.Join(root, ".swarm/config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	var reports []sandbox.ToolCapabilityReport
	for _, role := range []string{"planner", "developer", "tester", "reviewer"} {
		_, report, err := agent.InspectWithOptions("codex", cfg, true, role, agent.RunnerOptions{LookupEnv: func(string) (string, bool) { return "", false }}, "repository_inspection")
		if err == nil {
			t.Fatal("missing binary passed")
		}
		reports = append(reports, report)
	}
	return root, reports
}

func runDoctorCapabilityFixture(t *testing.T, root string, flags ...string) string {
	t.Helper()
	factory := func(context.Context, func(progress.Event)) harness.DoctorRunner {
		return func(category, name string, check func() harness.DoctorCheck) harness.DoctorCheck {
			if name == ".swarm/config.yaml" {
				check()
			}
			if strings.HasPrefix(name, "codex/") {
				return check()
			}
			return harness.DoctorCheck{Category: category, Name: name, Status: "OK", Detail: "fixture"}
		}
	}
	cmd := newDoctorCmdWithRunner(&RootOptions{RepoRoot: root, lookupEnv: func(string) (string, bool) { return "", false }}, factory)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(flags)
	err := cmd.ExecuteContext(context.Background())
	if err == nil || err.Error() != "doctor detected 4 failing check(s)" {
		t.Fatalf("error = %v; output: %s", err, out.String())
	}
	return out.String()
}

func TestDoctorCapabilityOutput(t *testing.T) {
	t.Parallel()
	root, reports := doctorCapabilityFixture(t)
	summary := runDoctorCapabilityFixture(t, root)
	full := runDoctorCapabilityFixture(t, root, "--capabilities")
	var wantSummary, wantFull strings.Builder
	for _, report := range reports {
		wantSummary.WriteString(formatDoctorToolCapabilitySummary(report))
		wantFull.WriteString(sandbox.FormatToolCapabilities(report))
		entry := report.Entries[0]
		if entry.Reason != "unavailable" || !entry.Authorized {
			t.Fatalf("fixture: %+v", entry)
		}
		if !strings.Contains(summary, "workspace=unavailable ("+sandbox.CapabilityReason("unavailable")+")") {
			t.Fatalf("hidden failure: %s", summary)
		}
	}
	summaryParts := strings.SplitN(summary, "CATEGORY", 2)
	fullParts := strings.SplitN(full, "CATEGORY", 2)
	if len(summaryParts) != 2 || len(fullParts) != 2 || summaryParts[0] != wantSummary.String() || fullParts[0] != wantFull.String() {
		t.Fatalf("unexpected reports:\n%s\n%s", summary, full)
	}
	if strings.Count(summaryParts[0], "\n") != len(reports) || summaryParts[1] != fullParts[1] || strings.Count(summaryParts[1], "FAIL") != 4 {
		t.Fatalf("table changed:\n%s\n%s", summary, full)
	}
}

func TestDoctorCapabilitiesJSON(t *testing.T) {
	t.Parallel()
	root, reports := doctorCapabilityFixture(t)
	plain := runDoctorCapabilityFixture(t, root, "--json")
	full := runDoctorCapabilityFixture(t, root, "--json", "--capabilities")
	if plain != full {
		t.Fatal("capabilities flag changed JSON")
	}
	var result struct {
		Status  string                         `json:"status"`
		Reports []sandbox.ToolCapabilityReport `json:"tool_capabilities"`
	}
	if err := json.Unmarshal([]byte(plain), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || !reflect.DeepEqual(result.Reports, reports) {
		t.Fatalf("JSON reports: %+v; want %+v", result, reports)
	}
	for _, capabilities := range []bool{false, true} {
		cmd := newDoctorCmdWithRunner(&RootOptions{RepoRoot: t.TempDir()}, func(context.Context, func(progress.Event)) harness.DoctorRunner {
			return func(category, name string, check func() harness.DoctorCheck) harness.DoctorCheck {
				if name == ".swarm/config.yaml" {
					check()
				}
				return harness.DoctorCheck{Category: category, Name: name, Status: "OK"}
			}
		})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--json", fmt.Sprintf("--capabilities=%t", capabilities)})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if string(data["tool_capabilities"]) != "[]" {
			t.Fatalf("empty reports: %s", out.String())
		}
	}
}

func TestDoctorRejectsReviewerOutsideReviewerAgents(t *testing.T) {
	cfg := &contract.SwarmConfig{
		DefaultAgent: "codex",
		Agents:       map[string]contract.AgentConfig{"codex": {}, "claude": {}},
		Security:     contract.SecurityConfig{ReviewerAgents: []string{"claude"}},
	}
	checks := doctorAgentChecks(cfg, false, func(string, *contract.SwarmConfig, bool, string) (string, error) {
		return "verified", nil
	})
	for _, check := range checks {
		if check.Name == "codex/reviewer" {
			if check.Status != "FAIL" || !strings.Contains(check.Detail, "security.reviewer_agents") {
				t.Fatalf("missing reviewer eligibility diagnostic: %+v", check)
			}
			return
		}
	}
	t.Fatal("missing selected reviewer check")
}

func TestDoctorPromptScaffoldStatus(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		wants        []string
	}{
		{"current", "OK", []string{"current"}},
		{"legacy with changed pin", "OK", []string{"declared revision 0"}},
		{"older", "WARN", []string{"older-managed", "swarm repo init --dry-run", "review and commit"}},
		{"mixed", "WARN", []string{"older-managed", "swarm repo init --dry-run", "tester.md (customized-or-unowned)", "Merge prompt updates manually"}},
		{"unowned", "WARN", []string{"planner.md (customized-or-unowned)", "Merge prompt updates manually"}},
		{"missing", "WARN", []string{"planner.md (missing)", "swarm repo init --dry-run"}},
		{"invalid", "WARN", []string{"manifest invalid", "planner.md (unverifiable)", "Inspect"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			prGit(t, root, "init", "-b", "main")
			if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root, SwarmVersion: "v0.6.0"}); err != nil {
				t.Fatal(err)
			}
			const planner = ".swarm/prompts/planner.md"
			const tester = ".swarm/prompts/tester.md"
			const old = "Old prompt"
			read := func(path string) string {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(root, path))
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.name {
			case "legacy with changed pin":
				write(harness.ManifestPath, `{"version":1,"swarm_version":"v9.0.0","files":{}}`)
				workflow := ".github/workflows/swarm-validate.yml"
				write(workflow, strings.ReplaceAll(read(workflow), "v0.6.0", "v9.0.0"))
			case "older", "mixed":
				write(planner, old)
				write(harness.ManifestPath, fmt.Sprintf(`{"version":1,"files":{%q:"%x"}}`, planner, sha256.Sum256([]byte(old))))
				if tc.name == "mixed" {
					write(tester, "Custom prompt")
				}
			case "unowned":
				write(planner, old)
				write(harness.ManifestPath, `{"version":1,"files":{}}`)
			case "missing":
				if err := os.Remove(filepath.Join(root, planner)); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				write(planner, old)
				write(harness.ManifestPath, "{")
			}
			manifestBefore, testerBefore := read(harness.ManifestPath), read(tester)
			for _, args := range [][]string{nil, {"--json"}} {
				factory := func(context.Context, func(progress.Event)) harness.DoctorRunner {
					return func(category, name string, check func() harness.DoctorCheck) harness.DoctorCheck {
						if name == "prompt-scaffold" || name == ".swarm/config.yaml" {
							return check()
						}
						return harness.DoctorCheck{Category: category, Name: name, Status: "OK"}
					}
				}
				cmd := newDoctorCmdWithRunner(&RootOptions{RepoRoot: root}, factory)
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(io.Discard)
				cmd.SetArgs(args)
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
				body := out.String()
				if len(args) != 0 {
					var result struct{ Checks []harness.DoctorCheck }
					if err := json.Unmarshal(out.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					body = ""
					for _, row := range result.Checks {
						if row.Name == "prompt-scaffold" {
							if row.Status != tc.status {
								t.Fatalf("scaffold status: %+v", row)
							}
							body = row.Detail
						}
					}
				} else if !strings.Contains(body, "prompt-scaffold") || !strings.Contains(body, tc.status) {
					t.Fatalf("missing scaffold row: %s", body)
				}
				for _, want := range tc.wants {
					if !strings.Contains(body, want) {
						t.Errorf("missing %q in %s", want, body)
					}
				}
			}
			if read(harness.ManifestPath) != manifestBefore || read(tester) != testerBefore {
				t.Fatal("doctor changed fixture files")
			}
		})
	}
}
