package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

type issueEditPreview struct {
	Issue         int    `json:"issue_number"`
	OldTitle      string `json:"old_title"`
	Title         string `json:"title"`
	Diff          string `json:"diff"`
	Lint          string `json:"lint"`
	DigestChanged bool   `json:"digest_changed"`
	Approval      string `json:"approval"`
	Claim         string `json:"claim"`
	Next          string `json:"next_command,omitempty"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
}

func newIssueEditCmd(opts *RootOptions) *cobra.Command {
	var title, objective, workflowName, gate string
	var constraints, acceptance []string
	var depends []int
	var dryRun, asJSON bool
	cmd := &cobra.Command{Use: "edit <issue-number>", Short: "Preview and lint structured WorkUnit replacements before editing", Args: cobra.ExactArgs(1)}
	cmd.RunE = func(cmd *cobra.Command, args []string) (runErr error) {
		if asJSON {
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
		}
		previewed := false
		defer func() {
			if !previewed {
				reportJSONFailure(cmd, asJSON, "issue-edit", runErr)
			}
		}()
		if opts.Fork {
			return fmt.Errorf("issue edit requires upstream maintainer access; --fork is unsupported")
		}
		number, err := parseNumberArg(args[0], "issue")
		if err != nil {
			return err
		}
		tr, mgr, _, err := opts.resolveLeaseManager(cmd)
		if err != nil {
			return err
		}
		if err = tr.Authorize(cmd.Context(), "edit-task"); err != nil {
			return err
		}
		issue, err := tr.GetIssue(cmd.Context(), number)
		if err != nil {
			return err
		}
		if issue.IsEpic() {
			return fmt.Errorf("issue #%d is an Epic; edit requires a WorkUnit", number)
		}
		edit := contract.WorkUnitEdit{}
		if cmd.Flags().Changed("title") {
			edit.Title = &title
		}
		if cmd.Flags().Changed("objective") {
			edit.Objective = &objective
		}
		if cmd.Flags().Changed("workflow") {
			edit.Workflow = &workflowName
		}
		if cmd.Flags().Changed("gate") {
			edit.Gate = &gate
		}
		if cmd.Flags().Changed("constraint") {
			edit.Constraints = &constraints
		}
		if cmd.Flags().Changed("acceptance") {
			edit.Acceptance = &acceptance
		}
		if cmd.Flags().Changed("depends-on") {
			edit.DependsOn = &depends
		}
		st, err := readIssueEditState(cmd.Context(), mgr, number)
		if err != nil {
			return err
		}
		newTitle, body, lintErr := contract.EditWorkUnitIssue(number, issue.Title, issue.Body, edit)
		old, _ := contract.ParseWorkUnitFromIssue(number, issue.Title, issue.Body)
		proposed, _ := contract.ParseWorkUnitFromIssue(number, newTitle, body)
		p := issueEditPreview{Issue: number, OldTitle: issue.Title, Title: newTitle, Diff: issueEditDiff(issue.Body, body), Lint: "valid", Approval: "unapproved", Claim: "no active claim", Status: "preview"}
		p.DigestChanged = contract.DigestWorkUnit(old) != contract.DigestWorkUnit(proposed)
		if lintErr != nil {
			p.Lint = lintErr.Error()
			runErr = fmt.Errorf("lint refused edit: %w", lintErr)
		}
		readyLabel := "swarm:ready"
		if st != nil && st.ApprovedBy != "" {
			// Label policy, like approval, comes from the recorded trusted base.
			cfg, loadErr := harness.LoadConfigAt(opts.resolveRepoRoot(), st.BaseSHA)
			if loadErr != nil {
				return loadErr
			}
			if cfg.Security.ReadyLabel != "" {
				readyLabel = cfg.Security.ReadyLabel
			}
			p.Approval = "current approval retained"
			if contract.DigestWorkUnit(old) != st.TaskDigest {
				p.Approval = "approval already stale"
			}
			if p.DigestChanged {
				p.Approval = "revision requires separate approval; ready label becomes draft"
				p.Next = fmt.Sprintf("swarm issue approve %d", number)
			}
		} else if p.DigestChanged {
			for _, label := range issue.Labels {
				if label == readyLabel {
					p.Approval = "ready label has no recorded approval; label retained"
				}
			}
		}
		now := time.Now()
		if opts.nowFn != nil {
			now = opts.nowFn()
		}
		if claimErr := issueEditClaim(st, number, now); claimErr != nil {
			p.Claim = claimErr.Error()
			runErr = claimErr
		}
		emit := func() error {
			previewed = true
			if runErr != nil {
				p.Error = runErr.Error()
				if p.Status == "preview" {
					p.Status = "refused"
				}
			}
			if asJSON {
				return writeJSON(cmd, p)
			}
			out := cmd.OutOrStdout()
			if p.OldTitle != p.Title {
				fmt.Fprintf(out, "Title: %q -> %q\n", p.OldTitle, p.Title)
			}
			fmt.Fprint(out, p.Diff)
			fmt.Fprintf(out, "Lint: %s\nApproval: %s\nClaim: %s\nStatus: %s\n", p.Lint, p.Approval, p.Claim, p.Status)
			if p.Next != "" {
				fmt.Fprintln(out, "Next:", p.Next)
			}
			return nil
		}
		if dryRun || runErr != nil {
			if err := emit(); err != nil {
				return err
			}
			return runErr
		}
		if body == issue.Body && newTitle == issue.Title {
			p.Status = "unchanged"
			return emit()
		}
		// Re-read immediately before mutation. GitHub PATCH is unconditional, so
		// this detects intervening edits but cannot close the GET-to-PATCH window.
		latestState, err := readIssueEditState(cmd.Context(), mgr, number)
		if err != nil {
			return err
		}
		now = time.Now()
		if opts.nowFn != nil {
			now = opts.nowFn()
		}
		if err = issueEditClaim(latestState, number, now); err != nil {
			return err
		}
		latest, err := tr.GetIssue(cmd.Context(), number)
		if err != nil {
			return err
		}
		if latest.Body != issue.Body || latest.Title != issue.Title || latest.IsEpic() {
			return fmt.Errorf("issue #%d changed concurrently; rerun issue edit to review the new version", number)
		}
		if err = writeIssueEdit(cmd.Context(), tr, issue, newTitle, body, p.DigestChanged && st != nil && st.ApprovedBy != "", readyLabel); err != nil {
			runErr = err
			p.Status = "failed"
			_ = emit()
			return err
		}
		p.Status = "edited"
		return emit()
	}
	f := cmd.Flags()
	f.StringVar(&title, "title", "", "Replace the issue title")
	f.StringVar(&objective, "objective", "", "Replace the objective")
	f.StringArrayVar(&constraints, "constraint", nil, "Replace constraints (repeat for each bullet)")
	f.StringArrayVar(&acceptance, "acceptance", nil, "Replace acceptance criteria (repeat for each criterion)")
	f.IntSliceVar(&depends, "depends-on", nil, "Replace prerequisite issue numbers; empty clears the list")
	f.Lookup("depends-on").Value = &emptyIntSliceValue{f.Lookup("depends-on").Value}
	f.StringVar(&workflowName, "workflow", "", "Replace workflow (feature, bugfix, refactor)")
	f.StringVar(&gate, "gate", "", "Replace plan gate (none, agent, human)")
	f.BoolVar(&dryRun, "dry-run", false, "Preview changes without writing")
	f.BoolVar(&asJSON, "json", false, "Emit structured edit results")
	return cmd
}

func readIssueEditState(ctx context.Context, mgr *lease.Manager, n int) (*lease.IssueState, error) {
	st, err := mgr.GetCurrentState(ctx, n)
	if errors.Is(err, coordination.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read authoritative issue state: %w", err)
	}
	return st, nil
}
func issueEditClaim(st *lease.IssueState, n int, now time.Time) error {
	if st != nil && st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now) {
		return fmt.Errorf("issue #%d claimed by %q until %s; use swarm issue release %d", n, st.Lease.Owner, st.Lease.ExpiresAt.Format(time.RFC3339), n)
	}
	return nil
}
func writeIssueEdit(ctx context.Context, tr tracker.Tracker, old *tracker.Issue, title, body string, demote bool, ready string) error {
	var completed []string
	failure := func(step string, err error) error {
		progress := strings.Join(completed, "; ")
		if progress == "" {
			progress = "no changes confirmed"
		}
		return fmt.Errorf("%s; failed to %s: %w; inspect issue #%d and rerun edit, then swarm issue approve %d", progress, step, err, old.Number, old.Number)
	}
	if body != old.Body {
		if _, err := tr.UpdateIssueBody(ctx, old.Number, body); err != nil {
			return failure("update body", err)
		}
		completed = append(completed, "body updated")
	}
	if title != old.Title {
		if _, err := tr.UpdateIssueTitle(ctx, old.Number, title); err != nil {
			return failure("update title", err)
		}
		completed = append(completed, "title updated")
	}
	if demote {
		if err := tr.RemoveLabel(ctx, old.Number, ready); err != nil {
			return failure("remove ready label", err)
		}
		completed = append(completed, "ready label removed")
		if ready != "swarm:ready" {
			if err := tr.RemoveLabel(ctx, old.Number, "swarm:ready"); err != nil {
				return failure("remove swarm:ready", err)
			}
		}
		if err := tr.AddLabels(ctx, old.Number, []string{"swarm:draft"}); err != nil {
			return failure("add draft label", err)
		}
	}
	return nil
}

// pflag's integer slice rejects an empty token; editing needs it to clear the list.
type emptyIntSliceValue struct{ pflag.Value }

func (v *emptyIntSliceValue) Set(s string) error {
	if s == "" {
		return v.Value.(pflag.SliceValue).Replace(nil)
	}
	return v.Value.Set(s)
}
