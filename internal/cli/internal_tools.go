package cli

import (
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/sandbox"
)

func newInternalToolsCmd() *cobra.Command {
	var executor sandbox.ToolExecutor
	cmd := &cobra.Command{Use: "internal-tools", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return sandbox.ServeTools(cmd.Context(), executor, cmd.InOrStdin(), cmd.OutOrStdout())
		}}
	cmd.Flags().StringVar(&executor.Work, "workspace", "", "Detached task workspace")
	cmd.Flags().BoolVar(&executor.ReadOnly, "read-only", false, "Permit writes only to role artifacts")
	_ = cmd.MarkFlagRequired("workspace")
	return cmd
}
