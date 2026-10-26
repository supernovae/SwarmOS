package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/coordination"
)

func newMigrateRefsCmd(options *RootOptions) *cobra.Command {
	var dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "migrate-refs",
		Short: "Move SwarmOS ledger refs from refs/heads/swarm-* to hidden refs/swarm/*",
		Long: `Move every coordination state, checkpoint retention, and contributor index ref
from the branch namespace (refs/heads/swarm-state, swarm-checkpoints,
swarm-contributors) to the hidden namespace (refs/swarm/state, checkpoints,
contributors) on the upstream remote, in one atomic push. Task branches stay
branches.

Run it during a maintenance window with workers and fork coordinator dispatch
paused. It refuses while any issue has an active claim and is safe to rerun.
Afterwards, merge ledger_namespace: hidden into .swarm/config.yaml on the
default branch, then resume workers.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runMigrateRefs(cmd, options, dryRun, asJSON)
			reportJSONFailure(cmd, asJSON, "migrate-refs", err)
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "List the refs that would move without pushing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a structured migration report")
	return cmd
}

func runMigrateRefs(cmd *cobra.Command, options *RootOptions, dryRun, asJSON bool) error {
	if options.Fork {
		return fmt.Errorf("swarm repo migrate-refs is a maintainer operation on the upstream remote and cannot run with --fork")
	}
	if strings.EqualFold(strings.TrimSpace(options.Backend), "local") {
		return fmt.Errorf("swarm repo migrate-refs migrates Git ledger refs; the local backend has none")
	}
	store := coordination.NewGitStoreWithRemote(options.resolveRepoRoot(), options.effectiveUpstreamRemote())
	report, err := store.MigrateLedgerRefs(cmd.Context(), options.now(), dryRun)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(cmd, map[string]any{"kind": "migrate-refs", "status": "ok", "report": report})
	}
	out := cmd.OutOrStdout()
	verb := "MOVED"
	if dryRun {
		verb = "WOULD MOVE"
	}
	for _, move := range report.Moves {
		fmt.Fprintf(out, "%s %s -> %s (%s)\n", verb, move.From, move.To, move.SHA)
	}
	switch {
	case len(report.Moves) == 0:
		fmt.Fprintf(out, "Nothing to migrate; %d ledger ref(s) already hidden.\n", report.AlreadyHidden)
	case dryRun:
		fmt.Fprintf(out, "Dry run: %d ledger ref(s) would move; nothing was pushed.\n", len(report.Moves))
	default:
		fmt.Fprintf(out, "Moved %d ledger ref(s) in one atomic push.\n", len(report.Moves))
	}
	if len(report.Moves) > 0 && !dryRun {
		fmt.Fprintln(out, "Next: merge ledger_namespace: hidden into .swarm/config.yaml on the default branch, then resume workers.")
	}
	return nil
}
