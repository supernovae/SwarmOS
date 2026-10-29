package cli

import (
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"os"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/fork"
	"github.com/supernovae/swarmos/internal/tracker"
)

func newForkCoordinatorCmd(rootOpts *RootOptions) *cobra.Command {
	var eventPath string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "fork-coordinator",
		Short: "Process a GitHub issue_comment.created fork worker request in an upstream Action",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if rootOpts.Fork {
				return fmt.Errorf("fork-coordinator runs in the upstream repository and cannot be invoked with --fork")
			}
			if strings.TrimSpace(eventPath) == "" {
				return fmt.Errorf("--event <path> is required")
			}
			ev, err := fork.LoadEventFromFile(eventPath)
			if err != nil {
				return err
			}
			if !fork.IsRequestComment(ev.Comment.Body) {
				if asJSON {
					return writeJSON(cmd, map[string]any{"kind": "fork-coordinator", "ignored": true})
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Ignored non-fork-request comment.")
				return nil
			}
			repoRoot := rootOpts.resolveRepoRoot()
			tr, err := rootOpts.resolveTracker(cmd)
			if err != nil {
				return err
			}
			upstreamRepo := strings.TrimSpace(ev.Repository.FullName)
			if gh, ok := tr.(*tracker.GitHubTracker); ok && gh.UpstreamRepo() != "" {
				upstreamRepo = gh.UpstreamRepo()
			}
			coord := &fork.Coordinator{
				RepoRoot:       repoRoot,
				UpstreamRepo:   upstreamRepo,
				UpstreamRemote: rootOpts.effectiveUpstreamRemote(),
				Tracker:        tr,
			}
			coord.InitForkReader = func() (func(context.Context, string, string) (string, error), func(context.Context, string, string, string) (string, error), error) {
				token := os.Getenv("SWARM_GITHUB_FORK_READ_TOKEN")
				if token == "" {
					return nil, nil, nil
				}
				if err := validateForkReadToken(token); err != nil {
					return nil, nil, err
				}
				host := os.Getenv("GH_HOST")
				if host == "" {
					host = "github.com"
				}
				reader, err := tracker.NewForkReader(upstreamRepo, host, token)
				if err != nil {
					return nil, nil, err
				}
				return func(ctx context.Context, slug, owner string) (string, error) {
						return reader.VerifyForkRepository(ctx, slug, owner, false)
					}, func(ctx context.Context, slug, op, value string) (string, error) {
						out, err := gitcredential.ForkRead(ctx, repoRoot, host, slug, token, op, value)
						return strings.TrimSpace(string(out)), err
					}, nil
			}
			res, err := coord.HandleEvent(cmd.Context(), *ev)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd, map[string]any{
					"kind":   "fork-coordinator",
					"result": res,
				})
			}
			if res != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Processed fork request %d (%s) on issue #%d [revision=%s]\n", res.RequestID, res.Operation, res.IssueNumber, res.Revision)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&eventPath, "event", "", "Path to the GitHub issue_comment webhook event payload ($GITHUB_EVENT_PATH)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit coordinator execution result as JSON")
	return cmd
}

func validateForkReadToken(token string) error {
	for _, r := range token {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r > 126 {
			return fmt.Errorf("invalid fork read credential")
		}
	}
	return nil
}
