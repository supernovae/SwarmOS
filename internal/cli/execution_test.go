package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
	"gopkg.in/yaml.v3"
)

func TestDryRunReportsIsolationBlockersWithoutClaim(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	// Exercise a missing harness even when the developer has Codex installed.
	cfg, err := harness.LoadConfig(f.root)
	if err != nil {
		t.Fatal(err)
	}
	ac := cfg.Agents["codex"]
	ac.Binary = filepath.Join(t.TempDir(), "missing-codex")
	cfg.Agents["codex"] = ac
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, ".swarm", "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".swarm/config.yaml"}, {"commit", "-m", "configure unavailable test harness"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	f.setenv("SWARM_EXECUTION_MODE", "")
	var result struct {
		Plan workflow.PreflightResult `json:"effective_plan"`
	}
	if err := json.Unmarshal([]byte(f.must(t, "issue", "work", n, "--dry-run")), &result); err != nil {
		t.Fatal(err)
	}
	if result.Plan.Execution.Mode != contract.ExecutionIsolated || result.Plan.Execution.Backend != contract.BackendNative || len(result.Plan.Blockers) == 0 {
		t.Fatalf("missing default boundary diagnostics: %+v", result.Plan)
	}
	issue, _ := strconv.Atoi(n)
	if st := f.state(t, issue); st.Lease.Status == "active" || st.Lease.ClaimID != "" {
		t.Fatal("inspection acquired a lease")
	}
	if out, err := f.run("issue", "work", n); err == nil || !strings.Contains(out+err.Error(), "preflight") {
		t.Fatalf("work did not fail closed: %v %s", err, out)
	}
	if st := f.state(t, issue); st.Lease.Status == "active" || st.Lease.ClaimID != "" {
		t.Fatal("blocked execution acquired a lease")
	}
}

func TestInspectionCommandSessionPerExecution(t *testing.T) {
	t.Parallel()
	opts := &RootOptions{RepoRoot: t.TempDir(), lookupEnv: func(name string) (string, bool) {
		if name == "SWARM_NODE_CONFIG" {
			return filepath.Join(t.TempDir(), "node.yaml"), true
		}
		return "", false
	}}
	root := newRootCmdWithOptions(opts)
	var seen []*sandbox.InspectionSession
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, args []string) error {
		seen = append(seen, sandbox.InspectionSessionFrom(cmd.Context()))
		return nil
	}})
	for range 2 {
		root.SetArgs([]string{"probe"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] == nil || seen[1] == nil || seen[0] == seen[1] {
		t.Fatalf("commands did not each receive a fresh inspection session: %v", seen)
	}
}

func TestToolCapabilityOptionalFailureDryRun(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	for _, format := range []string{"json", "text"} {
		var output bytes.Buffer
		calls := 0
		cmd := f.command(&RootOptions{inspectCapabilityEntry: func(id string) error {
			if id == "browser" {
				calls++
				return fmt.Errorf("TOKEN /home/private-user/secret")
			}
			return nil
		}})
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{"--root", f.root, "issue", "work", n, "--dry-run", "--agent", "mock", "--format=" + format})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err, output.String())
		}
		if calls == 0 {
			t.Fatal("report assembler was not exercised")
		}
		if strings.Contains(output.String(), "TOKEN") || strings.Contains(output.String(), "/home/private-user") {
			t.Fatal("secret escaped")
		}
		if format == "json" {
			var result struct {
				Plan workflow.PreflightResult `json:"effective_plan"`
			}
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Plan.Blockers) > 0 {
				t.Fatal(result.Plan.Blockers)
			}
			for _, r := range result.Plan.ToolCapabilities {
				if r.Entries[1].Runtime != "not_probed" {
					t.Fatal(r)
				}
			}
		} else if !strings.Contains(output.String(), "runtime=not_probed") {
			t.Fatal(output.String())
		}
	}
	issue, _ := strconv.Atoi(n)
	if st := f.state(t, issue); st.Lease.ClaimID != "" {
		t.Fatal("dry-run acquired a claim")
	}
	_, err := f.run("issue", "work", n, "--dry-run", "--format=text", "--json")
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatal(err)
	}
}

func rolePreviewConfig(t *testing.T, f *cliFixture) *contract.SwarmConfig {
	t.Helper()
	f.setenv("SWARM_EXECUTION_MODE", "")
	cfg, err := harness.LoadConfig(f.root)
	if err != nil {
		t.Fatal(err)
	}
	guide, err := harness.OnboardingGuide()
	if err != nil {
		t.Fatal(err)
	}
	_, example, found := strings.Cut(guide, "```yaml\n")
	example, _, closed := strings.Cut(example, "\n```")
	if !found || !closed {
		t.Fatal("missing onboarding YAML example")
	}
	var fragment contract.SwarmConfig
	if err := yaml.Unmarshal([]byte(example), &fragment); err != nil {
		t.Fatal(err)
	}
	cfg.Roles = fragment.Roles
	maps.Copy(cfg.Agents, fragment.Agents)
	missing := t.TempDir()
	for name, ac := range cfg.Agents {
		ac.Binary = filepath.Join(missing, name)
		cfg.Agents[name] = ac
	}
	return cfg
}

func commitRolePreviewConfig(t *testing.T, f *cliFixture, cfg *contract.SwarmConfig) {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const path = ".swarm/config.yaml"
	if err := os.WriteFile(filepath.Join(f.root, path), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", path}, {"commit", "-m", "Set role preview fixture policy"}} {
		if out, err := gitfixture.Command(t, f.root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func readRolePreview(t *testing.T, f *cliFixture, issue int, flags ...string) workflow.PreflightResult {
	t.Helper()
	args := append([]string{"issue", "work", strconv.Itoa(issue), "--dry-run", "--format=json"}, flags...)
	var result struct {
		Plan workflow.PreflightResult `json:"effective_plan"`
	}
	if err := json.Unmarshal([]byte(f.must(t, args...)), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.RolePreview) != 4 || result.Plan.RolePolicy != "approved_task" {
		t.Fatalf("missing approved task role preview: %+v", result.Plan)
	}
	return result.Plan
}

func checkRolePreviewReadOnly(t *testing.T, f *cliFixture, issue int) {
	t.Helper()
	if st := f.state(t, issue); st.Lease.ClaimID != "" || st.Lease.Status == "active" {
		t.Fatal("dry-run acquired a claim")
	}
	if _, err := os.Stat(workspace.WorktreePath(f.root, issue)); !os.IsNotExist(err) {
		t.Fatalf("expected no task worktree, stat error = %v", err)
	}
}

func TestWorkRolePreviewApprovedBase(t *testing.T) {
	f := newCLIFixture(t)
	cfg := rolePreviewConfig(t, f)
	commitRolePreviewConfig(t, f, cfg)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	base := f.state(t, issue).BaseSHA

	cfg.Roles = nil
	cfg.DefaultAgent = "opencode-build"
	commitRolePreviewConfig(t, f, cfg)
	doctor, err := f.run("repo", "doctor")
	if err == nil {
		t.Fatal("doctor accepted unavailable binaries")
	}
	for _, want := range []string{"checkout policy; entry=opencode-build, source=default_agent", "harness=opencode, model=opencode-go/glm-5, effort=harness default"} {
		if !strings.Contains(doctor, want) {
			t.Fatalf("doctor missing %q: %s", want, doctor)
		}
	}
	text := f.must(t, "issue", "work", n, "--dry-run", "--format=text")
	for _, want := range []string{"approved task policy", "base=" + base, "reviewer: selected (entry=claude-review", "model=gpt-6.2-sol, effort=medium", "model=opencode-go/glm-5, effort=harness default"} {
		if !strings.Contains(text, want) {
			t.Fatalf("dry-run missing %q: %s", want, text)
		}
	}
	plan := readRolePreview(t, f, issue)
	if plan.BaseSHA != base {
		t.Fatalf("preview base = %s, want %s", plan.BaseSHA, base)
	}
	for i, want := range []struct{ role, entry, harness, model, effort string }{
		{"planner", "codex-plan", "codex", "gpt-6.2-sol", "medium"},
		{"reviewer", "claude-review", "claude", "claude-sonnet-4-6", ""},
		{"developer", "opencode-build", "opencode", "opencode-go/glm-5", ""},
		{"tester", "codex-test", "codex", "gpt-6.2-sol", "high"},
	} {
		row := plan.RolePreview[i]
		if row.Role != want.role || row.Disposition != "selected" || row.Entry != want.entry ||
			row.Source != "roles."+want.role || row.Harness != want.harness || row.Model != want.model || row.Effort != want.effort {
			t.Errorf("row = %+v, want %+v", row, want)
		}
	}
	plan = readRolePreview(t, f, issue, "--agent", "codex-test", "--planner-agent", "codex-plan")
	if planner, developer := plan.RolePreview[0], plan.RolePreview[2]; planner.Entry != "codex-plan" ||
		planner.Source != "--planner-agent" || developer.Entry != "codex-test" || developer.Source != "--agent" {
		t.Fatalf("overrides: %+v", plan.RolePreview)
	}
	checkRolePreviewReadOnly(t, f, issue)
}

func TestWorkRolePreviewManualAndSkipped(t *testing.T) {
	f := newCLIFixture(t)
	cfg := rolePreviewConfig(t, f)
	cfg.PlanMode = "interactive"
	commitRolePreviewConfig(t, f, cfg)
	issue := f.create(t, "human")
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	plan := readRolePreview(t, f, issue)
	for i, want := range []string{"manual", "manual", "deferred", "deferred"} {
		if row := plan.RolePreview[i]; row.Disposition != want || row.Reason == "" || row.Entry != "" {
			t.Errorf("row = %+v, want %q with reason", row, want)
		}
	}
	if len(plan.ToolCapabilities) != 0 || len(plan.Agents) != 1 || plan.Agents["planner"] != "interactive" {
		t.Fatalf("manual roles entered agent inspection: %+v", plan.Agents)
	}
	plan = readRolePreview(t, f, issue, "--phase", "plan")
	if row := plan.RolePreview[1]; row.Disposition != "skipped" || row.Reason != "outside the requested phase" {
		t.Fatalf("unexpected phase exclusion: %+v", row)
	}
	checkRolePreviewReadOnly(t, f, issue)

	resume, resumedIssue := issue466Fixture(t)
	resumed := readRolePreview(t, resume, resumedIssue, "--agent", "mock")
	if developer, tester := resumed.RolePreview[2], resumed.RolePreview[3]; developer.Disposition != "skipped" ||
		developer.Reason != "continuation reuses completed work" || tester.Disposition != "selected" {
		t.Fatalf("unexpected continuation preview: %+v", resumed.RolePreview)
	}
}

func TestWorkRolePreviewDiagnostics(t *testing.T) {
	f := newCLIFixture(t)
	cfg := rolePreviewConfig(t, f)
	ac := cfg.Agents["codex-plan"]
	ac.Roles = []string{"tester"}
	ac.Binary = "git" // A real executable lets role admission run before any harness probe.
	cfg.Agents["codex-plan"] = ac
	commitRolePreviewConfig(t, f, cfg)
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	plan := readRolePreview(t, f, issue, "--phase", "plan")
	if len(plan.Blockers) != 1 || !strings.Contains(plan.Blockers[0], `planner agent preflight (approved task policy): agent "codex-plan" is not permitted for role "planner"`) ||
		!strings.Contains(plan.Blockers[0], "choose an eligible entry") || plan.RolePreview[0].Entry != "codex-plan" {
		t.Fatalf("missing role denial: %+v", plan)
	}
	if out, err := f.run("issue", "work", n, "--phase", "plan"); err == nil || !strings.Contains(out+err.Error(), "not permitted") {
		t.Fatalf("work accepted disallowed role: %v %s", err, out)
	}
	for _, tc := range []struct{ name, effort, diagnostic string }{
		{"opencode-build", "high", "effort is supported only for claude and codex"},
		{"codex-test", "turbo", "must be one of minimal, low, medium, high"},
	} {
		entry := cfg.Agents[tc.name]
		invalid := entry
		invalid.Effort = tc.effort
		cfg.Agents[tc.name] = invalid
		commitRolePreviewConfig(t, f, cfg)
		for _, args := range [][]string{{"repo", "doctor"}, {"issue", "work", n}} {
			out, err := f.run(args...)
			if err == nil || !strings.Contains(out+err.Error(), tc.diagnostic) || !strings.Contains(out+err.Error(), tc.name) {
				t.Fatalf("%v did not reject invalid effort: %v %s", args, err, out)
			}
		}
		cfg.Agents[tc.name] = entry
	}
	checkRolePreviewReadOnly(t, f, issue)
}
