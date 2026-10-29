package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestIssue83GuideCommandRegistration(t *testing.T) {
	commands := []string{
		"issue claim 83", "issue status 83", "issue stage 83 --phase plan --harness codex --model default",
		"issue review-plan 83", "issue review-plan 83 --approve --notes Reviewed",
		"issue validate 83", "issue stage 83 --phase implement --harness codex --model default",
		"issue renew 83", "issue submit 83", "pr status 321", "pr work 321 --phase plan", "pr work 321 --phase review",
		"pr stage 321 --phase plan --harness codex --model default", "pr validate 321",
		"pr stage 321 --phase implement --harness codex --model default", "pr submit 321",
	}
	validate := func(argv []string) error {
		root := NewRootCmd()
		cmd, args, err := root.Find(argv)
		if err != nil {
			return err
		}
		if err := cmd.ParseFlags(args); err != nil {
			return err
		}
		if cmd.Args != nil {
			return cmd.Args(cmd, cmd.Flags().Args())
		}
		if cmd.RunE == nil && cmd.Run == nil {
			return fmt.Errorf("not an executable command: %s", cmd.CommandPath())
		}
		return nil
	}
	for _, command := range commands {
		for _, selectors := range []string{"", "--fork ", "--backend github --upstream-remote upstream "} {
			argv := strings.Fields(command + " " + selectors)
			t.Run(strings.Join(argv, " "), func(t *testing.T) {
				if err := validate(argv); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	for _, command := range []string{"issue stage 83 --invented-issue83", "pr renew 321", "pr review-plan 321"} {
		if err := validate(strings.Fields(command)); err == nil {
			t.Fatalf("invalid command accepted: %s", command)
		}
	}
	guide, err := os.ReadFile("../../docs/human-led.md")
	if err != nil {
		t.Fatal(err)
	}
	examples := regexp.MustCompile("(?m)^swarm [^\n]+$|`swarm [^`]+`").FindAllString(string(guide), -1)
	if len(examples) < 20 {
		t.Fatal("guide command inventory unexpectedly empty")
	}
	words := regexp.MustCompile(`'[^']*'|[^\s]+`)
	for _, example := range examples {
		example = strings.Trim(example, "`")
		argv := words.FindAllString(strings.TrimPrefix(example, "swarm "), -1)
		for i := range argv {
			argv[i] = strings.Trim(argv[i], "'")
		}
		if err := validate(argv); err != nil {
			t.Fatalf("guide command %s: %v", example, err)
		}
	}

	for _, dir := range []string{".claude/commands", ".opencode/commands"} {
		for _, name := range []string{"swarm-plan.md", "swarm-implement.md"} {
			if _, err := os.Stat(filepath.Join("../..", dir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestIssue83CLIStagingStatusAndLocalPublication(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	wt := filepath.Join(f.root, ".swarm", "worktrees", "issue-"+n)
	for name, body := range map[string]string{"PLAN.md": "# Hands-on plan", "TEST_PLAN.md": "# Hands-on tests"} {
		if err := os.WriteFile(filepath.Join(wt, ".swarm", "active", name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.must(t, "issue", "stage", n, "--phase", "plan", "--harness", "codex", "--model", "default")
	f.must(t, "issue", "review-plan", n, "--agent", "mock")
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.must(t, "issue", "stage", n, "--phase", "implement", "--harness", "codex", "--model", "default")
	st := f.state(t, number)
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactReview {
			continue
		}
		if a.Metadata["production_mode"] != "interactive" || a.Metadata["staged_by"] != "alice" || a.Metadata["harness_source"] != "self-reported" || a.Metadata["model_source"] != "self-reported" {
			t.Fatal(a)
		}
	}
	status := f.must(t, "issue", "status", n)
	if !strings.Contains(status, "staged interactively by alice") {
		t.Fatal(status)
	}
	var statusJSON any
	if err := json.Unmarshal([]byte(f.must(t, "issue", "status", n, "--json")), &statusJSON); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(st)
	f.must(t, "issue", "validate", n)
	issue83AssertStandaloneTester(t, f.state(t, number))
	f.must(t, "issue", "submit", n)
	issue83AssertStandaloneTester(t, f.state(t, number))
	after := f.state(t, number)
	pr, err := f.tracker.FindDraftPR(t.Context(), after.Branch, "main")
	if err != nil || pr == nil {
		t.Fatal(pr, err)
	}
	if strings.Contains(pr.Body, "alice") || !strings.Contains(pr.Body, "staged interactively") {
		t.Fatal(pr.Body)
	}
	start := strings.Index(pr.Body, "```json\n")
	if start < 0 {
		t.Fatal("missing JSON inventory")
	}
	jsonBody := strings.SplitN(pr.Body[start+8:], "\n```", 2)[0]
	var inventory struct {
		Roles []map[string]any `json:"roles"`
	}
	if err := json.Unmarshal([]byte(jsonBody), &inventory); err != nil {
		t.Fatal(err, jsonBody)
	}
	for _, role := range inventory.Roles {
		if _, ok := role["staged_by"]; ok {
			t.Fatal("local actor field was encoded rather than omitted", role)
		}
	}
	// Submission may update checkpoints; rendering itself must be read-only.
	_ = formatDraftPRBody(&contract.WorkUnit{IssueNumber: number}, st, "ref")
	rendered, _ := json.Marshal(st)
	if string(before) != string(rendered) {
		t.Fatal("rendering changed state")
	}
	raw := lease.PublicProvenanceAttestation(nil, st, lease.ProjectionContext{})
	if len(raw.Roles) == 0 {
		t.Fatal("inventory removed")
	}
	for _, role := range raw.Roles {
		if role.StagedBy != "" {
			t.Fatal(role)
		}
	}
}

// The trusted tracker double supplies the authenticated account through the
// same CurrentUser API as the GitHub backend; artifact payloads cannot set it.
type issue83GitHubCLITracker struct {
	tracker.Tracker
	test *testing.T
	root string
}

func (tr issue83GitHubCLITracker) UpstreamRepo() string { return "owner/repo" }
func (tr issue83GitHubCLITracker) GetPullRequest(ctx context.Context, number int) (*tracker.PullRequest, error) {
	p, err := tr.Tracker.GetPullRequest(ctx, number)
	if err != nil {
		return nil, err
	}
	p.HeadRepo = "owner/repo"
	p.BaseRepo = "owner/repo"
	if tr.test != nil {
		p.HeadSHA = prGit(tr.test, tr.root, "rev-parse", "refs/remotes/origin/"+p.Head)
	}
	return p, nil
}

func (issue83GitHubCLITracker) BackendName() string { return "github (owner/repo)" }

func TestIssue83AuthenticatedIssueSubmitPublishesStager(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "human")
	n := strconv.Itoa(number)
	remote := filepath.Join(t.TempDir(), "origin.git")
	prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
	prGit(t, f.root, "remote", "add", "origin", remote)
	prGit(t, f.root, "push", "origin", "main")
	f.setenv("SWARM_USER", "authenticated-github-stager")
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	raw, _, err := f.manager.Store().Load(t.Context(), number)
	if err != nil {
		t.Fatal(err)
	}
	store := trustedLedgerStore(t, f.root, "")
	if _, err := store.CompareAndSwap(t.Context(), number, "", raw); err != nil {
		t.Fatal(err)
	}

	tr := issue83GitHubCLITracker{Tracker: f.tracker.WithUser("authenticated-github-stager", "OWNER"), test: t, root: f.root}
	run := func(args ...string) string {
		t.Helper()
		activeTracker := tr
		if len(args) > 1 && args[1] == "review-plan" {
			activeTracker = issue83GitHubCLITracker{Tracker: f.tracker.WithUser("authenticated-independent-reviewer", "OWNER"), test: t, root: f.root}
		}
		cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "github", trackerOverride: activeTracker})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(t.Context()); err != nil {
			t.Fatalf("%v: %v %s", args, err, out.String())
		}
		return out.String()
	}

	wt := filepath.Join(f.root, ".swarm", "worktrees", "issue-"+n)
	for name, body := range map[string]string{"PLAN.md": "# Authenticated plan", "TEST_PLAN.md": "# Authenticated tests"} {
		if err := os.WriteFile(filepath.Join(wt, ".swarm", "active", name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("issue", "stage", n, "--phase", "plan", "--harness", "codex", "--model", "default")
	run("issue", "review-plan", n, "--approve", "--notes", "Reviewed")
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("issue", "stage", n, "--phase", "implement", "--harness", "codex", "--model", "default")
	run("issue", "submit", n)
	manager := lease.NewManager(tr, contract.SecurityConfig{})
	if err := manager.ConfigureStore(f.root); err != nil {
		t.Fatal(err)
	}
	st, err := manager.GetCurrentState(t.Context(), number)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := tr.GetPullRequest(t.Context(), st.PRNumber)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pr.Body, "staged interactively by authenticated-github-stager") || !strings.Contains(pr.Body, `"staged_by": "authenticated-github-stager"`) {
		t.Fatal(pr.Body)
	}
	start := strings.Index(pr.Body, "```json\n")
	if start < 0 {
		t.Fatal("missing JSON")
	}
	jsonBody := strings.SplitN(pr.Body[start+8:], "\n```", 2)[0]
	var att contract.ProvenanceAttestation
	if err := json.Unmarshal([]byte(jsonBody), &att); err != nil {
		t.Fatal(err, jsonBody)
	}
	for _, role := range att.Roles {
		if role.ProductionMode == "interactive" && role.StagedBy != "authenticated-github-stager" {
			t.Fatal(role)
		}
	}
	if strings.Contains(pr.Body, "alice") {
		t.Fatal("local alias escaped into GitHub publication")
	}

	// Provision the interactive PR checkout through the same fenced manager and
	// workspace operations as PR work, without launching a paid harness.
	owner := "authenticated-github-stager"
	claimed, err := manager.AcquirePR(t.Context(), number, pr.Number, owner, "claude", "all", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	manager.BindClaim(number, claimed.Lease.ClaimID)
	path, branch, err := workspace.PreparePR(t.Context(), f.root, workspace.PRWorkspaceOptions{Number: pr.Number, Remote: "origin", HeadBranch: pr.Head, HeadSHA: pr.HeadSHA, BaseSHA: claimed.BaseSHA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginPRRevision(t.Context(), number, owner, pr.Number, pr.HeadSHA, pr.HeadSHA, branch); err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveClaimToken(f.root, number, claimed.Lease.ClaimID, claimed.BaseSHA); err != nil {
		t.Fatal(err)
	}
	_, unit, err := loadWorkUnit(t.Context(), tr, number)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := harness.LoadWorkflowAt(f.root, unit.Workflow, claimed.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.CompilePRHarnessBundleAt(path, unit, wf, nil, "planner", claimed.BaseSHA, pr.Number); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"PLAN.md": "# Authenticated revision plan", "TEST_PLAN.md": "# Authenticated revision tests"} {
		if err := os.WriteFile(filepath.Join(path, ".swarm", "active", name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := strconv.Itoa(pr.Number)
	run("pr", "stage", p, "--phase", "plan", "--harness", "codex", "--model", "default")
	run("issue", "review-plan", n, "--approve", "--notes", "Reviewed revision")
	if err := os.WriteFile(filepath.Join(path, "revision.txt"), []byte("revision\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("pr", "stage", p, "--phase", "implement", "--harness", "codex", "--model", "default")
	run("pr", "validate", p)
	run("pr", "submit", p)
	updated, err := tr.GetPullRequest(t.Context(), pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated.Body, "staged interactively by authenticated-github-stager") || !strings.Contains(updated.Body, "human review by authenticated-independent-reviewer") {
		t.Fatal(updated.Body)
	}
}

func issue83AssertStandaloneTester(t *testing.T, st *lease.IssueState) {
	t.Helper()
	found := false
	for _, att := range st.Attestations {
		if att.Role == "tester" {
			found = true
			if att.ProductionMode != "" || att.StagedBy != "" || att.HarnessSource != "" || att.ModelSource != "" || att.ExecutionMode != "" || att.Authentication != "" {
				t.Fatal("stale tester attribution", att)
			}
		}
	}
	if !found {
		t.Fatal("tester provenance lost")
	}
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactValidation {
			for _, key := range []string{"production_mode", "staged_by", "harness_source", "model_source", "execution_mode", "authentication"} {
				if a.Metadata[key] != "" {
					t.Fatal(key, a.Metadata)
				}
			}
			if a.Metadata["validation_trusted_owner"] != st.Lease.Owner || a.Metadata["validation_trusted_claim"] != st.Lease.ClaimID {
				t.Fatal(a.Metadata)
			}
		}
	}
	got := lease.RoleProductionSummary(st, "tester")
	if !strings.Contains(got, "coordinator validation by "+st.Lease.Owner) {
		t.Fatal(got)
	}
}

func TestIssue83PRStageValidateSubmitPrivacy(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	prepared := readPROutcome(t, f.must(t, "pr", "work", p, "--phase", "plan", "--agent", "mock", "--progress=false", "--json"))
	wt := prepared.Worktree
	for name, body := range map[string]string{"PLAN.md": "# Revised interactive plan", "TEST_PLAN.md": "# Revised interactive tests"} {
		if err := os.WriteFile(filepath.Join(wt, ".swarm", "active", name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.must(t, "pr", "stage", p, "--phase", "plan", "--harness", "codex", "--model", "default")
	f.must(t, "pr", "work", p, "--phase", "review", "--agent", "mock", "--progress=false")
	if err := os.WriteFile(filepath.Join(wt, "interactive-pr.txt"), []byte("revision\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.must(t, "pr", "stage", p, "--phase", "implement", "--harness", "codex", "--model", "default")
	st := f.state(t, issue)
	for _, role := range []string{"planner", "developer", "tester"} {
		if !strings.Contains(lease.RoleProductionSummary(st, role), "staged interactively by alice") {
			t.Fatal(role, lease.RoleProductionSummary(st, role))
		}
	}
	f.must(t, "pr", "validate", p)
	issue83AssertStandaloneTester(t, f.state(t, issue))
	pr, err := f.tracker.GetPullRequest(t.Context(), number)
	if err != nil {
		t.Fatal(err)
	}
	outside := "Human prose mentioning alice stays exactly.\n"
	if _, err := f.tracker.UpdateDraftPR(t.Context(), number, "Human title", outside+pr.Body); err != nil {
		t.Fatal(err)
	}
	f.must(t, "pr", "submit", p)
	issue83AssertStandaloneTester(t, f.state(t, issue))
	updated, err := f.tracker.GetPullRequest(t.Context(), number)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(updated.Body, outside) || updated.Title != "Human title" || updated.Draft != pr.Draft {
		t.Fatal("human PR properties changed")
	}
	managed := strings.TrimPrefix(updated.Body, outside)
	if strings.Contains(managed, "alice") || !strings.Contains(managed, "staged interactively") {
		t.Fatal(managed)
	}
}

// This runner returns a completed RunResult through the real workflow engine.
// It keeps deterministic mock execution while providing explicit execution
// evidence so fresh-run classification can be tested independently of reuse.
type issue83FreshRunner struct{ agent.Runner }

func (r issue83FreshRunner) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	result, err := r.Runner.Run(ctx, req)
	if result != nil {
		result.Attestation.ExecutionMode = contract.ExecutionTrustedLocal
		result.Attestation.Authentication = "none"
	}
	return result, err
}

func TestIssue83FreshWorkflowValidationThenStandaloneReuse(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf fresh > fresh-workflow.txt")
	f.resolverRunner = func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error) {
		runner, err := agent.NewConfiguredRunnerWithOptions(name, cfg, local, agent.RunnerOptions{LookupEnv: f.env.lookup()})
		if err != nil {
			return nil, err
		}
		return issue83FreshRunner{Runner: runner}, nil
	}
	p := strconv.Itoa(number)
	f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false")
	st := f.state(t, issue)
	summary := lease.RoleProductionSummary(st, "tester")
	if !strings.Contains(summary, "headless agent run by alice; configured agent mock") {
		t.Fatal(summary)
	}
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactValidation {
			if a.Metadata["validation_trusted_origin"] != "coordinator" || a.Metadata["execution_mode"] != contract.ExecutionTrustedLocal {
				t.Fatal(a.Metadata)
			}
		}
	}
	if status := f.must(t, "issue", "status", strconv.Itoa(issue)); !strings.Contains(status, "tester production: headless agent run by alice") {
		t.Fatal(status)
	}
	if got := lease.DescriptionProductionSummary(st); strings.Contains(got, "alice") || !strings.Contains(got, "headless agent run;") {
		t.Fatal(got)
	}
	f.must(t, "pr", "validate", p)
	issue83AssertStandaloneTester(t, f.state(t, issue))
}

func TestIssue83InteractiveMetadataCannotSatisfyHumanGate(t *testing.T) {
	f := newCLIFixture(t)
	cfgPath := filepath.Join(f.root, ".swarm", "config.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	policy := strings.Replace(string(cfg), "security:\n", "security:\n  independent_review: true\n", 1)
	if policy == string(cfg) {
		t.Fatal("missing security section")
	}
	if err := os.WriteFile(cfgPath, []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".swarm/config.yaml")
	prGit(t, f.root, "commit", "-m", "require independent review")

	number := f.create(t, "human")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	wt := workspace.WorktreePath(f.root, number)
	for _, name := range []string{"PLAN.md", "TEST_PLAN.md"} {
		if err := os.WriteFile(filepath.Join(wt, ".swarm", "active", name), []byte("# Interactive evidence\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.must(t, "issue", "stage", n, "--phase", "plan", "--harness", "codex", "--model", "default")
	if _, err := f.run("issue", "review-plan", n, "--approve", "--notes", "Self-approval"); err == nil {
		t.Fatal("interactive owner self-approved")
	}
	if _, err := f.run("issue", "stage", n, "--phase", "implement"); err == nil {
		t.Fatal("interactive declaration satisfied human gate")
	}
	f.setenv("SWARM_USER", "independent-reviewer")
	f.must(t, "issue", "review-plan", n, "--approve", "--notes", "Independent approval")
}

func TestIssue83AgentOnlyPlanningRejectsInteractiveStage(t *testing.T) {
	f := newCLIFixture(t)
	path := filepath.Join(f.root, ".swarm", "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	configured := strings.Replace(string(raw), "plan_mode: auto", "plan_mode: agent", 1)
	if configured == string(raw) {
		t.Fatal("fixture plan_mode changed")
	}
	if err := os.WriteFile(path, []byte(configured), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".swarm/config.yaml")
	prGit(t, f.root, "commit", "-m", "agent-only planning policy")
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "claim", n, "--agent", "mock")
	before, _ := json.Marshal(f.state(t, number).Artifacts)
	if _, err := f.run("issue", "stage", n, "--phase", "plan"); err == nil || !strings.Contains(err.Error(), "plan_mode: agent") {
		t.Fatal(err)
	}
	after, _ := json.Marshal(f.state(t, number).Artifacts)
	if string(before) != string(after) {
		t.Fatal("rejected stage wrote artifacts")
	}
}
