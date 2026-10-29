package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

func newReconcileCmd(opts *RootOptions) *cobra.Command {
	var asJSON, checkpoint, dryRun bool
	var expected, candidate string
	var reconciliation *lease.CheckpointReconciliation
	cmd := &cobra.Command{Use: "reconcile <issue-number>", Short: "Repair PR outcomes or explicitly reconcile a task checkpoint", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		var state *lease.IssueState
		defer func() {
			if !asJSON {
				return
			}
			data := map[string]any{"kind": "reconcile", "status": "completed", "state": state}
			if reconciliation != nil {
				data["checkpoint_reconciliation"] = reconciliation
			}
			if runErr != nil {
				data["status"] = "failed"
				data["error"] = runErr.Error()
			}
			if e := writeJSON(cmd, data); runErr == nil {
				runErr = e
			}
		}()

		if !checkpoint && (expected != "" || candidate != "" || dryRun) {
			return fmt.Errorf("--expected-head, --candidate-head and --dry-run require --checkpoint")
		}
		if checkpoint && (expected == "" || candidate == "") {
			return fmt.Errorf("--checkpoint requires --expected-head and --candidate-head full SHAs")
		}
		n, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return err
		}
		if opts.Fork {
			return fmt.Errorf("reconciliation requires an upstream repository operator; use the fork PR revision workflow")
		}
		var tr tracker.Tracker
		var m *lease.Manager
		if checkpoint {
			tr, m, err = opts.resolveCheckpointManager(cmd)
		} else {
			tr, m, _, err = opts.resolveLeaseManager(cmd)
		}
		if err != nil {
			return err
		}
		if checkpoint {
			root := opts.resolveRepoRoot()
			if !dryRun {
				lock, e := lease.LockClaim(root, n)
				if e != nil {
					return e
				}
				defer lease.UnlockClaim(lock)
			}
			owner, e := tr.CurrentUser(cmd.Context())
			if e != nil {
				return e
			}
			token, e := lease.LoadClaimToken(root, n)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
			m.BindClaim(n, token)
			reconciliation, err = m.PrepareCheckpoint(cmd.Context(), n, owner, expected, candidate, !dryRun)
			if err != nil {
				return err
			}
			if !reconciliation.ClaimRequired {
				hint, e := lease.LoadClaimBaseHint(root, n)
				if e != nil {
					return e
				}
				if hint != reconciliation.State.BaseSHA {
					return fmt.Errorf("%w: persisted claim base does not match approved checkpoint base", lease.ErrClaimFence)
				}
			}
			if !dryRun {
				if reconciliation.ClaimRequired {
					acquired, e := m.AcquirePreparedCheckpoint(cmd.Context(), n, reconciliation)
					if e != nil {
						return e
					}
					if e = lease.SaveClaimToken(root, n, acquired.Lease.ClaimID, acquired.BaseSHA); e != nil {
						_, releaseErr := m.Release(cmd.Context(), n, owner, "checkpoint token setup failed")
						return errors.Join(e, releaseErr)
					}
				}
				reconciliation, err = m.ReconcilePreparedCheckpoint(cmd.Context(), n, reconciliation)
				if err != nil {
					return fmt.Errorf("%w; checkpoint work preserved; the authorized claim remains held unless its fence changed; retry reconciliation or use swarm issue release %d", err, n)
				}
			}

			if reconciliation != nil {
				state = reconciliation.State
			}
			if err != nil {
				return err
			}
			if !asJSON {
				if !dryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d checkpoint reconciled: %s -> %s\n", n, expected, candidate)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\nDry run: %t; validation required: %t; fresh plan review required: %t; new claim required: %t; boundary probe required: %t\n", reconciliation.Observation.String(), dryRun, reconciliation.ValidationRequired, reconciliation.ReviewRequired, reconciliation.ClaimRequired, reconciliation.BoundaryProbeRequired)
			}
			return nil
		}
		state, err = m.ReconcileSubmission(cmd.Context(), n)
		if err != nil {
			return err
		}
		if !asJSON {
			fmt.Fprintf(cmd.OutOrStdout(), "Issue #%d PR #%d: %s\n", n, state.PRNumber, state.PROutcome.Status)
		}
		return nil
	}}
	cmd.Flags().BoolVar(&checkpoint, "checkpoint", false, "Explicitly reconcile a local task checkpoint with authoritative state")
	cmd.Flags().StringVar(&expected, "expected-head", "", "Full authoritative predecessor checkpoint SHA")
	cmd.Flags().StringVar(&candidate, "candidate-head", "", "Full clean local task candidate SHA")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Inspect checkpoint preconditions without executing checks or publishing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured reconciliation results")
	return cmd
}

// Checkpoint admission must resolve the upstream-selected ledger without the
// fetch and remote-ref updates used by ordinary worker setup.
func (o *RootOptions) resolveCheckpointManager(cmd *cobra.Command) (tracker.Tracker, *lease.Manager, error) {
	tr, err := o.resolveTracker(cmd)
	if err != nil {
		return nil, nil, err
	}
	root, err := filepath.Abs(o.resolveRepoRoot())
	if err != nil {
		return nil, nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, err
	}
	var store coordination.Store = coordination.NewFileStore(root)
	if strings.HasPrefix(tr.BackendName(), "github") {
		if err := workspace.ValidateRemoteName(o.effectiveUpstreamRemote()); err != nil {
			return nil, nil, err
		}
		raw, err := gitcredential.Output(cmd.Context(), root, "ls-remote", "--", o.effectiveUpstreamRemote(), "HEAD")
		if err != nil {
			return nil, nil, err
		}
		fields := strings.Fields(string(raw))
		if len(fields) != 2 || fields[1] != "HEAD" {
			return nil, nil, fmt.Errorf("upstream advertised default head unavailable")
		}
		namespace, err := harness.LedgerNamespaceAt(root, fields[0])
		if err != nil {
			return nil, nil, fmt.Errorf("upstream default policy unavailable locally; fetch upstream before checkpoint reconciliation: %w", err)
		}
		layout, err := coordination.LayoutFor(namespace)
		if err != nil {
			return nil, nil, err
		}
		store = coordination.NewGitStoreWithLayout(root, o.effectiveUpstreamRemote(), layout)
	}
	m := lease.NewManager(tr, contract.SecurityConfig{})
	if o.nowFn != nil {
		m.SetNowFunc(o.nowFn)
	}
	m.SetUpstreamRemote(o.effectiveUpstreamRemote())
	m.SetStore(root, store)
	return tr, m, nil
}
