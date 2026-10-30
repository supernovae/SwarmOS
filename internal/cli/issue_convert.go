package cli

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

type issueConvertPreview struct {
	Issue        int               `json:"issue_number"`
	Title        string            `json:"title"`
	OldTitle     string            `json:"old_title"`
	Diff         string            `json:"diff"`
	Lint         string            `json:"lint"`
	Gate         contract.GateMode `json:"gate"`
	Approval     string            `json:"approval"`
	Status       string            `json:"status"`
	Dependencies []int             `json:"blocked_by"`
	Completed    []string          `json:"completed_steps"`
	Warnings     []string          `json:"warnings"`
	Next         string            `json:"next_commands"`
	Error        string            `json:"error,omitempty"`
}

// Resolve policy exactly as approval's no-state path, never from the candidate.
func issueConvertPolicy(root, remote string) (string, *contract.SwarmConfig, error) {
	base, err := workspace.ResolveBaseFrom(root, remote)
	if err != nil {
		return "", nil, err
	}
	cfg, err := harness.LoadConfigAt(root, base)
	if err != nil {
		return "", nil, err
	}
	if cfg.BaseBranch != "" {
		base, err = workspace.ResolveBranchFrom(root, remote, cfg.BaseBranch)
		if err != nil {
			return "", nil, err
		}
		cfg, err = harness.LoadConfigAt(root, base)
	}
	return base, cfg, err
}

func issueConvertTarget(i *tracker.Issue) error {
	if i == nil {
		return fmt.Errorf("issue not found")
	}
	if i.IsPullRequest {
		return fmt.Errorf("#%d is a pull request; use swarm pr status %d and the PR workflow", i.Number, i.Number)
	}
	if i.State != "open" {
		return fmt.Errorf("issue #%d is not open", i.Number)
	}
	if i.IsEpic() {
		return fmt.Errorf("issue #%d is an Epic; conversion requires an intake issue", i.Number)
	}
	return nil
}

func issueConvertState(st *lease.IssueState, n int, now time.Time) error {
	if st == nil {
		return nil
	}
	if st.PRNumber != 0 || st.PRRevision != nil {
		return fmt.Errorf("issue #%d has linked PR/revision state; inspect swarm issue status %d and swarm pr status %d; continue through the PR workflow", n, n, st.PRNumber)
	}
	if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now) {
		return fmt.Errorf("issue #%d is claimed by %q; inspect swarm issue status %d; the owner must release the claim or wait for expiry before maintainer recovery", n, st.Lease.Owner, n)
	}
	return fmt.Errorf("issue #%d has authoritative state; inspect swarm issue status %d; use swarm issue edit %d for a valid contract, otherwise have a maintainer restore the contract in place from its recorded approved version, then approve the repaired revision separately", n, n, n)
}

func newIssueConvertCmd(opts *RootOptions) *cobra.Command {
	var wu contract.WorkUnit
	var gate string
	var dryRun, asJSON bool
	cmd := &cobra.Command{Use: "convert <issue-number>", Short: "Convert one intake issue in place into a draft WorkUnit", Args: cobra.ExactArgs(1)}
	cmd.RunE = func(cmd *cobra.Command, args []string) (runErr error) {
		if asJSON {
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
		}
		emitted := false
		defer func() {
			if !emitted {
				reportJSONFailure(cmd, asJSON, "issue-convert", runErr)
			}
		}()
		if opts.Fork {
			return fmt.Errorf("issue convert requires upstream maintainer access; --fork is unsupported")
		}
		if cmd.Flags().Changed("title") && strings.TrimSpace(wu.Title) == "" {
			return fmt.Errorf("--title must not be empty")
		}
		n, err := parseNumberArg(args[0], "issue")
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
		issue, err := tr.GetIssue(cmd.Context(), n)
		if err != nil {
			return err
		}
		if err = issueConvertTarget(issue); err != nil {
			return err
		}
		now := time.Now()
		if opts.nowFn != nil {
			now = opts.nowFn()
		}
		st, err := readIssueEditState(cmd.Context(), mgr, n)
		if err != nil {
			return err
		}
		if err = issueConvertState(st, n, now); err != nil {
			return err
		}
		base, cfg, err := issueConvertPolicy(opts.resolveRepoRoot(), mgr.UpstreamRemote())
		if err != nil {
			return fmt.Errorf("resolve trusted conversion policy: %w", err)
		}
		wu.Gate = contract.GateMode(gate)
		title, body, lintErr := contract.ConvertWorkUnitIssue(n, issue.Title, issue.Body, wu)
		p := issueConvertPreview{Issue: n, OldTitle: issue.Title, Title: title, Diff: issueEditDiff(issue.Body, body), Lint: "valid", Gate: wu.Gate, Approval: "draft; separate approval required; workflow or policy may require a stricter gate", Status: "preview", Next: fmt.Sprintf("swarm issue lint %d; swarm issue approve %d", n, n)}
		seen := map[int]bool{}
		for _, dep := range wu.DependsOn {
			if dep != n && !seen[dep] {
				p.Dependencies = append(p.Dependencies, dep)
				seen[dep] = true
			}
		}
		emit := func() error {
			emitted = true
			if runErr != nil {
				p.Error = runErr.Error()
				p.Status = "failed"
			}
			if asJSON {
				return writeJSON(cmd, p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Title: %q -> %q\n%sLint: %s\nGate: %s\nApproval: %s\nBlocked-by projections: %v\nStatus: %s\nCompleted: %v\nNext: %s\n", p.OldTitle, p.Title, p.Diff, p.Lint, p.Gate, p.Approval, p.Dependencies, p.Status, p.Completed, p.Next)
			for _, w := range p.Warnings {
				fmt.Fprintln(cmd.OutOrStdout(), "warning:", w)
			}
			if p.Error != "" {
				fmt.Fprintln(cmd.OutOrStdout(), p.Error)
			}
			return nil
		}
		if lintErr != nil {
			p.Lint = lintErr.Error()
			runErr = fmt.Errorf("lint refused conversion: %w", lintErr)
		}
		if dryRun || runErr != nil {
			if err := emit(); err != nil {
				return err
			}
			return runErr
		}
		// GitHub PATCH is unconditional. These rereads detect observed changes but
		// cannot make the cross-system GET-to-PATCH interval atomic.
		latest, err := tr.GetIssue(cmd.Context(), n)
		if err != nil {
			return err
		}
		if err = issueConvertTarget(latest); err != nil {
			return err
		}
		if !reflect.DeepEqual(issue, latest) {
			return fmt.Errorf("issue #%d changed concurrently; rerun conversion preview", n)
		}
		latestBase, latestCfg, err := issueConvertPolicy(opts.resolveRepoRoot(), mgr.UpstreamRemote())
		if err != nil {
			return err
		}
		if base != latestBase || !reflect.DeepEqual(cfg, latestCfg) {
			return fmt.Errorf("trusted policy changed concurrently; rerun conversion preview")
		}
		st, err = readIssueEditState(cmd.Context(), mgr, n)
		if err != nil {
			return err
		}
		if err = issueConvertState(st, n, now); err != nil {
			return err
		}
		persisted := false
		fail := func(step string, e error) error {
			retry := "rerun issue convert with the reviewed fields"
			if persisted {
				retry = "use swarm issue edit for further contract changes"
			}
			runErr = fmt.Errorf("failed to %s: %w; completed: %v; inspect swarm issue status %d and the issue, then %s; approval remains separate", step, e, p.Completed, n, retry)
			_ = emit()
			return runErr
		}
		ready := cfg.Security.ReadyLabel
		if ready == "" {
			ready = "swarm:ready"
		}
		removed := map[string]bool{}
		for _, label := range []string{ready, "swarm:ready", "swarm:needs-scope"} {
			if removed[label] {
				continue
			}
			removed[label] = true
			// Avoid backend-specific errors deleting absent labels.
			present := false
			for _, l := range issue.Labels {
				if l == label {
					present = true
				}
			}
			if !present {
				continue
			}
			if err = tr.RemoveLabel(cmd.Context(), n, label); err != nil {
				return fail("remove "+label, err)
			}
			p.Completed = append(p.Completed, "removed "+label)
		}
		if err = tr.AddLabels(cmd.Context(), n, []string{"swarm:draft"}); err != nil {
			return fail("add draft label", err)
		}
		p.Completed = append(p.Completed, "added draft label")
		if _, err = tr.UpdateIssueBody(cmd.Context(), n, body); err != nil {
			return fail("update body", err)
		}
		persisted = true
		p.Completed = append(p.Completed, "body persisted")
		if title != issue.Title {
			if _, err = tr.UpdateIssueTitle(cmd.Context(), n, title); err != nil {
				return fail("update title", err)
			}
			p.Completed = append(p.Completed, "title persisted")
		}
		workflow.ProjectDependencies(cmd.Context(), tr, n, wu.DependsOn, func(message string) {
			p.Warnings = append(p.Warnings, message+"; retry the failed blocked-by link in GitHub; dependencies remain authoritative in the contract")
		})
		p.Status = "converted"
		if len(p.Warnings) > 0 {
			p.Status = "converted-with-warnings"
		}
		return emit()
	}
	f := cmd.Flags()
	f.StringVar(&wu.Title, "title", "", "Optional issue title")
	f.StringVar(&wu.Objective, "objective", "", "Explicit WorkUnit objective (required)")
	f.StringArrayVar(&wu.Constraints, "constraint", nil, "Constraint bullet (repeat for each bullet)")
	f.StringArrayVar(&wu.Acceptance, "acceptance", nil, "Explicit acceptance criterion (required; repeat for each criterion)")
	f.StringVar(&wu.Workflow, "workflow", "feature", "Workflow name (feature, bugfix, refactor)")
	f.StringVar(&gate, "gate", "none", "Task plan gate (none, agent, human); approval also enforces workflow and policy")
	f.IntSliceVar(&wu.DependsOn, "depends-on", nil, "Prerequisite issue numbers")
	f.StringSliceVar(&wu.RequiredCapabilities, "capability", nil, "Required node capabilities")
	f.BoolVar(&wu.AllowHarnessChanges, "allow-harness-changes", false, "Allow edits to protected harness paths")
	f.StringVar(&wu.Size, "size", "", "WorkUnit size (small, multi_area)")
	f.StringVar(&wu.Uncertainty, "uncertainty", "", "WorkUnit uncertainty (known, unknown)")
	f.StringVar(&wu.Risk, "risk", "", "WorkUnit risk (standard, sensitive, urgent)")
	f.BoolVar(&dryRun, "dry-run", false, "Preview and validate without writing")
	f.BoolVar(&asJSON, "json", false, "Emit structured conversion results")
	return cmd
}
