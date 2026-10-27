package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newDoctorCmd(rootOpts *RootOptions) *cobra.Command {
	return newDoctorCmdWithRunner(rootOpts, nil)
}

func newDoctorCmdWithRunner(rootOpts *RootOptions, factory func(context.Context, func(progress.Event)) harness.DoctorRunner) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify git, GitHub auth, .swarm/ contracts, and agent binaries (claude, codex, opencode, omp)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot := rootOpts.resolveRepoRoot()
			checkGHAuth := !strings.EqualFold(rootOpts.Backend, "local")
			var observer func(progress.Event)
			if !asJSON {
				observer = operationProgressPrinter(progressWriter(cmd))
			}
			var run harness.DoctorRunner
			if factory != nil {
				run = factory(cmd.Context(), observer)
			} else {
				run = harness.NewDoctorRunner(cmd.Context(), observer, nil, nil)
			}
			checks := harness.RunDoctorWithRunner(repoRoot, checkGHAuth, run)
			checks = append(checks, run.Run("system", "git-identity", func() harness.DoctorCheck {
				if err := workspace.CheckCommitIdentity(cmd.Context(), repoRoot); err != nil {
					return harness.DoctorCheck{Category: "system", Name: "git-identity", Status: "FAIL", Detail: err.Error()}
				} else {
					return harness.DoctorCheck{Category: "system", Name: "git-identity", Status: "OK", Detail: "checkpoint commits can be authored"}
				}
			}))
			checks = append(checks, run.Run("execution", "dependency-caches", func() harness.DoctorCheck {
				return doctorDependencyCacheCheck(rootOpts.nodeConfigPath())
			}))
			if cfg, err := harness.LoadConfig(repoRoot); err == nil {
				checks = append(checks, run.Run("execution", "mode", func() harness.DoctorCheck {
					execution, err := contract.ResolveExecution(cfg, rootOpts.getenv("SWARM_EXECUTION_MODE"))
					status, detail := "OK", ""
					if err != nil {
						status, detail = "FAIL", err.Error()
					} else {
						detail = "mode=" + execution.Mode + "; policy is read from each task's approved base before execution"
						if execution.Mode == contract.ExecutionIsolated {
							detail += "; backend=" + execution.Backend
						}
						if execution.Mode == contract.ExecutionTrustedLocal {
							status = "WARN"
							detail += "; workers share operator OS permissions"
						}
					}
					return harness.DoctorCheck{Category: "execution", Name: "mode", Status: status, Detail: detail}
				}))
				preflight := func(name string, cfg *contract.SwarmConfig, local bool, role string) (string, error) {
					return agent.PreflightWithOptions(name, cfg, local, role, rootOpts.runnerOptions())
				}
				checks = append(checks, doctorAgentChecksWithRunner(cfg, !checkGHAuth, preflight, run)...)
				checks = append(checks, run.Run("security", "plan-reviewers", func() harness.DoctorCheck { return doctorReviewerCheck(cfg) }))
			}
			if checkGHAuth && (strings.EqualFold(rootOpts.Backend, "github") || rootOpts.trackerOverride != nil || rootOpts.Repo != "" || hasGitRemote(repoRoot)) {
				var tr tracker.Tracker
				var resolveErr error
				checks = append(checks, run.Run("tracker", "github-epic-type", func() harness.DoctorCheck {
					tr, resolveErr = rootOpts.resolveTracker(cmd)
					if resolveErr != nil {
						return harness.DoctorCheck{Category: "tracker", Name: "github-epic-type", Status: "WARN", Detail: fmt.Sprintf("unable to check GitHub Epic issue type (configure GitHub remote and token): %v", resolveErr)}
					}
					if err := tr.CheckIssueType(cmd.Context(), "Epic"); err != nil {
						return harness.DoctorCheck{Category: "tracker", Name: "github-epic-type", Status: "WARN", Detail: err.Error()}
					}
					return harness.DoctorCheck{Category: "tracker", Name: "github-epic-type", Status: "OK", Detail: "organization issue type \"Epic\" is enabled"}
				}))
				checks = append(checks, run.Run("tracker", "github-epic-auth", func() harness.DoctorCheck {
					if resolveErr != nil {
						return harness.DoctorCheck{Category: "tracker", Name: "github-epic-auth", Status: "WARN", Detail: fmt.Sprintf("unable to check maintainer create-epic permission: %v", resolveErr)}
					}
					if err := tr.Authorize(cmd.Context(), "create-epic"); err != nil {
						return harness.DoctorCheck{Category: "tracker", Name: "github-epic-auth", Status: "WARN", Detail: fmt.Sprintf("current actor cannot publish Epics (maintainer/admin role required): %v", err)}
					}
					return harness.DoctorCheck{Category: "tracker", Name: "github-epic-auth", Status: "OK", Detail: "current actor is authorized to publish Epics (maintain/admin)"}
				}))
			}

			failures := 0
			for _, c := range checks {
				if c.Status == "FAIL" {
					failures++
				}
			}
			if asJSON {
				status := "passed"
				if failures > 0 {
					status = "failed"
				}
				if err := writeJSON(cmd, map[string]any{"kind": "doctor", "status": status, "checks": checks}); err != nil {
					return err
				}
			} else {
				out := cmd.OutOrStdout()
				fmt.Fprintln(out, "CATEGORY   CHECK                                    STATUS   DETAIL")
				fmt.Fprintln(out, "--------   -----                                    ------   ------")
				for _, c := range checks {
					fmt.Fprintf(out, "%-10s %-40s %-8s %s\n", c.Category, c.Name, c.Status, c.Detail)
				}
			}
			if failures > 0 {
				return fmt.Errorf("doctor detected %d failing check(s)", failures)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit doctor checks as JSON")
	return cmd
}

// doctorReviewerCheck reports who can satisfy plan gates under the local policy.
func doctorReviewerCheck(cfg *contract.SwarmConfig) harness.DoctorCheck {
	sec := cfg.Security
	humans, agents := "any repository writer", "any configured reviewer"
	if len(sec.ReviewerCollaborators) > 0 {
		humans = strings.Join(sec.ReviewerCollaborators, ", ")
	}
	if len(sec.ReviewerAgents) > 0 {
		agents = strings.Join(sec.ReviewerAgents, ", ")
	}
	detail := fmt.Sprintf("default_gate=%s; humans: %s; agents: %s; independent_review=%t", cfg.DefaultGate, humans, agents, sec.IndependentReview)
	status := "OK"
	if cfg.DefaultGate == contract.GateHuman && len(sec.ReviewerCollaborators) == 0 {
		status = "WARN"
		detail += "; set security.reviewer_collaborators so plan reviews have named, notified reviewers"
	}
	return harness.DoctorCheck{Category: "security", Name: "plan-reviewers", Status: status, Detail: detail}
}

// Check selected roles even when an agent's allowlist excludes that assignment.
func doctorAgentChecks(cfg *contract.SwarmConfig, local bool, preflight func(string, *contract.SwarmConfig, bool, string) (string, error)) []harness.DoctorCheck {
	return doctorAgentChecksWithRunner(cfg, local, preflight, nil)
}

func doctorAgentChecksWithRunner(cfg *contract.SwarmConfig, local bool, preflight func(string, *contract.SwarmConfig, bool, string) (string, error), run harness.DoctorRunner) []harness.DoctorCheck {
	var checks []harness.DoctorCheck
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, role := range []string{"planner", "developer", "tester", "reviewer"} {
			selected := cfg.Roles[role]
			if selected == "" {
				selected = cfg.DefaultAgent
			}
			allowed := len(cfg.Agents[name].Roles) == 0
			for _, permitted := range cfg.Agents[name].Roles {
				allowed = allowed || permitted == role
			}
			if selected != name && !allowed {
				continue
			}
			checks = append(checks, run.Run("execution", name+"/"+role, func() harness.DoctorCheck {
				detail, err := preflight(name, cfg, local, role)
				status := "OK"
				if err != nil {
					status, detail = "FAIL", err.Error()
					if selected != name {
						status = "OPTIONAL"
					}
				}
				if selected == name {
					detail = "selected; " + detail
				}
				return harness.DoctorCheck{Category: "execution", Name: name + "/" + role, Status: status, Detail: detail}
			}))
		}
	}
	return checks
}

// doctorDependencyCacheCheck reports how each ecosystem's dependencies reach
// sandboxes, naming those that share host caches. The node profile opt-in is
// a warning because it exposes every package the host has fetched.
func doctorDependencyCacheCheck(profilePath string) harness.DoctorCheck {
	mode, err := node.DependencyCacheMode(profilePath)
	status, detail := "OK", dependencyCacheSummary(mode)+"; Go detects the root go.mod only"
	if err != nil {
		status, detail = "WARN", detail+"; "+err.Error()
	} else if mode == sandbox.SharedHostCaches {
		status = "WARN"
	}
	return harness.DoctorCheck{Category: "execution", Name: "dependency-caches", Status: status, Detail: detail}
}

// dependencyCacheSummary lists each ecosystem's mode, grouping those that
// share host caches.
func dependencyCacheSummary(mode sandbox.DependencyCacheMode) string {
	var parts, shared []string
	for _, eco := range sandbox.DependencyCacheModes(mode) {
		if strings.HasPrefix(eco.Mode, "host-shared (no fetch-only prefetch") {
			shared = append(shared, eco.Name)
			continue
		}
		parts = append(parts, eco.Name+": "+eco.Mode)
	}
	if len(shared) > 0 {
		parts = append(parts, "sharing host caches read-only (no fetch-only prefetch): "+strings.Join(shared, ", "))
	}
	return strings.Join(parts, "; ")
}
