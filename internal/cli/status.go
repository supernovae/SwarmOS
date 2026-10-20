package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

func newStatusCmd(rootOpts *RootOptions) *cobra.Command {
	var allRepos bool

	cmd := &cobra.Command{
		Use:   "status [<issue-number>]",
		Short: "Display active claims, remaining lease TTL, workflow phase, plan gate status, and artifacts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			if len(args) == 1 {
				num, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
				if err != nil || num <= 0 {
					return fmt.Errorf("invalid issue number %q", args[0])
				}
				tr, mgr, _, err := rootOpts.resolveLeaseManager(cmd)
				if err != nil {
					return err
				}
				return printSingleIssueStatus(cmd, rootOpts.resolveRepoRoot(), tr, mgr, num)
			}

			if allRepos {
				targets, err := rootOpts.resolveTargets(true)
				if err != nil {
					return err
				}
				fmt.Fprintln(out, "REPOSITORY                   ISSUE   OWNER          AGENT      PHASE       STATUS      TTL         TITLE")
				fmt.Fprintln(out, "----------                   -----   -----          -----      -----       ------      ---         -----")
				now := time.Now().UTC()
				total := 0
				for _, tgt := range targets {
					tr, mgr, _, err := rootOpts.resolveTargetLeaseManager(cmd, tgt)
					if err != nil {
						continue
					}
					issues, err := tr.ListIssues(ctx, tracker.IssueFilter{State: "open"})
					if err != nil {
						continue
					}
					for _, iss := range issues {
						total++
						st, err := mgr.GetCurrentState(ctx, iss.Number)
						if err != nil || st == nil {
							fmt.Fprintf(out, "%-28s #%-6d %-14s %-10s %-11s %-11s %-11s %s\n",
								tgt.Repo, iss.Number, "-", "-", "-", "unclaimed", "-", iss.Title)
							continue
						}
						ttl := "expired"
						if st.Lease.ExpiresAt.After(now) && st.Lease.Status == "active" {
							ttl = st.Lease.ExpiresAt.Sub(now).Round(time.Second).String()
						} else if st.Lease.Status != "active" {
							ttl = "-"
						}
						fmt.Fprintf(out, "%-28s #%-6d %-14s %-10s %-11s %-11s %-11s %s\n",
							tgt.Repo, iss.Number, st.Lease.Owner, st.Lease.Agent, st.Lease.Phase, st.Lease.Status, ttl, iss.Title)
					}
				}
				if total == 0 {
					fmt.Fprintln(out, "No open issues found across registered repositories.")
				}
				return nil
			}

			tr, mgr, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			issues, err := tr.ListIssues(ctx, tracker.IssueFilter{State: "open"})
			if err != nil {
				return err
			}
			if len(issues) == 0 {
				fmt.Fprintln(out, "No open issues found.")
				return nil
			}

			fmt.Fprintln(out, "ISSUE   OWNER          AGENT      PHASE       STATUS      TTL         TITLE")
			fmt.Fprintln(out, "-----   -----          -----      -----       ------      ---         -----")
			now := time.Now().UTC()
			for _, iss := range issues {
				st, err := mgr.GetCurrentState(ctx, iss.Number)
				if err != nil || st == nil {
					fmt.Fprintf(out, "#%-6d %-14s %-10s %-11s %-11s %-11s %s\n",
						iss.Number, "-", "-", "-", "unclaimed", "-", iss.Title)
					continue
				}
				ttl := "expired"
				if st.Lease.ExpiresAt.After(now) && st.Lease.Status == "active" {
					ttl = st.Lease.ExpiresAt.Sub(now).Round(time.Second).String()
				} else if st.Lease.Status != "active" {
					ttl = "-"
				}
				fmt.Fprintf(out, "#%-6d %-14s %-10s %-11s %-11s %-11s %s\n",
					iss.Number, st.Lease.Owner, st.Lease.Agent, st.Lease.Phase, st.Lease.Status, ttl, iss.Title)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&allRepos, "all", false, "Show status across all registered repositories")
	return cmd
}

func printSingleIssueStatus(cmd *cobra.Command, repoRoot string, tr tracker.Tracker, mgr *lease.Manager, issueNum int) error {
	ctx := cmd.Context()
	iss, err := tr.GetIssue(ctx, issueNum)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Issue #%d: %s\n", iss.Number, iss.Title)
	fmt.Fprintf(out, "Labels:    %s\n", strings.Join(iss.Labels, ", "))

	if wu, err := contract.ParseWorkUnitFromIssueWithRoot(iss.Number, iss.Title, iss.Body, repoRoot); err == nil {
		fmt.Fprintf(out, "Workflow:  %s (gate=%s)\n", wu.Workflow, wu.Gate)
	}

	st, err := mgr.GetCurrentState(ctx, issueNum)
	if err != nil || st == nil {
		fmt.Fprintln(out, "Lease:     unclaimed")
		return nil
	}

	now := time.Now().UTC()
	ttl := "expired"
	if st.Lease.ExpiresAt.After(now) && st.Lease.Status == "active" {
		ttl = st.Lease.ExpiresAt.Sub(now).Round(time.Second).String()
	} else if st.Lease.Status != "active" {
		ttl = "-"
	}
	fmt.Fprintf(out, "Owner:     %s (agent=%s)\n", st.Lease.Owner, st.Lease.Agent)
	fmt.Fprintf(out, "Phase:     %s\n", st.Lease.Phase)
	fmt.Fprintf(out, "Status:    %s (TTL: %s, renewals: %d/%d)\n", st.Lease.Status, ttl, st.Lease.Renewals, st.Lease.MaxRenewals)

	if len(st.Artifacts) > 0 {
		fmt.Fprintln(out, "\nArtifacts:")
		for _, a := range st.Artifacts {
			verdict := a.Verdict
			if verdict == "" {
				verdict = "-"
			}
			fmt.Fprintf(out, "  - %-14s v%d by %-16s [verdict=%s] %s\n", a.Type, a.Version, a.Producer, verdict, a.Summary)
		}
	}
	return nil
}

func newRenewCmd(rootOpts *RootOptions) *cobra.Command {
	var durationStr string
	cmd := &cobra.Command{
		Use:   "renew <issue-number>",
		Short: "Extend active lease TTL for an issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			issueNum, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
			if err != nil || issueNum <= 0 {
				return fmt.Errorf("invalid issue number %q", args[0])
			}
			tr, mgr, cfg, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			dur := 4 * time.Hour
			if durationStr != "" {
				parsed, err := time.ParseDuration(durationStr)
				if err != nil {
					return err
				}
				dur = parsed
			} else if cfg.Lease.Duration != "" {
				if parsed, err := time.ParseDuration(cfg.Lease.Duration); err == nil {
					dur = parsed
				}
			}
			owner, _ := tr.CurrentUser(cmd.Context())
			st, err := mgr.Renew(cmd.Context(), issueNum, owner, dur)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Renewed lease on issue #%d for %s (renewal %d/%d, expires %s)\n",
				issueNum, st.Lease.Owner, st.Lease.Renewals, st.Lease.MaxRenewals, st.Lease.ExpiresAt.Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().StringVar(&durationStr, "duration", "", "Lease renewal duration (default 4h)")
	return cmd
}

func newReleaseCmd(rootOpts *RootOptions) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "release <issue-number>",
		Short: "Release active claim on an issue back to the backlog",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			issueNum, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
			if err != nil || issueNum <= 0 {
				return fmt.Errorf("invalid issue number %q", args[0])
			}
			tr, mgr, _, err := rootOpts.resolveLeaseManager(cmd)
			if err != nil {
				return err
			}
			owner, _ := tr.CurrentUser(cmd.Context())
			st, err := mgr.Release(cmd.Context(), issueNum, owner, reason)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Released lease on issue #%d (status=%s)\n", issueNum, st.Lease.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for releasing the lease")
	return cmd
}
