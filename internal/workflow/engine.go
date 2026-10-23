package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

var (
	ErrPlanReviewRejected = errors.New("plan review requested changes")
	ErrPlanApprovalNeeded = errors.New("plan approval required before implementation")
	ErrLeaseLost          = errors.New("lease ownership lost during execution")
)

type WorkOptions struct {
	InspectOnly     bool // report execution blockers without claiming or running work
	RepoRoot        string
	IssueNumber     int
	Owner           string
	ClaimID         string // explicit fencing token persisted by the CLI
	AgentName       string
	PlannerAgent    string
	ReviewerAgent   string
	DeveloperAgent  string
	TesterAgent     string
	Phase           string // "all", "plan", "review", "implement"
	Gate            contract.GateMode
	Headless        bool
	Handoff         bool
	LeaseDuration   time.Duration
	ForkMode        bool
	ForkRemote      string
	PRNumber        int // nonzero selects an already prepared, separate PR worktree
	PRWorktree      string
	Instructions    string                        // additional operator guidance, never a replacement for trusted policy
	FreshPRRevision bool                          // inspect the new revision's empty evidence before mutation
	OnClaim         func(*lease.IssueState) error // persist fencing token before setup
	// OnProgress receives serialized local status events. It must return promptly
	// and must not call back into execution. No callback runs after ExecuteWork returns.
	OnProgress func(ProgressEvent)
}

type WorkResult struct {
	IssueNumber      int
	WorktreePath     string
	BranchName       string
	BaseSHA          string
	HeadSHA          string
	PhaseCompleted   string
	HandedOffToPhase string
	PausedForHuman   bool
	ReviewVerdict    string
	ValidationPassed bool
	RoleAgentsUsed   map[string]string
	State            *lease.IssueState
}

// PreflightResult is the immutable execution selection made before a claim.
type PreflightResult struct {
	Execution    contract.ExecutionConfig
	Blockers     []string
	Issue        *tracker.Issue
	WorkUnit     *contract.WorkUnit
	Workflow     *contract.WorkflowSpec
	Config       *contract.SwarmConfig
	BaseSHA      string
	Gate         contract.GateMode
	Agents       map[string]string
	Binaries     map[string]string
	Validation   *contract.ValidationSpec
	TaskDigest   string
	PolicyDigest string
	Phase        string
}

type Engine struct {
	Tracker      tracker.Tracker
	LeaseManager *lease.Manager
	Config       *contract.SwarmConfig
	repoRoot     string
	ForkMode     bool
	ForkRemote   string
}

func NewEngine(repoRoot string, tr tracker.Tracker) (*Engine, error) {
	cfg, err := harness.LoadConfig(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("load SwarmOS config: %w", err)
	}
	mgr := lease.NewManager(tr, cfg.Security)
	if err := mgr.ConfigureStore(repoRoot); err != nil {
		return nil, fmt.Errorf("configure coordination store: %w", err)
	}
	if prof, err := node.LoadOrDetectProfile(""); err == nil {
		mgr.SetNodeProfile(prof)
	}
	return &Engine{Tracker: tr, LeaseManager: mgr, Config: cfg, repoRoot: repoRoot}, nil
}

func NewEngineWithManager(repoRoot string, tr tracker.Tracker, mgr *lease.Manager, cfg *contract.SwarmConfig, forkMode bool, forkRemote string) *Engine {
	if prof, err := node.LoadOrDetectProfile(""); err == nil && mgr != nil {
		mgr.SetNodeProfile(prof)
	}
	return &Engine{
		Tracker:      tr,
		LeaseManager: mgr,
		Config:       cfg,
		repoRoot:     repoRoot,
		ForkMode:     forkMode,
		ForkRemote:   forkRemote,
	}
}

// resolveRoleAgent applies role override, generic override, repository role,
// then repository default precedence. It performs no availability check.
func resolveRoleAgent(cfg *contract.SwarmConfig, role string, opts WorkOptions) string {
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
	if opts.AgentName != "" {
		return opts.AgentName
	}
	if cfg != nil && cfg.Roles != nil {
		if configured := strings.TrimSpace(cfg.Roles[role]); configured != "" {
			return configured
		}
	}
	if cfg == nil {
		return ""
	}
	return cfg.DefaultAgent
}

// Preflight rejects selector, policy, contract, workflow, and runner errors
// before acquiring a lease or provisioning a worktree.
func (e *Engine) Preflight(ctx context.Context, opts WorkOptions) (*PreflightResult, error) {
	if opts.RepoRoot == "" {
		opts.RepoRoot = e.repoRoot
	}
	if opts.Phase == "" {
		opts.Phase = "all"
	}
	if err := contract.ValidatePhase(opts.Phase); err != nil {
		return nil, err
	}
	if err := contract.ValidateGate(opts.Gate); err != nil {
		return nil, err
	}
	iss, err := e.Tracker.GetIssue(ctx, opts.IssueNumber)
	if err != nil {
		return nil, err
	}
	if iss.IsEpic() {
		return nil, fmt.Errorf("issue #%d has type \"Epic\" and cannot be claimed or executed as a WorkUnit", opts.IssueNumber)
	}
	wu, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
	if err != nil {
		return nil, err
	}
	state, err := e.LeaseManager.GetCurrentState(ctx, opts.IssueNumber)
	if err != nil {
		return nil, fmt.Errorf("read approved task state: %w", err)
	}
	if state == nil || state.BaseSHA == "" {
		return nil, errors.New("task has no approved immutable base")
	}
	if state.PRRevision != nil && opts.PRNumber == 0 {
		return nil, fmt.Errorf("issue #%d has a pending PR revision; use swarm pr work/validate/submit", opts.IssueNumber)
	}
	if opts.PRNumber != 0 && (state.PRRevision == nil || state.PRRevision.Number != opts.PRNumber) && !opts.InspectOnly && !opts.FreshPRRevision {
		return nil, fmt.Errorf("PR revision is not linked to this issue")
	}
	if opts.FreshPRRevision {
		if opts.PRNumber == 0 || state.PRRevision != nil {
			return nil, fmt.Errorf("fresh PR revision preflight does not match authoritative state")
		}
		if opts.Phase == "implement" || opts.Phase == "review" {
			return nil, fmt.Errorf("%w: first prepare the new PR plan", ErrPlanApprovalNeeded)
		}
		copyState := *state
		copyState.Artifacts = nil
		copyState.Attestations = nil
		state = &copyState
	}
	_ = workspace.FetchCommit(opts.RepoRoot, "origin", state.BaseSHA)
	cfg, err := harness.LoadConfigAt(opts.RepoRoot, state.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("load approved base config: %w", err)
	}
	if state.TaskDigest != contract.DigestWorkUnit(wu) || state.PolicyDigest != contract.DigestApprovedPolicy(state.BaseSHA, cfg) ||
		!slices.Contains(iss.Labels, cfg.Security.ReadyLabel) {
		return nil, lease.ErrApprovalRequired
	}
	execution, err := contract.ResolveExecution(cfg, os.Getenv("SWARM_EXECUTION_MODE"))
	if err != nil {
		return nil, err
	}
	var blockers []string
	checks, err := harness.LoadValidationSpecAt(opts.RepoRoot, state.BaseSHA)
	if err != nil {
		return nil, err
	}
	wf, err := harness.LoadWorkflowAt(opts.RepoRoot, wu.Workflow, state.BaseSHA)
	if err != nil {
		return nil, err
	}
	gate, err := contract.ResolveGate(wu.Gate, wf.DefaultGate, cfg.DefaultGate)
	if err != nil {
		return nil, err
	}
	gate, err = contract.ResolveGate(opts.Gate, gate, state.EffectiveGate)
	if err != nil {
		return nil, err
	}
	agents := make(map[string]string, 4)
	binaries := make(map[string]string, 4)
	approvalState := *state
	approvalState.EffectiveGate = gate
	roles := rolesForPhase(opts.Phase, gate, hasDesignAndTestPlanArtifacts(state), lease.VerifyApproval(&approvalState, wu) == nil)
	if strings.EqualFold(cfg.PlanMode, "interactive") && (opts.Phase == "plan" || (opts.Phase == "all" && !hasDesignAndTestPlanArtifacts(state))) {
		agents["planner"] = "interactive"
		binaries["planner"] = "interactive"
	} else {
		for _, role := range roles {
			if role == "planner" && strings.EqualFold(cfg.PlanMode, "interactive") {
				agents[role] = "interactive"
				binaries[role] = "interactive"
				continue
			}
			name := resolveRoleAgent(cfg, role, opts)
			if name == "" {
				return nil, fmt.Errorf("no configured %s agent", role)
			}
			resolved, err := agent.Preflight(name, cfg, e.Tracker.BackendName() == "local", role)
			if err != nil {
				if !opts.InspectOnly {
					return nil, fmt.Errorf("%s agent preflight: %w", role, err)
				}
				blockers = append(blockers, fmt.Sprintf("%s agent preflight: %v", role, err))
			}
			agents[role], binaries[role] = name, resolved
		}
	}
	if execution.Mode == contract.ExecutionIsolated && slices.Contains(roles, "developer") && len(checks.RequiredChecks) > 0 {
		var runtimeErr error
		if execution.Backend == contract.BackendNative {
			runtimeErr = sandbox.CheckNativeValidation(ctx, cfg)
		} else {
			runtimeErr = sandbox.CheckWorkerRuntime(ctx, execution.ContainerRuntime, execution.ContainerImage)
		}
		if err := runtimeErr; err != nil {
			if !opts.InspectOnly {
				return nil, fmt.Errorf("validation runtime preflight: %w", err)
			}
			blockers = append(blockers, fmt.Sprintf("validation runtime preflight: %v", err))
		}
	}
	return &PreflightResult{Execution: execution, Blockers: blockers, Issue: iss, WorkUnit: wu, Workflow: wf, Config: cfg, BaseSHA: state.BaseSHA, Gate: gate, Agents: agents,
		Binaries: binaries, Validation: checks, TaskDigest: state.TaskDigest, PolicyDigest: state.PolicyDigest, Phase: opts.Phase}, nil
}

func rolesForPhase(phase string, gate contract.GateMode, hasPlan, approved bool) []string {
	switch phase {
	case "plan":
		return []string{"planner"}
	case "review":
		if gate == contract.GateAgent || gate == contract.GateNone {
			return []string{"reviewer"}
		}
		return nil
	case "implement":
		return []string{"developer", "tester"}
	case "all":
		var roles []string
		if !hasPlan {
			roles = append(roles, "planner")
		}
		if gate == contract.GateHuman && (!hasPlan || !approved) {
			return roles
		}
		if gate == contract.GateAgent && (!hasPlan || !approved) {
			roles = append(roles, "reviewer")
		}
		return append(roles, "developer", "tester")
	default:
		return nil
	}
}

func (e *Engine) ExecuteWork(ctx context.Context, opts WorkOptions) (result *WorkResult, workErr error) {
	progress := startWorkProgress(ctx, opts.IssueNumber, opts.OnProgress)
	defer func() { progress.finish(result, workErr) }()
	// Inspection never weakens execution preflight, even for direct API callers.
	opts.InspectOnly = false
	if opts.RepoRoot == "" {
		opts.RepoRoot = e.repoRoot
	}
	if opts.Phase == "" {
		opts.Phase = "all"
	}
	if opts.Owner == "" {
		var err error
		if opts.Owner, err = e.Tracker.CurrentUser(ctx); err != nil {
			return nil, err
		}
	}
	pre, err := e.Preflight(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := e.LeaseManager.VerifyDependencies(ctx, pre.WorkUnit); err != nil {
		return nil, err
	}
	duration := opts.LeaseDuration
	if duration <= 0 {
		if duration, err = time.ParseDuration(pre.Config.Lease.Duration); err != nil {
			return nil, fmt.Errorf("parse configured lease duration: %w", err)
		}
	}
	primaryAgent := pre.Agents["planner"]
	if primaryAgent == "" {
		primaryAgent = pre.Agents["reviewer"]
	}
	if primaryAgent == "" {
		primaryAgent = pre.Agents["developer"]
	}
	if primaryAgent == "" {
		primaryAgent = pre.Agents["tester"]
	}
	var st *lease.IssueState
	progress.next("claim")
	if opts.ClaimID != "" {
		e.LeaseManager.BindClaim(opts.IssueNumber, opts.ClaimID)
		st, err = e.LeaseManager.Resume(ctx, opts.IssueNumber, opts.Owner, opts.ClaimID)
	} else {
		st, err = e.LeaseManager.Acquire(ctx, opts.IssueNumber, opts.Owner, primaryAgent, opts.Phase, duration, pre.Config.Lease.MaxRenewals)
	}
	if err != nil {
		return nil, err
	}
	setupComplete := false
	defer func() {
		if !setupComplete {
			_, _ = e.LeaseManager.Release(context.Background(), opts.IssueNumber, opts.Owner, "workflow setup failed")
		}
	}()
	if st.BaseSHA != pre.BaseSHA {
		return nil, errors.New("approved task base changed during preflight; rerun swarm issue work (or swarm pr work)")
	}
	if opts.OnClaim != nil {
		if err := opts.OnClaim(st); err != nil {
			return nil, fmt.Errorf("persist claim token: %w", err)
		}
	}
	cfg, err := harness.LoadConfigAt(opts.RepoRoot, st.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("load approved base config: %w", err)
	}
	wf, err := harness.LoadWorkflowAt(opts.RepoRoot, pre.WorkUnit.Workflow, st.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("load approved base workflow: %w", err)
	}
	gate, err := contract.ResolveGate(pre.WorkUnit.Gate, wf.DefaultGate, cfg.DefaultGate)
	if err != nil {
		return nil, err
	}
	if opts.Gate != "" {
		if gate, err = contract.ResolveGate(opts.Gate, gate, gate); err != nil {
			return nil, err
		}
	}
	st, err = e.LeaseManager.RequireGate(ctx, opts.IssueNumber, opts.Owner, gate)
	if err != nil {
		return nil, err
	}
	gate = st.EffectiveGate
	if !(strings.EqualFold(cfg.PlanMode, "interactive") && (opts.Phase == "plan" || (opts.Phase == "all" && !hasDesignAndTestPlanArtifacts(st)))) {
		for _, role := range rolesForPhase(opts.Phase, gate, hasDesignAndTestPlanArtifacts(st), lease.VerifyApproval(st, pre.WorkUnit) == nil) {
			if pre.Agents[role] == "" {
				return nil, fmt.Errorf("task phase changed during preflight; rerun swarm issue work (or swarm pr work) to validate the %s agent", role)
			}
		}
	}
	e.Config = cfg
	heartbeat := e.LeaseManager.StartHeartbeat(ctx, opts.IssueNumber, opts.Owner, duration)
	defer heartbeat.Stop()
	ctx = heartbeat.Context()
	check := func() error {
		if heartbeatErr := heartbeat.Err(); heartbeatErr != nil {
			return fmt.Errorf("%w: %w", ErrLeaseLost, heartbeatErr)
		}
		if err := e.LeaseManager.CheckClaim(ctx, opts.IssueNumber, opts.Owner); err != nil {
			heartbeat.Stop()
			return fmt.Errorf("%w: %w", ErrLeaseLost, err)
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, err
	}
	forkMode := e.ForkMode || opts.ForkMode
	forkRemote := strings.TrimSpace(opts.ForkRemote)
	if forkRemote == "" {
		forkRemote = strings.TrimSpace(e.ForkRemote)
	}
	if forkRemote == "" {
		forkRemote = "fork"
	}
	targetBranch := st.Branch
	checkpointRemote := "origin"
	if forkMode {
		checkpointRemote = forkRemote
		if targetBranch == "" {
			targetBranch = workspace.ForkBranchName(opts.IssueNumber, st.Lease.ClaimID)
		}
	}
	progress.next("workspace")
	var wtPath, branch string
	if opts.PRNumber != 0 {
		if opts.PRWorktree == "" || st.PRRevision == nil || st.PRRevision.Number != opts.PRNumber {
			return nil, fmt.Errorf("PR worktree and fenced revision are required")
		}
		wtPath, branch = opts.PRWorktree, st.PRRevision.Branch
		head, dirty, rebasing, headErr := workspace.PRHead(ctx, wtPath)
		if headErr != nil {
			return nil, headErr
		}
		if rebasing || dirty || head != st.PRRevision.LocalHeadSHA {
			return nil, fmt.Errorf("PR worktree is dirty, rebasing, or differs from its recorded checkpoint")
		}
	} else {
		wtPath, branch, err = workspace.ProvisionFromRemoteAt(opts.RepoRoot, checkpointRemote, opts.IssueNumber, pre.Issue.Title, st.BaseSHA, targetBranch, st.HeadSHA)
		if err != nil {
			return nil, err
		}
	}
	setupComplete = true
	res := &WorkResult{IssueNumber: opts.IssueNumber, WorktreePath: wtPath, BranchName: branch, BaseSHA: st.BaseSHA, HeadSHA: st.HeadSHA, RoleAgentsUsed: map[string]string{}, State: st}
	if opts.PRNumber != 0 {
		res.HeadSHA = st.PRRevision.LocalHeadSHA
	}
	mode := "interactive"
	if opts.Headless {
		mode = "headless"
	}
	runPlan := opts.Phase == "all" || opts.Phase == "plan"
	if opts.Phase == "all" && hasDesignAndTestPlanArtifacts(st) {
		runPlan = false
	}
	if runPlan {
		progress.next("plan")
		if err := check(); err != nil {
			return res, err
		}
		if strings.EqualFold(cfg.PlanMode, "interactive") {
			res.RoleAgentsUsed["planner"] = "interactive"
			if err := compileExecutionBundle(wtPath, pre.WorkUnit, wf, st, "planner", opts.PRNumber); err != nil {
				return res, err
			}
			if st, err = e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "plan", "active"); err != nil {
				return res, err
			}
			res.State = st
			res.PausedForHuman = true
			res.PhaseCompleted = "awaiting-plan"
			return res, nil
		}
		planner := pre.Agents["planner"]
		res.RoleAgentsUsed["planner"] = planner
		runner, err := agent.NewConfiguredRunner(planner, cfg, e.Tracker.BackendName() == "local")
		if err != nil {
			return res, err
		}
		if _, err := e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "plan", "active"); err != nil {
			return res, err
		}
		if err := compileExecutionBundle(wtPath, pre.WorkUnit, wf, st, "planner", opts.PRNumber); err != nil {
			return res, err
		}
		planPrompt := "Execute /swarm-plan and write .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md."
		if opts.Instructions != "" {
			planPrompt += "\nOperator instructions: " + opts.Instructions
		}
		planRun, err := runner.Run(ctx, agent.RunRequest{Agent: planner, Role: "planner", Mode: mode, WorktreePath: wtPath, SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md", TaskPrompt: planPrompt})
		if err != nil {
			return res, err
		}
		plan, err := readRequiredArtifact(wtPath, "PLAN.md")
		if err != nil {
			return res, fmt.Errorf("planner agent %q: %w", planner, err)
		}
		testPlan, err := readRequiredArtifact(wtPath, "TEST_PLAN.md")
		if err != nil {
			return res, fmt.Errorf("planner agent %q: %w", planner, err)
		}
		if err := check(); err != nil {
			return res, err
		}
		planMeta := planRun.Attestation.Metadata(nil)
		if _, err = e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, contract.Artifact{Type: contract.ArtifactDesign, Producer: planner + ":planner", Summary: fmt.Sprintf("Implementation plan for #%d", opts.IssueNumber), Body: plan, Metadata: planMeta}); err != nil {
			return res, err
		}
		testMeta := planRun.Attestation.Metadata(nil)
		if st, err = e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, contract.Artifact{Type: contract.ArtifactTestPlan, Producer: planner + ":planner", Summary: fmt.Sprintf("Test plan covering %d acceptance criteria", len(pre.WorkUnit.Acceptance)), Body: testPlan, Metadata: testMeta}); err != nil {
			return res, err
		}
		res.State, res.PhaseCompleted = st, "plan"
		if opts.Phase == "plan" {
			if opts.Handoff {
				progress.next("handoff")
				next := "implement"
				if gate != contract.GateNone {
					next = "review"
				}
				if err := check(); err != nil {
					return res, err
				}
				if st, err = e.LeaseManager.Handoff(ctx, opts.IssueNumber, opts.Owner, "plan", next); err != nil {
					return res, err
				}
				res.State, res.HandedOffToPhase = st, next
			}
			return res, nil
		}
	}
	if gate == contract.GateHuman && (opts.Phase == "review" || opts.Phase == "all" && (runPlan || lease.VerifyApproval(st, pre.WorkUnit) != nil)) {
		progress.next("awaiting-approval")
		if err := check(); err != nil {
			return res, err
		}
		if opts.Handoff {
			if st, err = e.LeaseManager.Handoff(ctx, opts.IssueNumber, opts.Owner, "plan", "review"); err != nil {
				return res, err
			}
			res.State, res.HandedOffToPhase = st, "review"
		} else {
			if st, err = e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "review", "active"); err != nil {
				return res, err
			}
			res.State = st
		}
		res.PausedForHuman, res.PhaseCompleted = true, "plan"
		return res, nil
	}
	if (gate == contract.GateAgent && (runPlan || (opts.Phase == "all" && lease.VerifyApproval(st, pre.WorkUnit) != nil))) || (opts.Phase == "review" && gate != contract.GateHuman) {
		progress.next("review")
		if gate != contract.GateAgent && gate != contract.GateNone {
			return res, fmt.Errorf("%w: gate %q requires an authenticated human decision", ErrPlanApprovalNeeded, gate)
		}
		if st, err = e.LeaseManager.GetCurrentState(ctx, opts.IssueNumber); err != nil {
			return res, err
		}
		res.State = st
		reviewer := pre.Agents["reviewer"]
		res.RoleAgentsUsed["reviewer"] = reviewer
		verdict, reviewed, err := e.RunAgentReview(ctx, wtPath, pre.WorkUnit, wf, st, opts.Owner, reviewer, mode, opts.Instructions, opts.PRNumber)
		res.ReviewVerdict = verdict
		if reviewed != nil {
			st, res.State = reviewed, reviewed
		}
		if err != nil {
			return res, err
		}
		res.PhaseCompleted = "review"
		if opts.Phase == "review" {
			if opts.Handoff {
				progress.next("handoff")
				if err := check(); err != nil {
					return res, err
				}
				if st, err = e.LeaseManager.Handoff(ctx, opts.IssueNumber, opts.Owner, "review", "implement"); err != nil {
					return res, err
				}
				res.State, res.HandedOffToPhase = st, "implement"
			}
			return res, nil
		}
	}
	if opts.Phase != "all" && opts.Phase != "implement" {
		return res, nil
	}
	if st, err = e.LeaseManager.GetCurrentState(ctx, opts.IssueNumber); err != nil {
		return res, err
	}
	res.State = st
	if res.ReviewVerdict == "" && hasApprovedReviewArtifact(st) {
		res.ReviewVerdict = "APPROVED"
	}
	if err := ensurePlanArtifactsInWorktree(wtPath, st); err != nil {
		return res, err
	}
	if err := lease.VerifyApproval(st, pre.WorkUnit); err != nil {
		return res, fmt.Errorf("%w: %w", ErrPlanApprovalNeeded, err)
	}
	if err := check(); err != nil {
		return res, err
	}
	progress.next("implement")
	developer := pre.Agents["developer"]
	res.RoleAgentsUsed["developer"] = developer
	devRunner, err := agent.NewConfiguredRunner(developer, cfg, e.Tracker.BackendName() == "local")
	if err != nil {
		return res, err
	}
	if _, err := e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "implement", "active"); err != nil {
		return res, err
	}
	if err := compileExecutionBundle(wtPath, pre.WorkUnit, wf, st, "developer", opts.PRNumber); err != nil {
		return res, err
	}
	devPrompt := "Execute /swarm-implement step-by-step using the approved plan and test plan."
	checkpointPurpose := "implement"
	if hasFailedValidationArtifact(st) {
		devPrompt = "Execute /swarm-implement step-by-step using the approved plan and test plan. A prior validation check failed; inspect .swarm/active/VALIDATION.md and .swarm/active/WORKUNIT.md and fix all failing checks."
		checkpointPurpose = "repair"
	}
	if opts.Instructions != "" {
		devPrompt += "\nOperator instructions: " + opts.Instructions
	}
	devRun, err := devRunner.Run(ctx, agent.RunRequest{Agent: developer, Role: "developer", Mode: mode, WorktreePath: wtPath, SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md", TaskPrompt: devPrompt})
	if err != nil {
		return res, err
	}
	if err := check(); err != nil {
		return res, err
	}
	summary, body := SummarizeImplementationChanges(ctx, wtPath, opts.IssueNumber)
	progress.next("checkpoint")
	checkpointHead, err := workspace.Checkpoint(ctx, wtPath, branch, workspace.CheckpointOptions{
		WorkUnit: pre.WorkUnit, Purpose: checkpointPurpose, Detail: cfg.CommitMessageDetail(),
	})
	if err != nil {
		return res, err
	}
	if opts.PRNumber != 0 {
		st, err = e.LeaseManager.RecordPRHead(ctx, opts.IssueNumber, opts.Owner, opts.PRNumber, branch, checkpointHead)
	} else {
		if forkMode {
			if err := workspace.PushForkCheckpoint(ctx, opts.RepoRoot, forkRemote, branch, checkpointHead); err != nil {
				return res, err
			}
		}
		st, err = e.LeaseManager.RecordCheckpoint(ctx, opts.IssueNumber, opts.Owner, branch, checkpointHead)
	}
	if err != nil {
		return res, err
	}
	res.State, res.HeadSHA = st, checkpointHead
	devMeta := devRun.Attestation.Metadata(nil)
	if st, err = e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, contract.Artifact{Type: contract.ArtifactImplementation, Producer: developer + ":developer", Verdict: "PASS", Summary: summary, Body: body, Metadata: devMeta}); err != nil {
		return res, err
	}
	res.State = st
	progress.next("test")
	tester := pre.Agents["tester"]
	res.RoleAgentsUsed["tester"] = tester
	testerRunner, err := agent.NewConfiguredRunner(tester, cfg, e.Tracker.BackendName() == "local")
	if err != nil {
		return res, err
	}
	if err := compileExecutionBundle(wtPath, pre.WorkUnit, wf, st, "tester", opts.PRNumber); err != nil {
		return res, err
	}
	testPrompt := "Execute /swarm-validate and record acceptance evidence."
	if opts.Instructions != "" {
		testPrompt += "\nOperator instructions: " + opts.Instructions
	}
	testerRun, err := testerRunner.Run(ctx, agent.RunRequest{Agent: tester, Role: "tester", Mode: mode, WorktreePath: wtPath, SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md", TaskPrompt: testPrompt})
	if err != nil {
		return res, fmt.Errorf("tester agent %q: %w", tester, err)
	}
	if err := check(); err != nil {
		return res, err
	}
	progress.next("required-checks")
	report, err := validation.Validate(ctx, wtPath, pre.WorkUnit, validation.ValidateOptions{BaseSHA: st.BaseSHA, ForkMode: forkMode})
	if report != nil {
		report.Artifact.Metadata = testerRun.Attestation.Metadata(report.Artifact.Metadata)
		updated, updateErr := e.LeaseManager.UpdateArtifact(ctx, opts.IssueNumber, opts.Owner, report.Artifact)
		if updateErr != nil {
			return res, updateErr
		}
		res.State, res.ValidationPassed = updated, report.Passed
	}
	if err != nil {
		return res, err
	}
	res.PhaseCompleted = "implement"
	return res, nil
}

func readRequiredArtifact(wtPath, name string) (string, error) {
	data, err := safefs.ReadFile(wtPath, filepath.Join(".swarm", "active", name), 1<<20)
	if err != nil {
		return "", fmt.Errorf("read .swarm/active/%s: %w", name, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("did not produce .swarm/active/%s", name)
	}
	return string(data), nil
}

// DecomposeIssue partitions a parent issue into sequential or parallel child WorkUnit issues linked via ParentIssue and DependsOn.
func (e *Engine) DecomposeIssue(ctx context.Context, repoRoot string, parentIssueNum int, subtasks []contract.WorkUnit, sequential bool) ([]*tracker.Issue, error) {
	parentIss, err := e.Tracker.GetIssue(ctx, parentIssueNum)
	if err != nil {
		return nil, err
	}
	if parentIss.IsEpic() {
		return nil, fmt.Errorf("issue #%d has type \"Epic\" and does not carry a WorkUnit contract for auto-decomposition", parentIssueNum)
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
		childIss, err := e.Tracker.CreateIssue(ctx, title, body, []string{"swarm:draft"})
		if err != nil {
			return created, err
		}
		if err := e.Tracker.LinkSubIssue(ctx, parentIssueNum, childIss.Number); err != nil {
			return created, fmt.Errorf("link decomposed child issue #%d to parent #%d: %w", childIss.Number, parentIssueNum, err)
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

func compileExecutionBundle(path string, wu *contract.WorkUnit, wf *contract.WorkflowSpec, st *lease.IssueState, role string, prNumber int) error {
	if prNumber != 0 {
		return workspace.CompilePRHarnessBundleAt(path, wu, wf, st.Artifacts, role, st.BaseSHA, prNumber)
	}
	return workspace.CompileHarnessBundleAt(path, wu, wf, st.Artifacts, role, st.BaseSHA)
}

func (e *Engine) RunAgentReview(ctx context.Context, wtPath string, wu *contract.WorkUnit, wf *contract.WorkflowSpec, st *lease.IssueState, owner, reviewerAgent, mode, instructions string, prNumber int) (string, *lease.IssueState, error) {
	if err := e.LeaseManager.CheckClaim(ctx, wu.IssueNumber, owner); err != nil {
		return "", st, fmt.Errorf("%w: %w", ErrLeaseLost, err)
	}
	if _, err := e.LeaseManager.UpdatePhase(ctx, wu.IssueNumber, owner, "review", "active"); err != nil {
		return "", st, err
	}
	if err := ensurePlanArtifactsInWorktree(wtPath, st); err != nil {
		return "", st, err
	}
	if err := compileExecutionBundle(wtPath, wu, wf, st, "reviewer", prNumber); err != nil {
		return "", st, err
	}
	runner, err := agent.NewConfiguredRunner(reviewerAgent, e.Config, e.Tracker.BackendName() == "local")
	if err != nil {
		return "", st, err
	}
	req := agent.RunRequest{
		Agent: reviewerAgent, Role: "reviewer", Mode: mode, WorktreePath: wtPath,
		SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md",
		TaskPrompt:   "Execute /swarm-review: audit .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md against .swarm/active/WORKUNIT.md and write exactly one first-line verdict: VERDICT: APPROVED or VERDICT: CHANGES_REQUESTED.",
	}
	if instructions != "" {
		req.TaskPrompt += "\nOperator instructions: " + instructions
	}
	revRun, err := agent.RunReview(ctx, runner, req)
	if err != nil {
		return "", st, err
	}
	review, err := readRequiredArtifact(wtPath, "REVIEW.md")
	if err != nil {
		return "", st, fmt.Errorf("reviewer agent %q: %w", reviewerAgent, err)
	}
	verdict, err := ParseReviewVerdict(review)
	if err != nil {
		return "", st, err
	}
	if err := e.LeaseManager.CheckClaim(ctx, wu.IssueNumber, owner); err != nil {
		return verdict, st, fmt.Errorf("%w: %w", ErrLeaseLost, err)
	}
	var revAtt contract.RoleAttestation
	if revRun != nil {
		revAtt = revRun.Attestation
	} else {
		revAtt = runner.Provenance("reviewer", true)
	}
	revMeta := revAtt.Metadata(map[string]string{"kind": "agent", "actor": owner})
	for key, value := range lease.ReviewBindings(st) {
		revMeta[key] = value
	}
	updated, err := e.LeaseManager.UpdateArtifact(ctx, wu.IssueNumber, owner, contract.Artifact{
		Type: contract.ArtifactReview, Producer: reviewerAgent + ":reviewer", Verdict: verdict,
		Summary: fmt.Sprintf("Automated plan review verdict: %s", verdict), Body: review,
		Metadata: revMeta,
	})
	if err != nil {
		return verdict, st, err
	}
	if verdict != "APPROVED" {
		if err := e.LeaseManager.CheckClaim(ctx, wu.IssueNumber, owner); err != nil {
			return verdict, updated, fmt.Errorf("%w: %w", ErrLeaseLost, err)
		}
		return verdict, updated, fmt.Errorf("%w: reviewer %q returned %s", ErrPlanReviewRejected, reviewerAgent, verdict)
	}
	if err := e.LeaseManager.CheckClaim(ctx, wu.IssueNumber, owner); err != nil {
		return verdict, updated, fmt.Errorf("%w: %w", ErrLeaseLost, err)
	}
	return verdict, updated, nil
}

// ParseReviewVerdict accepts an exact first non-empty verdict line. Review
// narrative may follow, but a second verdict is contradictory and rejected.
func ParseReviewVerdict(content string) (string, error) {
	var verdict string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if verdict == "" {
			switch line {
			case "VERDICT: APPROVED":
				verdict = "APPROVED"
			case "VERDICT: CHANGES_REQUESTED":
				verdict = "CHANGES_REQUESTED"
			default:
				return "", errors.New("review must begin with VERDICT: APPROVED or VERDICT: CHANGES_REQUESTED")
			}
			continue
		}
		if strings.HasPrefix(line, "VERDICT:") {
			return "", errors.New("review contains more than one verdict")
		}
	}
	if verdict == "" {
		return "", errors.New("review is missing an exact verdict")
	}
	return verdict, nil
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

func hasFailedValidationArtifact(st *lease.IssueState) bool {
	if st == nil {
		return false
	}
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactValidation && strings.EqualFold(a.Verdict, "FAIL") {
			return true
		}
	}
	return false
}

// ensurePlanArtifactsInWorktree restores the authoritative state artifacts on
// every phase transition. Existing local files are never accepted as a plan.
func ensurePlanArtifactsInWorktree(wtPath string, st *lease.IssueState) error {
	if st == nil {
		return errors.New("missing authoritative issue state")
	}
	safe, err := safefs.Open(wtPath)
	if err != nil {
		return err
	}
	defer safe.Close()
	active, err := safe.Sub(filepath.Join(".swarm", "active"), true, 0755)
	if err != nil {
		return err
	}
	defer active.Close()
	artifacts := map[contract.ArtifactType]string{}
	for _, artifact := range st.Artifacts {
		switch artifact.Type {
		case contract.ArtifactDesign, contract.ArtifactTestPlan, contract.ArtifactReview, contract.ArtifactValidation:
			artifacts[artifact.Type] = artifact.Body
		}
	}
	required := map[contract.ArtifactType]string{
		contract.ArtifactDesign: "PLAN.md", contract.ArtifactTestPlan: "TEST_PLAN.md",
	}
	for kind, name := range required {
		body := artifacts[kind]
		if strings.TrimSpace(body) == "" {
			return fmt.Errorf("missing %s in authoritative state (run `swarm issue work <issue> --phase plan` first)", name)
		}
		if err := active.WriteFile(name, []byte(body), 0o644); err != nil {
			return err
		}
	}
	if review, ok := artifacts[contract.ArtifactReview]; ok {
		if err := active.WriteFile("REVIEW.md", []byte(review), 0o644); err != nil {
			return err
		}
	}
	if val, ok := artifacts[contract.ArtifactValidation]; ok {
		if err := active.WriteFile("VALIDATION.md", []byte(val), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// SummarizeImplementationChanges describes a worktree's uncommitted changes for
// an implementation artifact: a one-line summary and a Markdown body.
func SummarizeImplementationChanges(ctx context.Context, wtPath string, issueNum int) (string, string) {
	statusOut, _ := gitcredential.Output(ctx, wtPath, "status", "--short")
	diffStat, _ := gitcredential.Output(ctx, wtPath, "diff", "--stat", "HEAD")

	lines := strings.Split(strings.TrimSpace(string(statusOut)), "\n")
	count := 0
	var body strings.Builder
	fmt.Fprintf(&body, "# Implementation Summary (Issue #%d)\n\n## Working Tree Changes\n```text\n", issueNum)
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
