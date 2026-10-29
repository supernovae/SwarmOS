package workflow

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/safefs"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"strings"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
)

func (r *workRun) plan() (bool, error) {
	var err error
	r.rejectedPlan, r.rejectedTest, r.rejectedFindings = rejectedPlanContext(r.st)
	r.runPlan = r.opts.Phase == "all" || r.opts.Phase == "plan"
	if r.opts.Phase == "all" && (hasDesignAndTestPlanArtifacts(r.st) && !hasRejectedReview(r.st)) {
		r.runPlan = false
	}
	if r.runPlan {
		if err := r.admit("plan"); err != nil {
			return true, err
		}
		r.progress.next("plan")
		if err := r.check(); err != nil {
			return true, err
		}
		if strings.EqualFold(r.cfg.PlanMode, "interactive") {
			r.res.RoleAgentsUsed["planner"] = "interactive"
			if err := compileSetupBundle(r.ctx, r.opts.SetupProgress, r.wtPath, r.pre.WorkUnit, r.wf, r.st, "planner", r.opts.PRNumber); err != nil {
				return true, err
			}
			if r.st, err = r.e.LeaseManager.UpdatePhase(r.ctx, r.opts.IssueNumber, r.opts.Owner, "plan", "active"); err != nil {
				return true, err
			}
			r.res.State = r.st
			r.res.PausedForHuman = true
			r.res.PhaseCompleted = "awaiting-plan"
			return true, nil
		}
		planner := r.pre.Agents["planner"]
		r.res.RoleAgentsUsed["planner"] = planner
		runner, err := r.e.configuredRunner(r.ctx, planner, r.cfg, r.e.Tracker.BackendName() == "local")
		if err != nil {
			return true, err
		}
		if _, err := r.e.LeaseManager.UpdatePhase(r.ctx, r.opts.IssueNumber, r.opts.Owner, "plan", "active"); err != nil {
			return true, err
		}
		if err := compileSetupBundle(r.ctx, r.opts.SetupProgress, r.wtPath, r.pre.WorkUnit, r.wf, r.st, "planner", r.opts.PRNumber); err != nil {
			return true, err
		}
		planPrompt := "Execute /swarm-plan and write .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md."
		if r.opts.instructionsFor("planner") != "" {
			planPrompt += "\nOperator instructions: " + r.opts.instructionsFor("planner")
		}
		if r.rejectedFindings != "" {
			planPrompt += "\nAddress the previous CHANGES_REQUESTED findings and revise PLAN.md or TEST_PLAN.md:\n" + r.rejectedFindings
		}
		if err := r.admit("plan"); err != nil {
			return true, err
		}
		planRun, err := runner.Run(r.ctx, agent.RunRequest{Agent: planner, Role: "planner", Mode: r.mode, WorktreePath: r.wtPath, SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md", TaskPrompt: revisionTaskPrompt(planPrompt, r.st, r.opts.PRNumber), OperatorInstructions: r.opts.instructionsFor("planner"), Dependencies: r.deps})
		if paused, pauseErr := r.pauseInterrupted(err, "plan", false); paused {
			return true, pauseErr
		}
		if err != nil {
			r.st, err = r.e.failedRun(r.ctx, r.st, r.opts, planRun, err, "planner")
			r.res.State = r.st
			return true, err
		}
		plan, err := readRequiredArtifact(r.wtPath, "PLAN.md")
		if err != nil {
			return true, fmt.Errorf("planner agent %q: %w", planner, err)
		}
		testPlan, err := readRequiredArtifact(r.wtPath, "TEST_PLAN.md")
		if err != nil {
			return true, fmt.Errorf("planner agent %q: %w", planner, err)
		}
		if err := r.check(); err != nil {
			return true, err
		}
		if r.rejectedFindings != "" && canonicalReport(plan) == r.rejectedPlan && canonicalReport(testPlan) == r.rejectedTest {
			return true, fmt.Errorf("issue #%d re-plan is unchanged after CHANGES_REQUESTED; revise PLAN.md or TEST_PLAN.md before review", r.opts.IssueNumber)
		}
		// Keep canonical plan files independent of steering evidence in artifacts.
		plan = guidedReport(plan, planRun)
		testPlan = guidedReport(testPlan, planRun)
		observeRun(planRun, r.st, false)
		planMeta := runMetadata(planRun, nil)
		if _, err = r.e.LeaseManager.UpdateArtifact(r.ctx, r.opts.IssueNumber, r.opts.Owner, contract.Artifact{Type: contract.ArtifactDesign, Producer: planner + ":planner", Summary: fmt.Sprintf("Implementation plan for #%d", r.opts.IssueNumber), Body: plan, Metadata: planMeta}); err != nil {
			return true, err
		}
		testMeta := runMetadata(planRun, nil)
		if r.st, err = r.e.LeaseManager.UpdateArtifact(r.ctx, r.opts.IssueNumber, r.opts.Owner, contract.Artifact{Type: contract.ArtifactTestPlan, Producer: planner + ":planner", Summary: fmt.Sprintf("Test plan covering %d acceptance criteria", len(r.pre.WorkUnit.Acceptance)), Body: testPlan, Metadata: testMeta}); err != nil {
			return true, err
		}
		r.res.State, r.res.PhaseCompleted = r.st, "plan"
		if r.stopForSplit() {
			return true, nil
		}
		if done, err := r.finishPlanHandoff(); done {
			return true, err
		}
	}
	return false, nil
}

func (r *workRun) finishPlanHandoff() (bool, error) {
	var err error
	if r.opts.Phase == "plan" {
		if r.opts.Handoff {
			r.progress.next("handoff")
			next := "implement"
			if r.gate != contract.GateNone {
				next = "review"
			}
			if err := r.check(); err != nil {
				return true, err
			}
			if r.st, err = r.e.LeaseManager.Handoff(r.ctx, r.opts.IssueNumber, r.opts.Owner, "plan", next); err != nil {
				return true, err
			}
			r.res.State, r.res.HandedOffToPhase = r.st, next
		}
		return true, nil
	}
	return false, nil
}

// executeCheckpointPlan never enters the implementation/checkpoint pipeline.
func (e *Engine) executeCheckpointPlan(ctx context.Context, opts WorkOptions) (out *WorkResult, runErr error) {
	if e.ForkMode || opts.ForkMode || opts.PRNumber != 0 || opts.Handoff || (opts.Phase != "plan" && opts.Phase != "review") || (opts.PlanCheckpoint && opts.Phase != "plan") {
		return nil, fmt.Errorf("checkpoint planning requires direct issue plan or authenticated recovery review")
	}
	if opts.Owner == "" {
		var err error
		opts.Owner, err = e.Tracker.CurrentUser(ctx)
		if err != nil {
			return nil, err
		}
	}
	if opts.ClaimID != "" {
		e.LeaseManager.BindClaim(opts.IssueNumber, opts.ClaimID)
	}
	p, err := e.LeaseManager.PrepareCheckpoint(ctx, opts.IssueNumber, opts.Owner, opts.ExpectedHead, opts.CandidateHead, false)
	if err != nil {
		return nil, err
	}
	cfg, err := harness.LoadConfigAt(e.repoRoot, p.State.BaseSHA)
	if err != nil {
		return nil, err
	}
	execution, err := contract.ResolveExecution(cfg, e.getenv("SWARM_EXECUTION_MODE"))
	if err != nil {
		return nil, err
	}
	if execution.Mode != contract.ExecutionIsolated || execution.Backend != contract.BackendNative || strings.EqualFold(cfg.PlanMode, "interactive") {
		return nil, fmt.Errorf("checkpoint planning requires native isolated Codex or harness-tools Bubblewrap and agent plan mode")
	}
	reporter := opts.SetupProgress
	if reporter == nil {
		reporter = startWorkProgress(ctx, opts.IssueNumber, opts.OnProgress)
		defer reporter.Close()
	}
	reporter.mu.Lock()
	reporter.issue = opts.IssueNumber
	reporter.mu.Unlock()
	ctx = progress.WithActivity(ctx, reporter.activityTracker())
	reporter.next("preflight")
	defer func() { reporter.finish(out, runErr) }()
	base, taskDigest, policyDigest := p.State.BaseSHA, p.State.TaskDigest, p.State.PolicyDigest
	requestedGate, err := contract.ResolveGate(opts.Gate, p.State.EffectiveGate, p.State.EffectiveGate)
	if err != nil {
		return nil, err
	}
	role := "planner"
	if opts.Phase == "review" {
		role = "reviewer"
	}
	name := resolveRoleAgent(cfg, role, opts)
	if role == "reviewer" && len(cfg.Security.ReviewerAgents) > 0 && !slices.Contains(cfg.Security.ReviewerAgents, name) {
		return nil, fmt.Errorf("reviewer is not allowlisted")
	}
	if role == "planner" {
		if err = e.LeaseManager.CheckPlanReplan(p.State); err != nil {
			return nil, err
		}
	}
	// Inspection and dependency preparation happen before fresh claim acquisition.
	if e.NewRunner == nil {
		if _, err = agent.PreflightWithOptions(name, cfg, e.Tracker.BackendName() == "local", role, e.runnerOptions(ctx)); err != nil {
			return nil, err
		}
	}
	runner, err := e.configuredRunner(ctx, name, cfg, e.Tracker.BackendName() == "local")
	if err != nil {
		return nil, err
	}
	deps, err := e.AgentDependencies(ctx, cfg, e.repoRoot, p.State.BaseSHA, role)
	if err != nil {
		return nil, err
	}
	if p.ClaimRequired {
		if role != "planner" {
			return nil, lease.ErrClaimFence
		}
		st, err := e.LeaseManager.AcquirePreparedCheckpoint(ctx, opts.IssueNumber, p)
		if err != nil {
			return nil, err
		}
		if opts.OnClaim != nil {
			if err = opts.OnClaim(st); err != nil {
				_, releaseErr := e.LeaseManager.Release(ctx, opts.IssueNumber, opts.Owner, "checkpoint plan token setup failed")
				return nil, errors.Join(err, releaseErr)
			}
		}
	}
	if err = e.LeaseManager.CheckPreparedPlan(ctx, opts.IssueNumber, p); err != nil {
		return nil, err
	}
	if err = e.admitStage(ctx, opts.IssueNumber, opts.Owner, opts.Phase); err != nil {
		return nil, err
	}
	if requestedGate != "" && requestedGate != p.State.EffectiveGate {
		if _, err = e.LeaseManager.RequireGate(ctx, opts.IssueNumber, opts.Owner, requestedGate); err != nil {
			return nil, err
		}
	}
	if role == "planner" {
		if _, err = e.LeaseManager.UpdatePhase(ctx, opts.IssueNumber, opts.Owner, "plan", "active"); err != nil {
			return nil, err
		}
	}
	// Admission may update budget evidence. Pin that exact authorized revision.
	p, err = e.LeaseManager.PrepareCheckpoint(ctx, opts.IssueNumber, opts.Owner, opts.ExpectedHead, opts.CandidateHead, false)
	if err != nil {
		return nil, err
	}
	if p.State.BaseSHA != base || p.State.TaskDigest != taskDigest || p.State.PolicyDigest != policyDigest {
		return nil, lease.ErrClaimFence
	}
	issue, err := e.Tracker.GetIssue(ctx, opts.IssueNumber)
	if err != nil {
		return nil, err
	}
	wu, err := contract.ParseWorkUnitFromIssue(opts.IssueNumber, issue.Title, issue.Body)
	if err != nil {
		return nil, err
	}
	wf, err := harness.LoadWorkflowAt(e.repoRoot, wu.Workflow, p.State.BaseSHA)
	if err != nil {
		return nil, err
	}
	reporter.next("workspace")
	snapshot, err := os.MkdirTemp("", "swarm-checkpoint-plan-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(snapshot)
	if _, err = gitcredential.Output(ctx, e.repoRoot, "clone", "--shared", "--no-checkout", "--", e.repoRoot, snapshot); err != nil {
		return nil, err
	}
	if _, err = gitcredential.Output(ctx, snapshot, "checkout", "--detach", opts.CandidateHead); err != nil {
		return nil, err
	}
	if err = checkpointPlanProjection(ctx, snapshot, p.State.BaseSHA); err != nil {
		return nil, err
	}
	if err = compileSetupBundle(ctx, opts.SetupProgress, snapshot, wu, wf, p.State, role, 0); err != nil {
		return nil, err
	}
	if err = e.LeaseManager.CheckPreparedPlan(ctx, opts.IssueNumber, p); err != nil {
		return nil, err
	}
	result := &WorkResult{IssueNumber: opts.IssueNumber, BaseSHA: p.State.BaseSHA, HeadSHA: p.State.HeadSHA, BranchName: p.State.Branch, State: p.State, RoleAgentsUsed: map[string]string{role: name}}
	e.Config = cfg
	reporter.next(opts.Phase)
	if role == "reviewer" {
		_, candidate, present, bindingErr := lease.CheckpointPlanHeads(p.State)
		if !present || bindingErr != nil || candidate != opts.CandidateHead {
			return nil, lease.ErrApprovalRequired
		}
		if p.State.EffectiveGate == contract.GateHuman {
			return nil, ErrPlanApprovalNeeded
		}
		verdict, st, err := e.runAgentReview(ctx, snapshot, wu, wf, p.State, opts.Owner, name, "headless", opts.instructionsFor(role), 0, opts, opts.SetupProgress)
		result.State, result.ReviewVerdict, result.PhaseCompleted = st, verdict, "review"
		return result, err
	}
	before, err := checkpointPlanFiles(snapshot)
	if err != nil {
		return nil, err
	}
	check := func(ctx context.Context, paths, deleted []string) error {
		if err := checkpointPlanImportPolicy(paths, deleted); err != nil {
			return err
		}
		return e.LeaseManager.CheckPreparedPlan(ctx, opts.IssueNumber, p)
	}
	_, _, findings := rejectedPlanContext(p.State)
	planPrompt := fmt.Sprintf("Execute /swarm-plan for preserved candidate %s replacing predecessor %s. Write only .swarm/active/PLAN.md and .swarm/active/TEST_PLAN.md. Reconciliation is a separate operator action.\n%s", opts.CandidateHead, opts.ExpectedHead, opts.instructionsFor(role))
	if findings != "" {
		planPrompt += "\nAddress the previous CHANGES_REQUESTED findings:\n" + findings
	}
	run, err := runner.Run(ctx, agent.RunRequest{Agent: name, Role: role, Mode: "headless", ReadOnly: true, WorktreePath: snapshot, SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md", TaskPrompt: planPrompt, OperatorInstructions: opts.instructionsFor(role), Dependencies: deps, CheckImport: check})
	if err != nil {
		return result, err
	}
	after, err := checkpointPlanFiles(snapshot)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(before, after) {
		return nil, fmt.Errorf("checkpoint planner modified source or authority inputs")
	}
	plan, err := readRequiredArtifact(snapshot, "PLAN.md")
	if err != nil {
		return nil, err
	}
	test, err := readRequiredArtifact(snapshot, "TEST_PLAN.md")
	if err != nil {
		return nil, err
	}
	if err = check(ctx, []string{".swarm/active/PLAN.md", ".swarm/active/TEST_PLAN.md"}, nil); err != nil {
		return nil, err
	}
	plan = fmt.Sprintf("Preserved candidate: %s\nAuthoritative predecessor: %s\n\n%s", opts.CandidateHead, opts.ExpectedHead, guidedReport(plan, run))
	st, err := e.LeaseManager.RecordCheckpointPlan(ctx, opts.IssueNumber, p, contract.Artifact{Type: contract.ArtifactDesign, Producer: name + ":planner", Summary: "Preserved checkpoint replacement plan", Body: plan, Metadata: runMetadata(run, nil)}, contract.Artifact{Type: contract.ArtifactTestPlan, Producer: name + ":planner", Summary: "Preserved checkpoint test plan", Body: guidedReport(test, run), Metadata: runMetadata(run, nil)})
	result.State, result.PhaseCompleted = st, "plan"
	return result, err
}

func checkpointPlanImportPolicy(paths, deleted []string) error {
	if len(deleted) > 0 {
		return fmt.Errorf("checkpoint planner cannot delete outputs")
	}
	for _, path := range paths {
		if path != ".swarm/active/PLAN.md" && path != ".swarm/active/TEST_PLAN.md" {
			return fmt.Errorf("checkpoint planner cannot import %s", path)
		}
	}
	return nil
}

// Replace candidate instruction surfaces with approved-base files in the private
// snapshot before compiling the existing trusted bundle and pointer projection.
func checkpointPlanProjection(ctx context.Context, root, base string) error {
	authority := func(path string) bool {
		return path == ".swarm" || strings.HasPrefix(path, ".swarm/") || path == ".claude" || strings.HasPrefix(path, ".claude/") || path == ".opencode" || strings.HasPrefix(path, ".opencode/") || filepath.Base(path) == "AGENTS.md" || filepath.Base(path) == "CLAUDE.md"
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			return filepath.SkipDir
		}
		if !authority(rel) {
			return nil
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return err
	}
	entries, err := listTree(ctx, root, base)
	if err != nil {
		return err
	}
	safe, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer safe.Close()
	for path, entry := range entries {
		if !authority(path) || strings.HasPrefix(path, ".swarm/active/") {
			continue
		}
		if entry.kind != "blob" || (entry.mode != "100644" && entry.mode != "100755") {
			return fmt.Errorf("approved instruction path is not a regular file: %s", path)
		}
		data, err := gitcredential.Output(ctx, root, "cat-file", "blob", entry.object)
		if err != nil {
			return err
		}
		if err = safe.WriteFile(path, data, 0644); err != nil {
			return err
		}
	}
	return nil
}

// A second check also covers deterministic runners and source changes outside
// the sandbox import channel. Output regularity is checked by readRequiredArtifact.
func checkpointPlanFiles(root string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			return filepath.SkipDir
		}
		if rel == ".swarm/active/PLAN.md" || rel == ".swarm/active/TEST_PLAN.md" {
			return nil
		}
		if d.IsDir() {
			files[rel] = "dir"
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			files[rel] = "link:" + target
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special snapshot file: %s", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = fmt.Sprintf("%o:%x", info.Mode(), sha256.Sum256(data))
		return nil
	})
	return files, err
}
