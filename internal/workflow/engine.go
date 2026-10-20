package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

var (
	ErrPlanReviewRejected = errors.New("plan review requested changes")
	ErrPlanApprovalNeeded = errors.New("plan approval required before implementation")
)

type WorkOptions struct {
	RepoRoot      string
	IssueNumber   int
	Owner         string
	AgentName     string
	ReviewerAgent string
	Phase         string            // "all", "plan", "implement"
	Gate          contract.GateMode // "", "none", "agent", "human"
	Headless      bool
	LeaseDuration time.Duration
}

type WorkResult struct {
	IssueNumber      int
	WorktreePath     string
	BranchName       string
	PhaseCompleted   string
	PausedForHuman   bool
	ReviewVerdict    string
	ValidationPassed bool
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
	return &Engine{
		Tracker:      tr,
		LeaseManager: mgr,
		Config:       cfg,
	}, nil
}

func (e *Engine) ExecuteWork(ctx context.Context, opts WorkOptions) (*WorkResult, error) {
	if opts.RepoRoot == "" {
		opts.RepoRoot = "."
	}
	if opts.AgentName == "" {
		opts.AgentName = e.Config.DefaultAgent
		if opts.AgentName == "" {
			opts.AgentName = "claude"
		}
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

	// 1. Acquire or refresh lease for current owner
	st, err := e.LeaseManager.Acquire(ctx, opts.IssueNumber, opts.Owner, opts.AgentName, opts.Phase, dur, e.Config.Lease.MaxRenewals)
	if err != nil {
		return nil, err
	}

	// 2. Provision isolated git worktree
	wtPath, branchName, err := workspace.Provision(opts.RepoRoot, opts.IssueNumber, iss.Title)
	if err != nil {
		return nil, err
	}

	res := &WorkResult{
		IssueNumber:  opts.IssueNumber,
		WorktreePath: wtPath,
		BranchName:   branchName,
		State:        st,
	}

	runner, err := agent.NewRunner(opts.AgentName)
	if err != nil {
		return nil, err
	}

	// Phase 1: PLAN (analyze + design + test_plan)
	if opts.Phase == "all" || opts.Phase == "plan" {
		_, _ = e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "plan", "active")
		if err := workspace.CompileHarnessBundle(wtPath, wu, wf, st.Artifacts, "planner"); err != nil {
			return nil, err
		}

		planReq := agent.RunRequest{
			Agent:        opts.AgentName,
			Role:         "planner",
			Mode:         mode,
			WorktreePath: wtPath,
			SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md",
			TaskPrompt:   "Execute /swarm-plan: inspect the repository without modifying production source files and write .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md.",
		}
		if _, err := runner.Run(ctx, planReq); err != nil {
			return nil, err
		}

		planBytes, err := os.ReadFile(filepath.Join(wtPath, ".swarm", "active", "PLAN.md"))
		if err != nil || len(strings.TrimSpace(string(planBytes))) == 0 {
			return nil, fmt.Errorf("planner agent %q did not produce .swarm/active/PLAN.md", opts.AgentName)
		}
		testPlanBytes, err := os.ReadFile(filepath.Join(wtPath, ".swarm", "active", "TEST_PLAN.md"))
		if err != nil || len(strings.TrimSpace(string(testPlanBytes))) == 0 {
			return nil, fmt.Errorf("planner agent %q did not produce .swarm/active/TEST_PLAN.md", opts.AgentName)
		}

		st, err = e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, contract.Artifact{
			Type:     contract.ArtifactDesign,
			Producer: opts.AgentName + ":planner",
			Summary:  fmt.Sprintf("Implementation plan for #%d", opts.IssueNumber),
			Body:     string(planBytes),
		})
		if err != nil {
			return nil, err
		}

		st, err = e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, contract.Artifact{
			Type:     contract.ArtifactTestPlan,
			Producer: opts.AgentName + ":planner",
			Summary:  fmt.Sprintf("Test plan covering %d acceptance criteria", len(wu.Acceptance)),
			Body:     string(testPlanBytes),
		})
		if err != nil {
			return nil, err
		}
		res.State = st
		res.PhaseCompleted = "plan"

		// Evaluate Optional Plan Gate
		switch gate {
		case contract.GateAgent:
			revAgent := opts.ReviewerAgent
			if revAgent == "" {
				revAgent = opts.AgentName
			}
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
			_, _ = e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "review", "active")
			res.PausedForHuman = true
			return res, nil
		}

		if opts.Phase == "plan" {
			return res, nil
		}
	}

	// Phase 2: IMPLEMENT & VALIDATE
	if opts.Phase == "all" || opts.Phase == "implement" {
		// Re-fetch state in case plan/review artifacts were posted in an earlier invocation
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

		_, _ = e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "implement", "active")
		if err := workspace.CompileHarnessBundle(wtPath, wu, wf, st.Artifacts, "developer"); err != nil {
			return nil, err
		}

		implReq := agent.RunRequest{
			Agent:        opts.AgentName,
			Role:         "developer",
			Mode:         mode,
			WorktreePath: wtPath,
			SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md",
			TaskPrompt:   "Execute /swarm-implement: implement .swarm/active/PLAN.md step-by-step and satisfy .swarm/active/TEST_PLAN.md.",
		}
		if _, err := runner.Run(ctx, implReq); err != nil {
			return nil, err
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
