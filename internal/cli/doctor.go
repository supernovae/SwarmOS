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
