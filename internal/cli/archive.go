package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"strings"
)

func newArchiveCmd(opts *RootOptions) *cobra.Command {
	makeCommand := func(restore bool) *cobra.Command {
		var apply, asJSON, all bool
		var destination string
		use := "archive [<issue-number> | --all-eligible]"
		short := "Preview or relocate finished issue ledgers to a verified Git archive"
		if restore {
			use = "restore <issue-number>"
			short = "Preview or restore archived issue ledgers"
		}
		cmd := &cobra.Command{Use: use, Short: short, Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			var report *lease.ArchiveReport
			var bulk *lease.BulkArchiveReport
			if all {
				bulk = &lease.BulkArchiveReport{Kind: "archive", Status: "preview", Issues: []*lease.ArchiveReport{}}
			}
			defer func() {
				if asJSON {
					if bulk != nil {
						if runErr != nil {
							bulk.Status = "failed"
							bulk.Error = runErr.Error()
						}
						if e := writeJSON(cmd, bulk); runErr == nil {
							runErr = e
						}
						return
					}
					if report == nil {
						report = &lease.ArchiveReport{Kind: "archive", Status: "failed"}
					}
					if runErr != nil {
						report.Status = "failed"
						report.Error = runErr.Error()
					}
					if e := writeJSON(cmd, report); runErr == nil {
						runErr = e
					}
				}
			}()
			if (all && (restore || len(args) != 0)) || (!all && len(args) != 1) {
				return fmt.Errorf("specify exactly one positive issue number or --all-eligible")
			}
			n := 0
			var err error
			if !all {
				n, err = parseNumberArg(args[0], "issue")
				if err != nil {
					return err
				}
			}
			if opts.Fork {
				return fmt.Errorf("archive requires an upstream repository operator")
			}
			m, policy, err := resolveArchiveRun(cmd, opts, apply)
			if err != nil {
				return err
			}
			if restore {
				report, err = m.RestoreIssueArchive(cmd.Context(), n, apply)
			} else if all {
				bulk, err = m.ArchiveAllEligible(cmd.Context(), policy, destination, apply)
				if !asJSON && bulk != nil {
					for _, r := range bulk.Issues {
						fmt.Fprintf(cmd.OutOrStdout(), "#%d: %s eligible=%t tip=%s committer=%v cutoff=%v after_days=%d %s\n", r.Issue, r.Status, r.Eligible, r.StateTip, r.CommitterTime, r.Cutoff, r.AfterDays, r.Error)
						for _, c := range r.Checks {
							fmt.Fprintf(cmd.OutOrStdout(), "  %s: eligible=%t: %s\n", c.Name, c.Eligible, c.Reason)
						}
					}
					fmt.Fprintf(cmd.OutOrStdout(), "totals: %+v\n", bulk.Totals)
				}
			} else {
				report, err = m.ArchivePolicyIssue(cmd.Context(), n, policy, destination, apply)
			}
			if !asJSON && report != nil {
				for _, c := range report.Checks {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: eligible=%t: %s\n", c.Name, c.Eligible, c.Reason)
				}
				for _, r := range report.Refs {
					fmt.Fprintf(cmd.OutOrStdout(), "%s %s -> %s\n", r.SHA, r.Source, r.Destination)
				}
				if err == nil {
					fmt.Fprintln(cmd.OutOrStdout(), report.Status)
				}
			}
			return err
		}}
		cmd.Flags().BoolVar(&apply, "apply", false, "Apply the verified ledger transfer")
		cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured checks and exact refs")
		if !restore {
			cmd.Flags().BoolVar(&all, "all-eligible", false, "Preview or archive every eligible issue older than archive.after_days")
			cmd.Flags().StringVar(&destination, "to", "", "Git archive destination (HTTPS, SSH, or file://)")
		}
		return cmd
	}
	cmd := makeCommand(false)
	cmd.AddCommand(makeCommand(true))
	return cmd
}

// Archive previews must not use the ordinary manager resolver: it refreshes
// remote-tracking refs. Inspect the advertised trusted commit without fetching.
func resolveArchiveRun(cmd *cobra.Command, opts *RootOptions, apply bool) (*lease.Manager, *contract.ArchiveConfig, error) {
	tr, err := opts.resolveTracker(cmd)
	if err != nil {
		return nil, nil, err
	}
	if tr.BackendName() != "github" {
		return nil, nil, fmt.Errorf("archive is unavailable on the local backend")
	}
	root := opts.resolveRepoRoot()
	remote := opts.effectiveUpstreamRemote()
	raw, err := gitcredential.Output(cmd.Context(), root, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return nil, nil, err
	}
	var tip string
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "HEAD" {
			tip = f[0]
		}
	}
	if len(tip) != 40 {
		return nil, nil, fmt.Errorf("upstream default commit unavailable")
	}
	if apply {
		_, err = gitcredential.Output(cmd.Context(), root, "fetch", "--no-write-fetch-head", remote, tip)
		if err != nil {
			return nil, nil, err
		}
	}
	ns, err := harness.LedgerNamespaceAt(root, tip)
	if err != nil {
		return nil, nil, fmt.Errorf("trusted upstream config unavailable locally; fetch the default branch before preview: %w", err)
	}
	policy, err := harness.ArchivePolicyAt(root, tip)
	if err != nil {
		return nil, nil, err
	}
	layout, err := coordination.LayoutFor(ns)
	if err != nil {
		return nil, nil, err
	}
	m := lease.NewManager(tr, contract.SecurityConfig{})
	m.SetUpstreamRemote(remote)
	m.SetStore(root, coordination.NewGitStoreWithLayout(root, remote, layout))
	if opts.nowFn != nil {
		m.SetNowFunc(opts.nowFn)
	}
	return m, policy, nil
}
