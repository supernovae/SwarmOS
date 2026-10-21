package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
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
