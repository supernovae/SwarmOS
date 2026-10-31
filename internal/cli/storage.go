package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/storage"
)

func newStorageCmd(opts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{Use: "storage", Short: "Inspect Swarm-managed host storage"}
	jsonOutput := false
	report := &cobra.Command{Use: "report", Short: "Report storage observations without changing storage or workflow state", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		root := opts.RepoRoot
		if root != "" {
			root, err = filepath.Abs(root)
			if err != nil {
				return err
			}
		}
		snapshot, scanErr := storage.Scan(cmd.Context(), storage.Options{RegistryPath: opts.registryPath(), CacheRoot: filepath.Join(cache, "swarm"), ScratchRoot: os.TempDir(), SelectedRoot: root, Repository: opts.Repo})
		if err := renderStorageReport(cmd.OutOrStdout(), snapshot, jsonOutput); err != nil {
			return err
		}
		return scanErr
	}}
	report.Flags().BoolVar(&jsonOutput, "json", false, "Print a JSON snapshot including unknown and incomplete observations")
	cmd.AddCommand(report)
	return cmd
}
func renderStorageReport(w io.Writer, s storage.Snapshot, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	if _, err := fmt.Fprintf(w, "Storage report: complete=%t\nScope: %s\nAllocated bytes are observations, not reclaimable savings. Unknown use evidence retains storage.\n", s.Complete, s.Scope); err != nil {
		return err
	}
	for _, reason := range s.Reasons {
		if _, err := fmt.Fprintf(w, "INCOMPLETE: %s\n", reason); err != nil {
			return err
		}
	}
	number := func(v *int64) string {
		if v == nil {
			return "unknown"
		}
		return strconv.FormatInt(*v, 10)
	}
	for _, r := range s.Resources {
		active := "unknown"
		if r.Active != nil {
			active = strconv.FormatBool(*r.Active)
		}
		last := "unknown"
		if r.LastUse != nil {
			last = r.LastUse.UTC().Format("2006-01-02T15:04:05Z")
		}
		if _, err := fmt.Fprintf(w, "%s owner=%q repository=%q path=%q ref=%q\n  logical=%s allocated=%s active=%s last-use=%s eligibility=%s\n  %s; evidence=%s; usage=%s\n", r.Class, r.Owner, r.Repository, r.Path, r.Ref, number(r.LogicalBytes), number(r.AllocatedBytes), active, last, r.Eligibility, r.Reason, r.Evidence, r.UsageReason); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "Resources: %d; entries observed: %d; unique allocated bytes: %s (partial observed: %d)\n", len(s.Resources), s.Counters.Entries, number(s.UniqueAllocatedBytes), s.ObservedAllocatedBytes)
	return err
}
