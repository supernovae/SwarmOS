package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

var (
	ErrPlanReviewRejected   = errors.New("plan review requested changes")
	ErrPlanApprovalNeeded   = errors.New("plan approval required before implementation")
	ErrReviewerModifiedCode = errors.New("reviewer agent modified repository files outside .swarm/active/")
)

type WorkOptions struct {
	RepoRoot       string
	IssueNumber    int
	Owner          string
	AgentName      string
	PlannerAgent   string
	ReviewerAgent  string
	DeveloperAgent string
	TesterAgent    string
	Phase          string            // "all", "plan", "implement"
	Gate           contract.GateMode // "", "none", "agent", "human"
	Headless       bool
	Handoff        bool // Release into collaborative handoff state after completing phase
	LeaseDuration  time.Duration
}

type WorkResult struct {
	IssueNumber      int
	WorktreePath     string
	BranchName       string
	PhaseCompleted   string
	HandedOffToPhase string
	PausedForHuman   bool
	ReviewVerdict    string
	ValidationPassed bool
	RoleAgentsUsed   map[string]string
	State            *lease.IssueState
}

type Engine struct {
	Tracker      tracker.Tracker
	LeaseManager *lease.Manager
	Config       *contract.SwarmConfig
}

func NewEngine(repoRoot string, tr tracker.Tracker) (*Engine, error) {
	cfg, _ := harness.LoadConfig(repoRoot)
	if cfg == nil {
		cfg = &contract.SwarmConfig{
			Version:      1,
			DefaultAgent: "claude",
			DefaultGate:  contract.GateNone,
		}
	}
	mgr := lease.NewManager(tr, cfg.Security)
	if prof, err := node.LoadOrDetectProfile(""); err == nil {
		mgr.SetNodeProfile(prof)
	}
	return &Engine{
		Tracker:      tr,
		LeaseManager: mgr,
		Config:       cfg,
	}, nil
}

func (e *Engine) resolveRoleAgent(role string, opts WorkOptions) string {
	switch role {
	case "planner":
		if opts.PlannerAgent != "" {
			return opts.PlannerAgent
		}
	case "reviewer":
		if opts.ReviewerAgent != "" {
			return opts.ReviewerAgent
		}
	case "developer":
		if opts.DeveloperAgent != "" {
			return opts.DeveloperAgent
		}
	case "tester":
		if opts.TesterAgent != "" {
			return opts.TesterAgent
		}
	}
	if e.Config != nil && e.Config.Roles != nil {
		if configured := strings.TrimSpace(e.Config.Roles[role]); configured != "" && opts.AgentName == "" {
			return configured
		}
	}
	if opts.AgentName != "" {
		return opts.AgentName
	}
	if e.Config != nil && e.Config.DefaultAgent != "" {
		return e.Config.DefaultAgent
	}
	return "claude"
}

func (e *Engine) ExecuteWork(ctx context.Context, opts WorkOptions) (*WorkResult, error) {
	if opts.RepoRoot == "" {
		opts.RepoRoot = "."
	}
	if opts.Phase == "" {
		opts.Phase = "all"
	}
	mode := "interactive"
	if opts.Headless {
		mode = "headless"
	}
	if opts.Owner == "" {
		u, err := e.Tracker.CurrentUser(ctx)
		if err != nil || u == "" {
			u = "local-dev"
		}
		opts.Owner = u
	}

	iss, err := e.Tracker.GetIssue(ctx, opts.IssueNumber)
	if err != nil {
		return nil, err
	}

	wu, err := contract.ParseWorkUnitFromIssueWithRoot(iss.Number, iss.Title, iss.Body, opts.RepoRoot)
	if err != nil {
		return nil, err
	}

	// Enforce dependency gating (depends_on)
	if err := e.LeaseManager.VerifyDependencies(ctx, wu); err != nil {
		return nil, err
	}

	wf, err := harness.LoadWorkflow(opts.RepoRoot, wu.Workflow)
	if err != nil {
		return nil, err
	}

	gate := opts.Gate
	if gate == "" {
		if wu.Gate != "" {
			gate = wu.Gate
		} else if wf.DefaultGate != "" {
			gate = wf.DefaultGate
		} else if e.Config.DefaultGate != "" {
			gate = e.Config.DefaultGate
		} else {
			gate = contract.GateNone
		}
	}
	wu.Gate = gate

	dur := opts.LeaseDuration
	if dur <= 0 {
		if parsedDur, err := time.ParseDuration(e.Config.Lease.Duration); err == nil && parsedDur > 0 {
			dur = parsedDur
		} else {
			dur = 4 * time.Hour
		}
	}

	primaryAgent := e.resolveRoleAgent("planner", opts)
	if opts.Phase == "implement" {
		primaryAgent = e.resolveRoleAgent("developer", opts)
	}

	// 1. Acquire or refresh lease for current owner (also seamlessly claims collaborative handoffs)
	st, err := e.LeaseManager.Acquire(ctx, opts.IssueNumber, opts.Owner, primaryAgent, opts.Phase, dur, e.Config.Lease.MaxRenewals)
	if err != nil {
		return nil, err
	}

	// If the lease was in handoff state with NextPhase == "implement" and opts.Phase == "all", skip re-planning!
	runPlan := opts.Phase == "all" || opts.Phase == "plan"
	if opts.Phase == "all" && hasDesignAndTestPlanArtifacts(st) && (gate == contract.GateNone || hasApprovedReviewArtifact(st)) {
		runPlan = false
	}

	// 2. Provision isolated git worktree
	wtPath, branchName, err := workspace.Provision(opts.RepoRoot, opts.IssueNumber, iss.Title)
	if err != nil {
		return nil, err
	}

	res := &WorkResult{
		IssueNumber:    opts.IssueNumber,
		WorktreePath:   wtPath,
		BranchName:     branchName,
		RoleAgentsUsed: make(map[string]string),
		State:          st,
	}

	// Step 1: PLAN (analyze + design + test_plan)
	if runPlan {
		plannerAgent := e.resolveRoleAgent("planner", opts)
		res.RoleAgentsUsed["planner"] = plannerAgent
		planRunner, err := agent.NewRunner(plannerAgent)
		if err != nil {
			return nil, err
		}

		_, _ = e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "plan", "active")
		if err := workspace.CompileHarnessBundle(wtPath, wu, wf, st.Artifacts, "planner"); err != nil {
			return nil, err
		}

		planReq := agent.RunRequest{
			Agent:        plannerAgent,
			Role:         "planner",
			Mode:         mode,
			WorktreePath: wtPath,
			SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md",
			TaskPrompt:   "Execute /swarm-plan: inspect the repository without modifying production source files and write .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md.",
		}
		if _, err := planRunner.Run(ctx, planReq); err != nil {
			return nil, err
		}

		planBytes, err := os.ReadFile(filepath.Join(wtPath, ".swarm", "active", "PLAN.md"))
		if err != nil || len(strings.TrimSpace(string(planBytes))) == 0 {
			return nil, fmt.Errorf("planner agent %q did not produce .swarm/active/PLAN.md", plannerAgent)
		}
		testPlanBytes, err := os.ReadFile(filepath.Join(wtPath, ".swarm", "active", "TEST_PLAN.md"))
		if err != nil || len(strings.TrimSpace(string(testPlanBytes))) == 0 {
			return nil, fmt.Errorf("planner agent %q did not produce .swarm/active/TEST_PLAN.md", plannerAgent)
		}

		if _, err := e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, contract.Artifact{
			Type:     contract.ArtifactDesign,
			Producer: plannerAgent + ":planner",
			Summary:  fmt.Sprintf("Implementation plan for #%d", opts.IssueNumber),
			Body:     string(planBytes),
		}); err != nil {
			return nil, err
		}

		st, err = e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, contract.Artifact{
			Type:     contract.ArtifactTestPlan,
			Producer: plannerAgent + ":planner",
			Summary:  fmt.Sprintf("Test plan covering %d acceptance criteria", len(wu.Acceptance)),
			Body:     string(testPlanBytes),
		})
		if err != nil {
			return nil, err
		}
		res.State = st
		res.PhaseCompleted = "plan"
		if opts.Phase == "plan" {
			if opts.Handoff {
				nextP := "implement"
				if gate != contract.GateNone && !hasApprovedReviewArtifact(st) {
					nextP = "review"
					_ = e.Tracker.AddLabels(ctx, opts.IssueNumber, []string{"swarm:plan-review"})
				}
				if hSt, hErr := e.LeaseManager.Handoff(ctx, opts.IssueNumber, opts.Owner, "plan", nextP); hErr == nil {
					res.State = hSt
					res.HandedOffToPhase = nextP
				}
			}
			return res, nil
		}

		// Evaluate Optional Plan Gate
		switch gate {
		case contract.GateAgent:
			revAgent := e.resolveRoleAgent("reviewer", opts)
			res.RoleAgentsUsed["reviewer"] = revAgent
			verdict, revState, err := e.RunAgentReview(ctx, wtPath, wu, wf, st, opts.Owner, revAgent, mode)
			res.ReviewVerdict = verdict
			if revState != nil {
				st = revState
				res.State = st
			}
			if err != nil {
				return res, err
			}

		case contract.GateHuman:
			_ = e.Tracker.AddLabels(ctx, opts.IssueNumber, []string{"swarm:plan-review"})
			if opts.Handoff {
				if hSt, hErr := e.LeaseManager.Handoff(ctx, opts.IssueNumber, opts.Owner, "plan", "review"); hErr == nil {
					res.State = hSt
					res.HandedOffToPhase = "review"
				}
			} else {
				_, _ = e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "review", "active")
			}
			res.PausedForHuman = true
			return res, nil
		}
	}

	// Step 1b: Standalone REVIEW phase (when a contributor or bot claims a handed-off review step)
	if opts.Phase == "review" {
		if latest, err := e.LeaseManager.GetCurrentState(ctx, opts.IssueNumber); err == nil {
			st = latest
			res.State = st
		}
		revAgent := e.resolveRoleAgent("reviewer", opts)
		res.RoleAgentsUsed["reviewer"] = revAgent
		verdict, revState, err := e.RunAgentReview(ctx, wtPath, wu, wf, st, opts.Owner, revAgent, mode)
		res.ReviewVerdict = verdict
		if revState != nil {
			st = revState
			res.State = st
		}
		if err != nil {
			return res, err
		}
		res.PhaseCompleted = "review"
		if opts.Handoff {
			if hSt, hErr := e.LeaseManager.Handoff(ctx, opts.IssueNumber, opts.Owner, "review", "implement"); hErr == nil {
				res.State = hSt
				res.HandedOffToPhase = "implement"
			}
		}
		return res, nil
	}

	// Step 2: IMPLEMENT, TEST & VALIDATE
	if opts.Phase == "all" || opts.Phase == "implement" {
		if latest, err := e.LeaseManager.GetCurrentState(ctx, opts.IssueNumber); err == nil {
			st = latest
			res.State = st
		}
		if err := ensurePlanArtifactsInWorktree(wtPath, st); err != nil {
			return nil, err
		}
		if gate == contract.GateAgent || gate == contract.GateHuman {
			if !hasApprovedReviewArtifact(st) {
				return nil, fmt.Errorf("%w: issue #%d has gate=%q; run `swarm review %d --approve` or `swarm review %d --agent <name>` first",
					ErrPlanApprovalNeeded, opts.IssueNumber, gate, opts.IssueNumber, opts.IssueNumber)
			}
		}

		devAgent := e.resolveRoleAgent("developer", opts)
		res.RoleAgentsUsed["developer"] = devAgent
		devRunner, err := agent.NewRunner(devAgent)
		if err != nil {
			return nil, err
		}

		_, _ = e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "implement", "active")
		if err := workspace.CompileHarnessBundle(wtPath, wu, wf, st.Artifacts, "developer"); err != nil {
			return nil, err
		}

		implReq := agent.RunRequest{
			Agent:        devAgent,
			Role:         "developer",
			Mode:         mode,
			WorktreePath: wtPath,
			SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md",
			TaskPrompt:   "Execute /swarm-implement: implement .swarm/active/PLAN.md step-by-step and satisfy .swarm/active/TEST_PLAN.md.",
		}
		if _, err := devRunner.Run(ctx, implReq); err != nil {
			return nil, err
		}

		// Publish `implementation` artifact capturing git diff summary
		implSummary, implBody := summarizeImplementationChanges(wtPath, opts.IssueNumber)
		if updatedSt, uErr := e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, contract.Artifact{
			Type:     contract.ArtifactImplementation,
			Producer: devAgent + ":developer",
			Verdict:  "PASS",
			Summary:  implSummary,
			Body:     implBody,
		}); uErr == nil {
			st = updatedSt
			res.State = st
		}

		// Optional Tester Agent Role execution before deterministic Validator
		if opts.TesterAgent != "" || (e.Config != nil && e.Config.Roles != nil && e.Config.Roles["tester"] != "") {
			testerAgent := e.resolveRoleAgent("tester", opts)
			res.RoleAgentsUsed["tester"] = testerAgent
			if testerRunner, tErr := agent.NewRunner(testerAgent); tErr == nil {
				_ = workspace.CompileHarnessBundle(wtPath, wu, wf, st.Artifacts, "tester")
				_, _ = testerRunner.Run(ctx, agent.RunRequest{
					Agent:        testerAgent,
					Role:         "tester",
					Mode:         mode,
					WorktreePath: wtPath,
					SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md",
					TaskPrompt:   "Execute /swarm-validate: verify all acceptance criteria in .swarm/active/TEST_PLAN.md.",
				})
			}
		}

		valSpec, err := harness.LoadValidationSpec(wtPath)
		if err != nil {
			valSpec, _ = harness.LoadValidationSpec(opts.RepoRoot)
		}
		valReport, valErr := validation.Validate(ctx, wtPath, wu, valSpec)
		if valReport != nil {
			if updatedSt, uErr := e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, valReport.Artifact); uErr == nil {
				st = updatedSt
				res.State = st
			}
			res.ValidationPassed = valReport.Passed
		}
		if valErr != nil {
			return res, valErr
		}

		res.PhaseCompleted = "implement"
	}

	return res, nil
}

// DecomposeIssue partitions a parent issue into sequential or parallel child WorkUnit issues linked via ParentIssue and DependsOn.
func (e *Engine) DecomposeIssue(ctx context.Context, repoRoot string, parentIssueNum int, subtasks []contract.WorkUnit, sequential bool) ([]*tracker.Issue, error) {
	parentIss, err := e.Tracker.GetIssue(ctx, parentIssueNum)
	if err != nil {
		return nil, err
	}
	parentWU, err := contract.ParseWorkUnitFromIssueWithRoot(parentIss.Number, parentIss.Title, parentIss.Body, repoRoot)
	if err != nil {
		return nil, err
	}

	if len(subtasks) == 0 {
		subtasks = parentWU.Subtasks
	}
	if len(subtasks) == 0 {
		// Automatically partition by acceptance criteria if no explicit subtasks array was supplied
		for idx, acc := range parentWU.Acceptance {
			subtasks = append(subtasks, contract.WorkUnit{
				ID:          fmt.Sprintf("%s-SUB-%d", parentWU.ID, idx+1),
				Title:       fmt.Sprintf("[%s Part %d] %s", parentWU.ID, idx+1, truncateStr(acc, 50)),
				Objective:   fmt.Sprintf("Sub-workunit of #%d (%s): %s", parentIssueNum, parentIss.Title, acc),
				Constraints: parentWU.Constraints,
				Acceptance:  []string{acc},
				Workflow:    parentWU.Workflow,
				Gate:        parentWU.Gate,
			})
		}
	}

	var created []*tracker.Issue
	var prevIssueNum int
	var summaryMD strings.Builder
	summaryMD.WriteString(fmt.Sprintf("### SwarmOS Work Partitioning (Parent `#%d`)\n\n", parentIssueNum))
	summaryMD.WriteString("| Sub-Issue | Title | Depends On |\n| :--- | :--- | :--- |\n")

	for idx, sub := range subtasks {
		sub.ParentIssue = parentIssueNum
		if sub.ID == "" {
			sub.ID = fmt.Sprintf("ISSUE-%d-SUB-%d", parentIssueNum, idx+1)
		}
		if sub.Workflow == "" {
			sub.Workflow = parentWU.Workflow
		}
		if len(sub.Constraints) == 0 {
			sub.Constraints = parentWU.Constraints
		}
		if sequential && prevIssueNum > 0 {
			sub.DependsOn = append(sub.DependsOn, prevIssueNum)
		}
		title := sub.Title
		if title == "" {
			title = fmt.Sprintf("[Subtask %d/%d] %s", idx+1, len(subtasks), parentIss.Title)
		}
		body := contract.FormatIssueMarkdown(&sub)
		childIss, err := e.Tracker.CreateIssue(ctx, title, body, []string{"swarm:ready"})
		if err != nil {
			return created, err
		}
		created = append(created, childIss)
		prevIssueNum = childIss.Number

		depStr := "-"
		if len(sub.DependsOn) > 0 {
			var parts []string
			for _, d := range sub.DependsOn {
				parts = append(parts, fmt.Sprintf("#%d", d))
			}
			depStr = strings.Join(parts, ", ")
		}
		summaryMD.WriteString(fmt.Sprintf("| `#%d` | %s | %s |\n", childIss.Number, childIss.Title, depStr))
	}

	_, _ = e.Tracker.CreateComment(ctx, parentIssueNum, summaryMD.String())
	return created, nil
}

func (e *Engine) RunAgentReview(ctx context.Context, wtPath string, wu *contract.WorkUnit, wf *contract.WorkflowSpec, st *lease.IssueState, owner, reviewerAgent, mode string) (string, *lease.IssueState, error) {
	_, _ = e.LeaseManager.UpdatePhase(ctx, wu.IssueNumber, owner, "review", "active")
	if err := ensurePlanArtifactsInWorktree(wtPath, st); err != nil {
		return "", st, err
	}
	if err := workspace.CompileHarnessBundle(wtPath, wu, wf, st.Artifacts, "reviewer"); err != nil {
		return "", st, err
	}

	revRunner, err := agent.NewRunner(reviewerAgent)
	if err != nil {
		return "", st, err
	}

	beforeSnap := snapshotNonActiveWorktreeStatus(wtPath)

	revReq := agent.RunRequest{
		Agent:        reviewerAgent,
		Role:         "reviewer",
		Mode:         mode,
		WorktreePath: wtPath,
		SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md",
		TaskPrompt:   "Execute /swarm-review: audit .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md against .swarm/active/WORKUNIT.md and write .swarm/active/REVIEW.md starting with VERDICT: APPROVED or VERDICT: CHANGES_REQUESTED.",
	}
	if _, err := revRunner.Run(ctx, revReq); err != nil {
		return "", st, err
	}

	afterSnap := snapshotNonActiveWorktreeStatus(wtPath)
	if beforeSnap != afterSnap {
		_ = exec.Command("git", "-C", wtPath, "checkout", "--", ".").Run()
		return "", st, fmt.Errorf("%w: reviewer %q altered files outside .swarm/active/", ErrReviewerModifiedCode, reviewerAgent)
	}

	reviewBytes, err := os.ReadFile(filepath.Join(wtPath, ".swarm", "active", "REVIEW.md"))
	if err != nil {
		return "", st, fmt.Errorf("reviewer agent %q did not write .swarm/active/REVIEW.md: %w", reviewerAgent, err)
	}
	verdict := ParseReviewVerdict(string(reviewBytes))

	updatedSt, err := e.LeaseManager.UpdateArtifact(ctx, wu.IssueNumber, owner, contract.Artifact{
		Type:     contract.ArtifactReview,
		Producer: reviewerAgent + ":reviewer",
		Verdict:  verdict,
		Summary:  fmt.Sprintf("Automated plan review verdict: %s", verdict),
		Body:     string(reviewBytes),
	})
	if err != nil {
		return verdict, st, err
	}

	if verdict != "APPROVED" {
		_ = e.Tracker.AddLabels(ctx, wu.IssueNumber, []string{"swarm:plan-review"})
		return verdict, updatedSt, fmt.Errorf("%w: reviewer %q returned %s", ErrPlanReviewRejected, reviewerAgent, verdict)
	}

	_ = e.Tracker.RemoveLabel(ctx, wu.IssueNumber, "swarm:plan-review")
	_ = e.Tracker.AddLabels(ctx, wu.IssueNumber, []string{"swarm:plan-approved"})
	return verdict, updatedSt, nil
}

func ParseReviewVerdict(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, "VERDICT:") {
			v := strings.TrimSpace(strings.TrimPrefix(upper, "VERDICT:"))
			if strings.Contains(v, "CHANGES_REQUESTED") {
				return "CHANGES_REQUESTED"
			}
			if strings.Contains(v, "APPROVED") {
				return "APPROVED"
			}
		}
	}
	if strings.Contains(strings.ToUpper(content), "CHANGES_REQUESTED") {
		return "CHANGES_REQUESTED"
	}
	return "APPROVED"
}

func hasDesignAndTestPlanArtifacts(st *lease.IssueState) bool {
	if st == nil {
		return false
	}
	var hasDesign, hasTestPlan bool
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactDesign {
			hasDesign = true
		}
		if a.Type == contract.ArtifactTestPlan {
			hasTestPlan = true
		}
	}
	return hasDesign && hasTestPlan
}

func hasApprovedReviewArtifact(st *lease.IssueState) bool {
	if st == nil {
		return false
	}
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactReview && a.Verdict == "APPROVED" {
			return true
		}
	}
	return false
}

func ensurePlanArtifactsInWorktree(wtPath string, st *lease.IssueState) error {
	activeDir := filepath.Join(wtPath, ".swarm", "active")
	if err := os.MkdirAll(activeDir, 0o755); err != nil {
		return err
	}
	if st != nil {
		for _, a := range st.Artifacts {
			switch a.Type {
			case contract.ArtifactDesign:
				p := filepath.Join(activeDir, "PLAN.md")
				if _, err := os.Stat(p); err != nil {
					_ = os.WriteFile(p, []byte(a.Body), 0o644)
				}
			case contract.ArtifactTestPlan:
				p := filepath.Join(activeDir, "TEST_PLAN.md")
				if _, err := os.Stat(p); err != nil {
					_ = os.WriteFile(p, []byte(a.Body), 0o644)
				}
			case contract.ArtifactReview:
				p := filepath.Join(activeDir, "REVIEW.md")
				if _, err := os.Stat(p); err != nil {
					_ = os.WriteFile(p, []byte(a.Body), 0o644)
				}
			}
		}
	}
	for _, req := range []string{"PLAN.md", "TEST_PLAN.md"} {
		if _, err := os.Stat(filepath.Join(activeDir, req)); err != nil {
			return fmt.Errorf("missing %s in .swarm/active/ (run `swarm work <issue> --phase plan` first)", req)
		}
	}
	return nil
}

func summarizeImplementationChanges(wtPath string, issueNum int) (string, string) {
	statusOut, _ := exec.Command("git", "-C", wtPath, "status", "--short").Output()
	diffStat, _ := exec.Command("git", "-C", wtPath, "diff", "--stat", "HEAD").Output()

	lines := strings.Split(strings.TrimSpace(string(statusOut)), "\n")
	count := 0
	var body strings.Builder
	body.WriteString(fmt.Sprintf("# Implementation Summary (Issue #%d)\n\n", issueNum))
	body.WriteString("## Working Tree Changes\n```text\n")
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if strings.Contains(l, ".swarm/active") || strings.Contains(l, "AGENTS.md") || strings.Contains(l, "CLAUDE.md") {
			continue
		}
		count++
		body.WriteString(l + "\n")
	}
	if count == 0 {
		body.WriteString("(clean working tree)\n")
	}
	body.WriteString("```\n")
	if len(strings.TrimSpace(string(diffStat))) > 0 {
		body.WriteString("\n## Diff Stat\n```text\n" + strings.TrimSpace(string(diffStat)) + "\n```\n")
	}
	return fmt.Sprintf("Implemented changes across %d file(s)", count), body.String()
}

func truncateStr(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func snapshotNonActiveWorktreeStatus(wtPath string) string {
	out, err := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
	if err != nil {
		return ""
	}
	var relevant []string
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.Contains(trimmed, ".swarm/active") || strings.Contains(trimmed, "AGENTS.md") || strings.Contains(trimmed, "CLAUDE.md") {
			continue
		}
		relevant = append(relevant, trimmed)
	}
	return strings.Join(relevant, "\n")
}
