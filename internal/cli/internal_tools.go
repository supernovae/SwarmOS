package cli

import (
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/sandbox"
)

func newInternalToolsCmd() *cobra.Command {
	var executor sandbox.ToolExecutor
	var caches []string
	cmd := &cobra.Command{Use: "internal-tools", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			if executor.BuildCaches, err = parseBuildCaches(caches, executor.Work); err != nil {
				return err
			}
			return sandbox.ServeTools(cmd.Context(), executor, cmd.InOrStdin(), cmd.OutOrStdout())
		}}
	cmd.Flags().StringVar(&executor.Work, "workspace", "", "Detached task workspace")
	cmd.Flags().BoolVar(&executor.ReadOnly, "read-only", false, "Permit writes only to role artifacts")
	cmd.Flags().StringArrayVar(&caches, "build-cache", nil, "Agent build cache as ecosystem=dir (repeatable)")
	_ = cmd.MarkFlagRequired("workspace")
	return cmd
}

// parseBuildCaches accepts only ecosystems from the sandbox build cache table;
// the environment key is taken from the table, never from the flag.
func parseBuildCaches(values []string, workspace string) ([]sandbox.BuildCache, error) {
	var caches []sandbox.BuildCache
	for _, value := range values {
		cache, err := sandbox.ParseBuildCache(value, workspace)
		if err != nil {
			return nil, err
		}
		caches = append(caches, cache)
	}
	return caches, nil
}
