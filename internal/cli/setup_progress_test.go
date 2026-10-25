package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"gopkg.in/yaml.v3"
)

// Stub only GitHub's tracker boundary; use the native Git CAS store against a
// local bare remote, without fork transport or network credentials.
type setupGitHubTracker struct{ tracker.Tracker }

func (setupGitHubTracker) BackendName() string { return "github" }

func TestSetupProgressNativeGitHub(t *testing.T) {
	for _, mode := range []string{"claim", "interactive", "headless"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			cfg, err := harness.LoadConfig(f.root)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Agents["mock"] = contract.AgentConfig{Binary: "mock"}
			data, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.root, ".swarm/config.yaml"), data, 0600); err != nil {
				t.Fatal(err)
			}
			prGit(t, f.root, "add", ".swarm/config.yaml")
			prGit(t, f.root, "commit", "-m", "allow fixture mock")
			remote := filepath.Join(t.TempDir(), "remote.git")
			prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
			prGit(t, f.root, "remote", "add", "origin", remote)
			prGit(t, f.root, "push", "origin", "main")
			n := f.create(t, "none")
			tr := setupGitHubTracker{f.tracker}
			manager := lease.NewManager(tr, cfg.Security)
			if err := manager.ConfigureStore(f.root); err != nil {
				t.Fatal(err)
			}
			if _, ok := manager.Store().(*coordination.GitStore); !ok {
				t.Fatal("native GitHub did not select Git CAS")
			}
			if _, err := manager.ApproveTask(t.Context(), n, "alice"); err != nil {
				t.Fatal(err)
			}
			cmd := newRootCmdWithOptions(&RootOptions{RepoRoot: f.root, Backend: "github", trackerOverride: tr})
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			args := []string{"issue", "claim", strconv.Itoa(n), "--agent", "mock", "--json"}
			if mode != "claim" {
				args = []string{"issue", "work", strconv.Itoa(n), "--agent", "mock", "--phase", "plan", "--headless=" + strconv.FormatBool(mode == "headless")}
				if mode == "headless" {
					args = append(args, "--json")
				}
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
			assertSetupProgress(t, stderr.String(), 1, 1)
			if strings.Contains(stdout.String(), " started (") {
				t.Fatal("progress on stdout")
			}
		})
	}
}
