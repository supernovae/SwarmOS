package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"io"
	"strings"
	"time"
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
			} else {
				report, err = m.ArchivePolicyIssue(cmd.Context(), n, policy, destination, apply)
			}
			if !asJSON {
				next := "swarm repo archive"
				if restore {
					next += " restore"
				}
				if all {
					next += " --all-eligible"
				} else {
					next += fmt.Sprintf(" %d", n)
				}
				if destination != "" {
					next += " --to " + shellArg(destination)
				}
				next += outputSelectors(opts)
				renderArchiveOutput(cmd.OutOrStdout(), report, bulk, apply, next, "swarm issue status"+outputSelectors(opts))
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
	ns, err := harness.LedgerNamespaceAtContext(cmd.Context(), root, tip)
	if err != nil {
		return nil, nil, fmt.Errorf("trusted upstream config unavailable locally; fetch the default branch before preview: %w", err)
	}
	policy, err := harness.ArchivePolicyAtContext(cmd.Context(), root, tip)
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

func archiveDate(t *time.Time) string {
	if t == nil {
		return "unavailable"
	}
	return t.Format("2006-01-02")
}
func renderArchiveOutput(w io.Writer, report *lease.ArchiveReport, bulk *lease.BulkArchiveReport, apply bool, next, status string) {
	reports := []*lease.ArchiveReport{}
	if bulk != nil {
		reports = bulk.Issues
	} else if report != nil {
		reports = append(reports, report)
	} else {
		return
	}
	ready := false
	var kept reasonTally
	for _, r := range reports {
		fmt.Fprintf(w, "#%d: %s\n", r.Issue, r.Status)
		if r.StateTip != "" {
			fmt.Fprintln(w, "  State tip: "+r.StateTip)
		}
		fmt.Fprintf(w, "  Committed: %s; cutoff: %s\n", archiveDate(r.CommitterTime), archiveDate(r.Cutoff))
		if r.Error != "" {
			fmt.Fprintln(w, "  Failed: "+r.Error)
		}
		if r.Eligible && r.Error == "" {
			ready = true
		}
		for _, c := range r.Checks {
			if !c.Eligible {
				kept.add(c.Reason)
			}
		}
		for _, ref := range r.Refs {
			fmt.Fprintf(w, "  %s %s -> %s\n", ref.SHA, ref.Source, ref.Destination)
		}
	}
	kept.render(w, "  ")
	if bulk != nil {
		t := bulk.Totals
		fmt.Fprintf(w, "Totals: %d candidates, %d eligible, %d previewed, %d archived, %d kept, %d failed\n", t.Candidates, t.Eligible, t.Previewed, t.Archived, t.Skipped, t.Failed)
		if bulk.Error != "" {
			fmt.Fprintln(w, "Failed: "+bulk.Error)
		}
	}
	if !apply {
		if ready {
			next += " --apply"
		} else {
			next = status
		}
	}
	fmt.Fprintln(w, "Next: "+next)
}
