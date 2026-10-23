package cli

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func init() {
	if info, ok := debug.ReadBuildInfo(); ok {
		Version, Commit, Date = resolveBuildVersion(Version, Commit, Date, info)
	}
}

// Release linker flags take precedence over the metadata embedded by Go.
// Without a linker-provided build date, Date is the source commit time.
func resolveBuildVersion(version, commit, date string, info *debug.BuildInfo) (string, string, string) {
	autoVersion := version == "dev"
	if autoVersion && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = strings.TrimPrefix(info.Main.Version, "v")
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if commit == "none" && setting.Value != "" {
				commit = setting.Value
			}
		case "vcs.time":
			if date == "unknown" && setting.Value != "" {
				date = setting.Value
			}
		case "vcs.modified":
			if autoVersion && setting.Value == "true" && !strings.HasSuffix(version, "+dirty") {
				version += "+dirty"
			}
		}
	}
	return version, commit, date
}

func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print SwarmOS version and build information",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := map[string]string{
				"version": Version,
				"commit":  Commit,
				"date":    Date,
				"go":      runtime.Version(),
				"os":      runtime.GOOS,
				"arch":    runtime.GOARCH,
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "swarm version %s (commit: %s, built: %s, %s %s/%s)\n",
				Version, Commit, Date, info["go"], info["os"], info["arch"])
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output version info as JSON")
	return cmd
}
