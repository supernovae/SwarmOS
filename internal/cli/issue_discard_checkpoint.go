package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/lease"
)

func newDiscardCheckpointCmd(rootOpts *RootOptions) *cobra.Command {
	var expectedHead string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "discard-checkpoint <issue-number>",
		Short: "Abandon an unsubmitted checkpoint so the next claim starts from the approved base (maintainer only)",
		Long: "Records the issue's unsubmitted checkpoint as discarded and keeps its commit permanently reachable\n" +
			"under a retention ref in the repository's ledger namespace: refs/heads/swarm-checkpoints/... in\n" +
			"branch mode, refs/swarm/checkpoints/... in hidden mode. The next claim starts on a new branch at\n" +
			"the approved base. Refused while the issue is claimed, has a pull request, or holds a fork\n" +
			"contributor's work.\n" +
			"Only a maintainer may run it: it needs the same authority as swarm issue approve.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "checkpoint-discard", runErr) }()
			number, err := parseNumberArg(args[0], "issue")
			if err != nil {
				return err
			}
			if strings.TrimSpace(expectedHead) == "" {
				return fmt.Errorf("--expected-head is required; copy the checkpoint SHA from swarm issue status %d", number)
			}
			tr, mgr, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			owner, err := tr.CurrentUser(cmd.Context())
			if err != nil || strings.TrimSpace(owner) == "" {
				return fmt.Errorf("determine discarding actor: %w", err)
			}
			st, err := mgr.DiscardCheckpoint(cmd.Context(), number, owner, expectedHead)
			if err != nil {
				var claimed *lease.AlreadyClaimedError
				if errors.As(err, &claimed) {
					return fmt.Errorf("%w; the claim must be released (swarm issue release %d) or expire first", err, number)
				}
				return err
			}
			discarded := st.DiscardedCheckpoints[len(st.DiscardedCheckpoints)-1]
			if asJSON {
				return writeJSON(cmd, map[string]any{"kind": "checkpoint-discard", "issue_number": number, "discarded": discarded, "state": st})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Discarded checkpoint %s of issue #%d (branch %s)\n", discarded.HeadSHA, number, discarded.Branch)
			fmt.Fprintf(out, "Retained at %s\n", discarded.RetentionRef)
			fmt.Fprintf(out, "The next claim starts on %s at approved base %s and plans afresh.\n", st.Branch, st.BaseSHA)
			return nil
		},
	}
	cmd.Flags().StringVar(&expectedHead, "expected-head", "", "Full SHA of the recorded checkpoint being discarded (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured discard results")
	return cmd
}

func newArchiveCompletionCmd(rootOpts *RootOptions) *cobra.Command {
	var expectedReceipt string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "archive-completion <issue-number>",
		Short: "Archive a stale completion receipt in the original coordinator checkout (maintainer only)",
		Long: "Preserve a pending completion receipt and pin its candidate locally in the original coordinator checkout.\n" +
			"The original claim must be released, revoked, or superseded. An expired claim needs a successor.\n" +
			"A maintainer with issue approval authority must run this command.\n" +
			"Copy the full --expected-receipt selector from the completion recovery diagnostic.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() { reportJSONFailure(cmd, asJSON, "completion-archive", runErr) }()
			number, err := parseNumberArg(args[0], "issue")
			if err != nil {
				return err
			}
			if rootOpts.Fork {
				return fmt.Errorf("completion archival requires the original direct coordinator checkout")
			}
			if expectedReceipt == "" {
				return fmt.Errorf("--expected-receipt is required")
			}
			_, mgr, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			result, err := mgr.ArchiveCheckpointCompletion(cmd.Context(), number, expectedReceipt)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{
					"kind": "completion-archive", "issue_number": number,
					"candidate": result.Candidate, "retention_ref": result.RetentionRef, "already_recorded": result.AlreadyRecorded,
				})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Archived completion for issue #%d. Candidate %s retained locally at %s.\n", number, result.Candidate, result.RetentionRef)
			fmt.Fprintf(out, "Inspect current state with swarm issue status %d.\n", number)
			if result.AlreadyRecorded {
				fmt.Fprintln(out, "The candidate was already recorded. Follow the current claim's normal plan and validation steps.")
			} else {
				fmt.Fprintf(out, "If the preserved worktree and current predecessor are eligible, preview recovery with swarm issue reconcile %d --checkpoint --expected-head <current-predecessor> --candidate-head %s --dry-run.\n", number, result.Candidate)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&expectedReceipt, "expected-receipt", "", "Full SHA-256 selector from the completion diagnostic (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured archive results")
	return cmd
}
