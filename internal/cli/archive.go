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
		var apply, asJSON bool
		var destination string
		use := "archive <issue-number>"
		short := "Preview or relocate finished issue ledgers to a verified Git archive"
		if restore {
			use = "restore <issue-number>"
			short = "Preview or restore archived issue ledgers"
		}
		cmd := &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			var report *lease.ArchiveReport
			defer func() {
				if asJSON {
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
			n, err := parseNumberArg(args[0], "issue")
			if err != nil {
				return err
			}
			if opts.Fork {
				return fmt.Errorf("archive requires an upstream repository operator")
			}
			m, err := resolveArchiveManager(cmd, opts, apply)
			if err != nil {
				return err
			}
			if restore {
				report, err = m.RestoreIssueArchive(cmd.Context(), n, apply)
			} else {
				report, err = m.ArchiveIssue(cmd.Context(), n, destination, apply)
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
func resolveArchiveManager(cmd *cobra.Command, opts *RootOptions, apply bool) (*lease.Manager, error) {
	if apply {
		_, m, _, err := opts.resolveLeaseManager(cmd)
		return m, err
	}
	tr, err := opts.resolveTracker(cmd)
	if err != nil {
		return nil, err
	}
	if tr.BackendName() != "github" {
		return nil, fmt.Errorf("archive is unavailable on the local backend")
	}
	root := opts.resolveRepoRoot()
	remote := opts.effectiveUpstreamRemote()
	raw, err := gitcredential.Output(cmd.Context(), root, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return nil, err
	}
	var tip string
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "HEAD" {
			tip = f[0]
		}
	}
	if len(tip) != 40 {
		return nil, fmt.Errorf("upstream default commit unavailable")
	}
	ns, err := harness.LedgerNamespaceAt(root, tip)
	if err != nil {
		return nil, fmt.Errorf("trusted upstream config unavailable locally; fetch the default branch before preview: %w", err)
	}
	layout, err := coordination.LayoutFor(ns)
	if err != nil {
		return nil, err
	}
	m := lease.NewManager(tr, contract.SecurityConfig{})
	m.SetUpstreamRemote(remote)
	m.SetStore(root, coordination.NewGitStoreWithLayout(root, remote, layout))
	if opts.nowFn != nil {
		m.SetNowFunc(opts.nowFn)
	}
	return m, nil
}
