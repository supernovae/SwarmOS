package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/fork"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newForkRequestStatusCmd(o *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "request-status <issue> <request-id>",
		Short: "Observe the original public fork request without resuming work",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !o.Fork {
				return fmt.Errorf("request-status requires --fork")
			}
			issue, err := strconv.Atoi(args[0])
			if err != nil || issue <= 0 {
				return fmt.Errorf("issue must be a positive integer")
			}
			id, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("request ID must be a positive int64")
			}
			tr, err := o.resolveTracker(cmd)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(tr.BackendName(), "github") {
				return fmt.Errorf("request-status requires the github backend")
			}
			root, remote := o.resolveRepoRoot(), o.effectiveUpstreamRemote()
			ns, err := workspace.TrustedLedgerNamespace(root, remote)
			if err != nil {
				return err
			}
			layout, err := coordination.LayoutFor(ns)
			if err != nil {
				return err
			}
			g := &fork.Gateway{Tracker: tr, Store: coordination.NewGitStoreWithLayout(root, remote, layout), PollInterval: o.forkPollInterval, PollTimeout: o.forkPollTimeout, OnProgress: forkProgressPrinter(progressWriter(cmd))}
			_, op, err := g.RequestStatus(cmd.Context(), issue, id)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Request %d (%s) on issue #%d confirmed by upstream state. Recovery does not resume work, restore a local claim token or repair the local checkpoint.\n", id, op, issue)
			return nil
		},
	}
}
