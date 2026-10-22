package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

func newDoctorCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify git, GitHub auth, .swarm/ contracts, and agent binaries (claude, codex, opencode, omp)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repoRoot := rootOpts.resolveRepoRoot()
			checkGHAuth := !strings.EqualFold(rootOpts.Backend, "local")
			checks := harness.RunDoctor(repoRoot, checkGHAuth)
			if cfg, err := harness.LoadConfig(repoRoot); err == nil {
				execution, err := contract.ResolveExecution(cfg, os.Getenv("SWARM_EXECUTION_MODE"))
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
				checks = append(checks, harness.DoctorCheck{Category: "execution", Name: "mode", Status: status, Detail: detail})
				checks = append(checks, doctorAgentChecks(cfg, !checkGHAuth, agent.Preflight)...)
			}
			if checkGHAuth && (strings.EqualFold(rootOpts.Backend, "github") || rootOpts.trackerOverride != nil || rootOpts.Repo != "" || hasGitRemote(repoRoot)) {
				tr, err := rootOpts.resolveTracker(cmd)
				if err != nil {
					checks = append(checks,
						harness.DoctorCheck{
							Category: "tracker",
							Name:     "github-epic-type",
							Status:   "WARN",
							Detail:   fmt.Sprintf("unable to check GitHub Epic issue type (configure GitHub remote and token): %v", err),
						},
						harness.DoctorCheck{
							Category: "tracker",
							Name:     "github-epic-auth",
							Status:   "WARN",
							Detail:   fmt.Sprintf("unable to check maintainer create-epic permission: %v", err),
						},
					)
				} else {
					if err := tr.CheckIssueType(cmd.Context(), "Epic"); err != nil {
						checks = append(checks, harness.DoctorCheck{
							Category: "tracker",
							Name:     "github-epic-type",
							Status:   "WARN",
							Detail:   err.Error(),
						})
					} else {
						checks = append(checks, harness.DoctorCheck{
							Category: "tracker",
							Name:     "github-epic-type",
							Status:   "OK",
							Detail:   "organization issue type \"Epic\" is enabled",
						})
					}
					if err := tr.Authorize(cmd.Context(), "create-epic"); err != nil {
						checks = append(checks, harness.DoctorCheck{
							Category: "tracker",
							Name:     "github-epic-auth",
							Status:   "WARN",
							Detail:   fmt.Sprintf("current actor cannot publish Epics (maintainer/admin role required): %v", err),
						})
					} else {
						checks = append(checks, harness.DoctorCheck{
							Category: "tracker",
							Name:     "github-epic-auth",
							Status:   "OK",
							Detail:   "current actor is authorized to publish Epics (maintain/admin)",
						})
					}
				}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "CATEGORY   CHECK                                    STATUS   DETAIL")
			fmt.Fprintln(out, "--------   -----                                    ------   ------")
			failures := 0
			for _, c := range checks {
				fmt.Fprintf(out, "%-10s %-40s %-8s %s\n", c.Category, c.Name, c.Status, c.Detail)
				if c.Status == "FAIL" {
					failures++
				}
			}
			if failures > 0 {
				return fmt.Errorf("doctor detected %d failing check(s)", failures)
			}
			return nil
		},
	}
	return cmd
}

// Check selected roles even when an agent's allowlist excludes that assignment.
func doctorAgentChecks(cfg *contract.SwarmConfig, local bool, preflight func(string, *contract.SwarmConfig, bool, string) (string, error)) []harness.DoctorCheck {
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
			checks = append(checks, harness.DoctorCheck{Category: "execution", Name: name + "/" + role, Status: status, Detail: detail})
		}
	}
	return checks
}
