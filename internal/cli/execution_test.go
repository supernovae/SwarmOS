package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
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
	if err := os.WriteFile(filepath.Join(f.root, "package.json"), []byte(`{"name":"approved","version":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".swarm/config.yaml", "package.json"}, {"commit", "-m", "configure unavailable test harness"}} {
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
	if result.Plan.Readiness == nil || len(result.Plan.Readiness.Rows) == 0 || result.Plan.Readiness.Input != "approved base "+result.Plan.BaseSHA || result.Plan.Readiness.Outcome != "unverified" {
		t.Fatalf("missing approved readiness report: %+v", result.Plan.Readiness)
	}
	for _, row := range result.Plan.Readiness.Rows {
		if row.Status != "unverified" || row.Required {
			t.Fatalf("dry-run executed a probe: %+v", row)
		}
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

func TestWorkRoleRoutingDrift(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux native routing")
	}
	for _, scenario := range []string{"preview_and_launch", "reapprove", "no_drift", "unavailable", "advertised_default"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCLIFixture(t)
			issue := f.create(t, "none")
			cfg := rolePreviewConfig(t, f)
			cfg.Tracker = "github"
			cfg.DefaultAgent, cfg.PlanMode, cfg.Roles = "opencode", "agent", nil
			cfg.Agents["opencode"] = contract.AgentConfig{Binary: "opencode"}
			if scenario != "advertised_default" {
				cfg.BaseBranch = "release"
			}
			commitRolePreviewConfig(t, f, cfg)
			remote := filepath.Join(t.TempDir(), "upstream.git")
			prGit(t, f.root, "init", "--bare", "--initial-branch=trunk", remote)
			prGit(t, f.root, "remote", "add", "upstream", remote)
			prGit(t, f.root, "push", "upstream", "HEAD:trunk", "HEAD:release")
			n := strconv.Itoa(issue)
			tr := setupGitHubTracker{f.tracker}
			manager := lease.NewManager(tr, cfg.Security)
			manager.SetUpstreamRemote("upstream")
			if err := manager.ConfigureStore(f.root); err != nil {
				t.Fatal(err)
			}
			cmdInspector := func(string, *contract.SwarmConfig, bool, string, agent.RunnerOptions, string) (string, sandbox.ToolCapabilityReport, error) {
				return "fixture-agent", sandbox.ToolCapabilityReport{}, nil
			}
			run := func(args ...string) (string, string, error) {
				cmd := f.command(&RootOptions{RepoRoot: f.root, UpstreamRemote: "upstream", trackerOverride: tr, inspectAgent: cmdInspector})
				var out, stderr bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&stderr)
				cmd.SetArgs(args)
				err := cmd.Execute()
				return out.String(), stderr.String(), err
			}
			must := func(args ...string) string {
				t.Helper()
				out, stderr, err := run(args...)
				if err != nil {
					t.Fatalf("%v: %v\n%s\n%s", args, err, out, stderr)
				}
				return out
			}
			state := func() *lease.IssueState {
				t.Helper()
				st, err := manager.GetCurrentState(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			must("issue", "approve", n)
			initial := state()
			base := initial.BaseSHA
			branch := "release"
			if scenario == "advertised_default" {
				branch = "trunk"
				if initial.BaseBranch != "" {
					t.Fatalf("expected no explicit target, got %q", initial.BaseBranch)
				}
			}
			preview := func(format, phase string, extra ...string) string {
				t.Helper()
				return must(append([]string{"issue", "work", n, "--dry-run", "--format=" + format, "--phase=" + phase}, extra...)...)
			}
			parse := func(phase string, extra ...string) workflow.PreflightResult {
				t.Helper()
				var envelope struct {
					Plan workflow.PreflightResult `json:"effective_plan"`
				}
				if err := json.Unmarshal([]byte(preview("json", phase, extra...)), &envelope); err != nil {
					t.Fatal(err)
				}
				return envelope.Plan
			}
			wantText := func(text string, pieces ...string) {
				t.Helper()
				for _, piece := range pieces {
					if !strings.Contains(text, piece) {
						t.Fatalf("missing %q in output:\n%s", piece, text)
					}
				}
			}
			noDrift := func() {
				t.Helper()
				text := preview("text", "plan")
				wantText(text, "planner: selected (entry=opencode, source=default_agent")
				if plan := parse("plan"); plan.RoleRoutingComparison != nil || plan.RolePreview[0].Entry != "opencode" || strings.Contains(text, "Role routing") {
					t.Fatalf("unexpected routing advisory: %+v\n%s", plan.RoleRoutingComparison, text)
				}
			}
			acquire := func() {
				t.Helper()
				st, err := manager.Acquire(t.Context(), issue, "alice", "opencode", "plan", time.Hour, cfg.Lease.MaxRenewals)
				if err != nil {
					t.Fatal(err)
				}
				if err := lease.SaveClaimToken(f.root, issue, st.Lease.ClaimID, st.BaseSHA); err != nil {
					t.Fatal(err)
				}
			}
			launch := func(warn bool) {
				t.Helper()
				calls := 0
				var out, stderr bytes.Buffer
				f.resolverRunner = func(name string, approved *contract.SwarmConfig, local bool) (agent.Runner, error) {
					calls++
					if warn {
						wantText(stderr.String(), "Warning: Role routing drift on "+branch, base, "After any active claim is released, run swarm issue approve "+n)
					} else if strings.Contains(stderr.String(), "Role routing") {
						t.Fatalf("unexpected execution warning: %s", &stderr)
					}
					if name != "opencode" || approved.DefaultAgent != "opencode" || approved.Roles["planner"] != "" || local {
						t.Fatalf("runner received %q with config %+v", name, approved.Roles)
					}
					return &agent.MockRunner{}, nil
				}
				cmd := f.command(&RootOptions{RepoRoot: f.root, UpstreamRemote: "upstream", trackerOverride: tr, inspectAgent: cmdInspector})
				cmd.SetOut(&out)
				cmd.SetErr(&stderr)
				cmd.SetArgs([]string{"issue", "work", n, "--phase=plan", "--headless", "--progress=false", "--json"})
				if err := cmd.Execute(); err != nil || calls != 1 || !json.Valid(out.Bytes()) {
					t.Fatalf("planner calls=%d, error=%v\n%s\n%s", calls, err, &out, &stderr)
				}
			}

			if scenario == "reapprove" || scenario == "unavailable" || scenario == "preview_and_launch" {
				acquire()
			}
			if scenario == "no_drift" {
				path := filepath.Join(f.root, ".swarm/config.yaml")
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Roles = map[string]string{"planner": "codex-plan"}
				edited, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, edited, 0600); err != nil {
					t.Fatal(err)
				}
				noDrift()
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
				cfg.Roles = map[string]string{"planner": "opencode"}
			} else {
				cfg.Roles = map[string]string{"planner": "codex-plan", "developer": "codex-test"}
			}
			commitRolePreviewConfig(t, f, cfg)
			target := prGit(t, f.root, "rev-parse", "HEAD")
			prGit(t, f.root, "push", "upstream", "HEAD:"+branch)

			switch scenario {
			case "no_drift":
				noDrift()
			case "unavailable":
				before := state()
				if _, _, err := workspace.ResolveTrustedTargetBranch(t.Context(), f.root, "upstream", branch); err != nil {
					t.Fatal(err)
				}
				prGit(t, remote, "update-ref", "-d", "refs/heads/"+branch)
				assertUnavailable := func(reason string) {
					t.Helper()
					wantText(preview("text", "plan"), "Role routing comparison unavailable", reason, "planner: selected (entry=opencode")
					plan := parse("plan")
					c := plan.RoleRoutingComparison
					if c == nil || !c.Unavailable || c.Reason != reason || len(plan.Blockers) != 0 || plan.RolePreview[0].Entry != "opencode" {
						t.Fatalf("incorrect unavailable preview: %+v, blockers=%v", c, plan.Blockers)
					}
					after := state()
					if after.BaseSHA != before.BaseSHA || after.PolicyDigest != before.PolicyDigest || after.Lease.Status != before.Lease.Status || after.Lease.ClaimID != before.Lease.ClaimID {
						t.Fatal("preview changed approved state or claim")
					}
				}
				assertUnavailable("trusted target branch could not be resolved")
				prGit(t, f.root, "rm", ".swarm/config.yaml")
				prGit(t, f.root, "commit", "-m", "remove target config")
				prGit(t, f.root, "push", "upstream", "HEAD:"+branch)
				prGit(t, f.root, "restore", "--source="+target, "--", ".swarm/config.yaml")
				assertUnavailable("trusted target branch config could not be loaded")
				launch(false)
			case "reapprove":
				must("issue", "release", n)
				must("issue", "approve", n)
				plan := parse("plan")
				if plan.BaseSHA != target || plan.RoleRoutingComparison != nil || plan.RolePreview[0].Entry != "codex-plan" || plan.RolePreview[0].Source != "roles.planner" {
					t.Fatalf("reapproved preview: base=%s, comparison=%+v, planner=%+v", plan.BaseSHA, plan.RoleRoutingComparison, plan.RolePreview[0])
				}
			default:
				guidance := "After any active claim is released, run swarm issue approve " + n
				text := preview("text", "plan")
				wantText(text, "Role routing drift on "+branch, base, target, "effective role routing differs",
					"planner: selected (entry=opencode, source=default_agent", "target (entry=codex-plan, source=roles.planner", guidance)
				plan := parse("plan")
				c := plan.RoleRoutingComparison
				if c == nil || c.Branch != branch || c.ApprovedSHA != base || c.TargetSHA != target ||
					!strings.Contains(c.Reason, "effective role routing differs") || !strings.Contains(c.Guidance, guidance) ||
					len(c.Changes) != 1 || plan.RolePreview[0].Entry != "opencode" {
					t.Fatalf("incorrect plan advisory: %+v", c)
				}
				change := c.Changes[0]
				if change.Approved.Role != "planner" || change.Approved.Entry != "opencode" || change.Approved.Source != "default_agent" ||
					change.Approved.Harness != "opencode" || change.Target.Entry != "codex-plan" || change.Target.Source != "roles.planner" || change.Target.Harness != "codex" {
					t.Fatalf("incorrect planner change: %+v", change)
				}
				if scenario == "advertised_default" {
					return
				}
				impl := parse("implement").RoleRoutingComparison
				if impl == nil || len(impl.Changes) != 1 || impl.Changes[0].Approved.Role != "developer" {
					t.Fatalf("incorrect implementation advisory: %+v", impl)
				}
				masked := parse("plan", "--planner-agent=opencode", "--agent=codex-plan")
				if masked.RoleRoutingComparison != nil || masked.RolePreview[0].Source != "--planner-agent" {
					t.Fatalf("incorrect override preview: %+v", masked.RolePreview[0])
				}
				if after := state(); after.BaseSHA != base || after.PolicyDigest != initial.PolicyDigest || after.Lease.Status != "active" {
					t.Fatal("preview changed approved state or claim")
				}
				launch(true)
				must("issue", "release", n)
				f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
					t.Fatal("fresh claim started a runner after policy changed")
					return nil, nil
				}
				_, stderr, err := run("issue", "work", n, "--phase=plan", "--headless", "--progress=false")
				if err == nil || !strings.Contains(err.Error(), "committed policy tree changed") {
					t.Fatalf("expected fresh claim refusal, got %v (%s)", err, stderr)
				}
				wantText(stderr, "Warning: Role routing drift on "+branch, guidance)
			}
		})
	}
}

type promptScaffoldRecordingRunner struct {
	agent.MockRunner
	observe func(agent.RunRequest)
}

func (r *promptScaffoldRecordingRunner) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	r.observe(req)
	return r.MockRunner.Run(ctx, req)
}

func TestWorkPromptScaffoldAdvisory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux native execution policy")
	}
	for _, driver := range []string{"codex", "claude"} {
		t.Run(driver, func(t *testing.T) {
			f := newCLIFixture(t)
			cfg := rolePreviewConfig(t, f)
			cfg.DefaultAgent, cfg.PlanMode, cfg.Roles = driver, "agent", nil
			cfg.BaseBranch = "main"
			commitRolePreviewConfig(t, f, cfg)

			const prompt = ".swarm/prompts/planner.md"
			const approvedMarker = "# Approved old planner prompt"
			const checkoutMarker = "# Checkout-only planner prompt"
			path := filepath.Join(f.root, prompt)
			metaPath := filepath.Join(f.root, harness.ManifestPath)
			read := func(path string) []byte {
				t.Helper()
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			current, meta := read(path), read(metaPath)
			write(path, []byte(approvedMarker))
			write(metaPath, []byte(fmt.Sprintf(`{"version":1,"files":{%q:"%x"}}`, prompt, sha256.Sum256([]byte(approvedMarker)))))
			prGit(t, f.root, "add", prompt, harness.ManifestPath)
			prGit(t, f.root, "commit", "-m", "Earlier managed scaffold")
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			base := f.state(t, issue).BaseSHA

			var runnerCalls, resolverCalls int
			var out, stderr bytes.Buffer
			run := func(args ...string) error {
				out.Reset()
				stderr.Reset()
				opts := &RootOptions{RepoRoot: f.root, inspectAgent: func(name string, cfg *contract.SwarmConfig, local bool, role string, opts agent.RunnerOptions, _ string) (string, sandbox.ToolCapabilityReport, error) {
					execution, err := contract.ResolveExecution(cfg, "")
					if err != nil || execution.Mode != contract.ExecutionIsolated || execution.Backend != contract.BackendNative {
						t.Fatalf("expected isolated Linux native policy: %+v, %v", execution, err)
					}
					return name, sandbox.ToolCapabilityReport{}, nil
				}}
				cmd := f.command(opts)
				cmd.SetOut(&out)
				cmd.SetErr(&stderr)
				cmd.SetArgs(append([]string{"issue", "work", n, "--phase=plan"}, args...))
				return cmd.Execute()
			}
			mustRun := func(args ...string) {
				t.Helper()
				if err := run(args...); err != nil {
					t.Fatalf("%v: %v\n%s\n%s", args, err, &out, &stderr)
				}
			}
			assertText := func(text string, wants ...string) {
				t.Helper()
				for _, want := range wants {
					if !strings.Contains(text, want) {
						t.Fatalf("missing %q in %s", want, text)
					}
				}
			}
			preview := func(extra ...string) *workflow.PromptScaffoldReport {
				t.Helper()
				prevRunnerCalls, prevResolverCalls := runnerCalls, resolverCalls
				mustRun(append([]string{"--dry-run", "--format=json"}, extra...)...)
				var envelope struct {
					Plan workflow.PreflightResult `json:"effective_plan"`
				}
				if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				if runnerCalls != prevRunnerCalls || resolverCalls != prevResolverCalls || len(envelope.Plan.Blockers) != 0 {
					t.Fatalf("unexpected runner or blockers during preview: %+v", envelope.Plan)
				}
				return envelope.Plan.PromptScaffold
			}
			f.resolverRunner = func(name string, approved *contract.SwarmConfig, local bool) (agent.Runner, error) {
				resolverCalls++
				if name != driver || approved.DefaultAgent != driver {
					t.Fatalf("route=%q, approved default=%q", name, approved.DefaultAgent)
				}
				return &promptScaffoldRecordingRunner{observe: func(req agent.RunRequest) {
					runnerCalls++
					promptBytes := read(filepath.Join(req.WorktreePath, req.SystemPrompt))
					assertText(string(promptBytes), approvedMarker)
					if strings.Contains(string(promptBytes), checkoutMarker) {
						t.Fatal("checkout content reached the compiled prompt")
					}
					assertText(stderr.String(), "Warning: Prompt scaffold", "approved commit "+f.state(t, issue).BaseSHA, "swarm issue approve "+n)
				}}, nil
			}

			claim, err := f.manager.Acquire(t.Context(), issue, "alice", driver, "plan", time.Hour, cfg.Lease.MaxRenewals)
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.SaveClaimToken(f.root, issue, claim.Lease.ClaimID, claim.BaseSHA); err != nil {
				t.Fatal(err)
			}
			write(path, current)
			write(metaPath, meta)
			before, refs := f.state(t, issue), prGit(t, f.root, "show-ref")
			mustRun("--dry-run", "--format=text")
			assertText(out.String(), "checkout: current", "approved commit "+base+": upgrade-needed",
				"target branch", "swarm issue release "+n, "swarm issue approve "+n)
			report := preview()
			if report == nil || report.Checkout.Status != "current" || report.Approved.Status != "upgrade-needed" || report.ApprovedSHA != base {
				t.Fatalf("incorrect scaffold report: %+v", report)
			}
			assertText(strings.Join(report.NextActions, " "), "target branch", "swarm issue release "+n, "swarm issue approve "+n)
			after := f.state(t, issue)
			beforeState, _ := json.Marshal(before)
			afterState, _ := json.Marshal(after)
			if !bytes.Equal(beforeState, afterState) || refs != prGit(t, f.root, "show-ref") ||
				!bytes.Equal(current, read(path)) || !bytes.Equal(meta, read(metaPath)) || runnerCalls != 0 || resolverCalls != 0 {
				t.Fatal("preview changed fixture state")
			}
			if report := preview("--agent=mock"); report != nil {
				t.Fatalf("non-target route reported scaffold: %+v", report)
			}
			write(path, []byte(checkoutMarker))
			mustRun("--headless", "--progress=false", "--json")
			if !json.Valid(out.Bytes()) || runnerCalls != 1 {
				t.Fatalf("ordinary launch calls=%d: %s", runnerCalls, &out)
			}
			f.must(t, "issue", "release", n)

			write(path, current)
			write(metaPath, meta)
			prGit(t, f.root, "add", prompt, harness.ManifestPath)
			prGit(t, f.root, "commit", "-m", "Upgrade scaffold")
			f.must(t, "issue", "approve", n)
			currentBase := f.state(t, issue).BaseSHA
			write(path, []byte(checkoutMarker))
			report = preview()
			if report == nil || report.Checkout.Status != "manual-review" || report.Approved.Status != "current" || report.ApprovedSHA != currentBase ||
				strings.Contains(strings.Join(report.NextActions, " "), "swarm issue approve") {
				t.Fatalf("checkout-only customization: %+v", report)
			}

			write(metaPath, []byte("{"))
			prGit(t, f.root, "add", prompt, harness.ManifestPath)
			prGit(t, f.root, "commit", "-m", "Save custom prompt with invalid metadata")
			f.must(t, "issue", "approve", n)
			report = preview()
			if report == nil || report.Approved.Status != "unverifiable" || report.ApprovedSHA != f.state(t, issue).BaseSHA {
				t.Fatalf("unverifiable approved scaffold: %+v", report)
			}
			assertText(strings.Join(report.NextActions, " "), "Inspect", "swarm issue approve "+n)
		})
	}

	t.Run("fresh claim advances base", func(t *testing.T) {
		f := newCLIFixture(t)
		cfg := rolePreviewConfig(t, f)
		cfg.DefaultAgent, cfg.PlanMode, cfg.Roles = "codex", "agent", nil
		cfg.BaseBranch = "main"
		commitRolePreviewConfig(t, f, cfg)
		const prompt = ".swarm/prompts/planner.md"
		if err := os.WriteFile(filepath.Join(f.root, prompt), []byte("# Older approved prompt"), 0o644); err != nil {
			t.Fatal(err)
		}
		prGit(t, f.root, "add", prompt)
		prGit(t, f.root, "commit", "-m", "Older scaffold")
		issue := f.create(t, "agent")
		n := strconv.Itoa(issue)
		f.must(t, "issue", "approve", n)
		oldBase := f.state(t, issue).BaseSHA
		prGit(t, f.root, "commit", "--allow-empty", "-m", "Advance target without a policy change")
		tip := prGit(t, f.root, "rev-parse", "HEAD")
		var out, stderr bytes.Buffer
		calls := 0
		f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
			calls++
			if !strings.Contains(stderr.String(), "Prompt scaffold approved commit "+tip+": unverifiable") ||
				strings.Contains(stderr.String(), "Prompt scaffold approved commit "+oldBase) {
				t.Fatalf("warning did not reflect the final base: %s", &stderr)
			}
			return &agent.MockRunner{}, nil
		}
		cmd := f.command(&RootOptions{RepoRoot: f.root, inspectAgent: func(name string, _ *contract.SwarmConfig, _ bool, _ string, _ agent.RunnerOptions, _ string) (string, sandbox.ToolCapabilityReport, error) {
			return name, sandbox.ToolCapabilityReport{}, nil
		}})
		cmd.SetOut(&out)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"issue", "work", n, "--phase=plan", "--headless", "--progress=false", "--json"})
		if err := cmd.Execute(); err != nil || calls != 1 || !json.Valid(out.Bytes()) {
			t.Fatalf("advanced base launch calls=%d err=%v\n%s\n%s", calls, err, &out, &stderr)
		}
	})
}
