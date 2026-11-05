package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/sandbox"
	"gopkg.in/yaml.v3"
)

// inspectionTask approves a task whose base policy selects a counting
// trusted-local OpenCode that reports version.
func inspectionTask(t *testing.T, version string) (*Engine, string, int, string) {
	t.Helper()
	eng, root, issue := approvedDependencyTask(t)
	dir := t.TempDir()
	binary := filepath.Join(dir, "opencode")
	script := `#!/bin/sh
case "$1 ${2:-}" in
"--version ") printf x >> '` + dir + `/version'; echo "opencode v` + version + `" ;;
"run --help") echo "--standalone --server --continue --model --agent --format --title" ;;
esac
`
	if err := testutil.WriteExecutable(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := harness.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Execution.Mode = contract.ExecutionTrustedLocal
	cfg.Agents["opencode"] = contract.AgentConfig{Binary: binary}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".swarm", "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "select counting harness"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	if eng, err = NewEngineWithOptions(root, eng.Tracker, EngineOptions{NodeProfilePath: filepath.Join(t.TempDir(), "node.yaml")}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.LeaseManager.ApproveTask(context.Background(), issue, "local-dev"); err != nil {
		t.Fatal(err)
	}
	return eng, root, issue, dir
}

func versionCalls(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "version"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(data)
}

func TestInspectionCommandSessionReusesPreclaimInspection(t *testing.T) {
	eng, root, issue, dir := inspectionTask(t, "2.1.2")
	opts := WorkOptions{RepoRoot: root, IssueNumber: issue, AgentName: "opencode", Phase: "all", InspectOnly: true}
	// A reused Engine shares nothing between commands; each preflights all roles.
	for command := 1; command <= 2; command++ {
		ctx := sandbox.WithInspectionSession(context.Background(), sandbox.NewInspectionSession())
		if _, err := eng.Preflight(ctx, opts); err != nil {
			t.Fatal(err)
		}
		if _, err := eng.Preflight(ctx, opts); err != nil {
			t.Fatal(err)
		}
		if got := versionCalls(t, dir); got != command {
			t.Fatalf("command %d: %d inspections in total, want %d", command, got, command)
		}
	}
	before := versionCalls(t, dir)
	if _, err := eng.Preflight(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := versionCalls(t, dir) - before; got < 2 {
		t.Fatalf("uncached command inspected %d times, want one per role", got)
	}
}

func TestInspectionPreflightFailureStopsBeforeClaim(t *testing.T) {
	eng, root, issue, _ := inspectionTask(t, "1.0.0")
	ctx := sandbox.WithInspectionSession(context.Background(), sandbox.NewInspectionSession())
	_, err := eng.ExecuteWork(ctx, WorkOptions{RepoRoot: root, IssueNumber: issue, AgentName: "opencode", Phase: "plan", Headless: true})
	if err == nil || !strings.Contains(err.Error(), "agent preflight") {
		t.Fatalf("ExecuteWork error = %v, want an agent preflight error", err)
	}
	st, err := eng.LeaseManager.GetCurrentState(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if st.Lease.ClaimID != "" {
		t.Fatalf("failed inspection acquired claim %q", st.Lease.ClaimID)
	}
}

func TestToolCapabilityOptionalFailurePreflight(t *testing.T) {
	eng, root, issue, _ := inspectionTask(t, "2.1.2")
	candidate, err := harness.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Execution.BrowserProfile = true
	candidate.Agents["opencode"] = contract.AgentConfig{Binary: "candidate-grant", Harness: "claude"}
	raw, err := yaml.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".swarm/config.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	eng.InspectCapabilityEntry = func(id string) error {
		if id == "browser" {
			return fmt.Errorf("TOKEN /home/private-user/secret")
		}
		return nil
	}
	for _, inspectOnly := range []bool{false, true} {
		result, err := eng.Preflight(t.Context(), WorkOptions{RepoRoot: root, IssueNumber: issue, AgentName: "opencode", Phase: "plan", InspectOnly: inspectOnly})
		if err != nil || len(result.Blockers) != 0 {
			t.Fatalf("optional report failure blocked preflight: %v %+v", err, result)
		}
		for _, r := range result.ToolCapabilities {
			if r.Harness != "opencode" || r.Entries[1].Declared {
				t.Fatal("candidate config influenced report", r)
			}
			if r.Entries[1].Runtime != "not_probed" || r.Entries[1].Usable {
				t.Fatal(r)
			}
			if strings.Contains(sandbox.FormatToolCapabilities(r), "TOKEN") {
				t.Fatal("secret escaped")
			}
		}
	}
	st, err := eng.LeaseManager.GetCurrentState(t.Context(), issue)
	if err != nil || st.Lease.ClaimID != "" {
		t.Fatal("preflight acquired a lease", err)
	}
}

// The overlay injects only the existing mandatory boundary probe's result. It
// leaves report assembly, approved-policy loading and claim ordering intact,
// without adding a production bypass or requiring a live Bubblewrap boundary.
func TestToolCapabilityInjectedBoundaryPreflight(t *testing.T) {
	if os.Getenv("SWARM_TEST_BOUNDARY_OVERLAY") == "1" {
		testToolCapabilityInjectedBoundaryPreflight(t)
		return
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "internal/agent/configured.go")
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	const needle = "sandbox.CheckToolExecutor(ctx)"
	if strings.Count(string(raw), needle) != 1 {
		t.Fatal("mandatory probe call changed; review injection")
	}
	replacement := `func() error {
 if result := getenvWith(opts.LookupEnv, "SWARM_TEST_BOUNDARY_RESULT"); result != "" { return fmt.Errorf("%s", result) }
 return nil
 }()`
	dir := t.TempDir()
	injected := filepath.Join(dir, "configured.go")
	if err := os.WriteFile(injected, []byte(strings.Replace(string(raw), needle, replacement, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: injected}})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "test", "-overlay="+overlayPath, "-v", "-timeout=2m", "./internal/workflow", "-run=^TestToolCapabilityInjectedBoundaryPreflight$", "-count=1")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "SWARM_TEST_BOUNDARY_OVERLAY=1")
	out, err := cmd.CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("injected boundary tests: %v", err)
	}
}

func testToolCapabilityInjectedBoundaryPreflight(t *testing.T) {
	const action = "Restore the selected runtime prerequisites and rerun inspection."
	for _, failure := range []string{"filesystem canary failed", "network canary failed", "read-only canary failed", "missing Python", "missing Bubblewrap"} {
		for _, mandatory := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/mandatory=%t", failure, mandatory), func(t *testing.T) {
				eng, root, issue, _ := inspectionTask(t, "2.1.2")
				cfg, err := harness.LoadConfig(root)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Execution.Mode = contract.ExecutionIsolated
				cfg.Execution.Backend = contract.BackendNative
				raw, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".swarm/config.yaml"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "select isolated boundary"}} {
					if out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
						t.Fatalf("git: %v (%s)", err, out)
					}
				}
				if _, err := eng.LeaseManager.ApproveTask(t.Context(), issue, "local-dev"); err != nil {
					t.Fatal(err)
				}
				calls := 0
				probeErr := fmt.Errorf("%s: raw stderr TOKEN-SECRET /home/private-user/secret", failure)
				eng.LookupEnv = func(key string) (string, bool) {
					if key == "SWARM_TEST_BOUNDARY_RESULT" {
						calls++
						if mandatory {
							return probeErr.Error(), true
						}
						return "", true
					}
					return "", false
				}
				if !mandatory {
					eng.InspectCapabilityEntry = func(id string) error {
						if id == "browser" {
							return probeErr
						}
						return nil
					}
				}
				opts := WorkOptions{RepoRoot: root, IssueNumber: issue, AgentName: "opencode", Phase: "plan", InspectOnly: true}
				result, err := eng.Preflight(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				if calls == 0 {
					t.Fatal("injected boundary probe was not reached")
				}
				if (len(result.Blockers) > 0) != mandatory {
					t.Fatalf("blockers = %v", result.Blockers)
				}
				if len(result.ToolCapabilities) == 0 {
					t.Fatal("missing role reports")
				}
				for _, report := range result.ToolCapabilities {
					entry := report.Entries[0]
					reason, wantAction, runtime := "unavailable", action, "unavailable"
					if !mandatory {
						if !entry.Usable {
							t.Fatal("optional prerequisite disabled mandatory transport")
						}
						entry = report.Entries[1]
						reason, wantAction, runtime = "report_unavailable", "Rerun inspection; optional report assembly did not complete.", "not_probed"
					}
					if entry.Reason != reason || entry.Action != wantAction || entry.Runtime != runtime || entry.Usable {
						t.Fatalf("unexpected failed entry: %+v", entry)
					}
					encoded, err := json.Marshal(report)
					if err != nil {
						t.Fatal(err)
					}
					text := sandbox.FormatToolCapabilities(report) + string(encoded) + strings.Join(result.Blockers, " ")
					for _, secret := range []string{failure, "raw stderr", "TOKEN-SECRET", "/home/", "private-user"} {
						if strings.Contains(text, secret) {
							t.Fatalf("probe data escaped: %q", secret)
						}
					}
				}
				opts.InspectOnly = false
				if mandatory {
					opts.Headless = true
					_, err = eng.ExecuteWork(t.Context(), opts)
					if err == nil || !strings.Contains(err.Error(), "agent preflight") || strings.Contains(err.Error(), "TOKEN-SECRET") {
						t.Fatalf("expected bounded preclaim refusal: %v", err)
					}
				} else if _, err := eng.Preflight(t.Context(), opts); err != nil {
					t.Fatalf("optional prerequisite blocked preflight: %v", err)
				}
				st, err := eng.LeaseManager.GetCurrentState(t.Context(), issue)
				if err != nil || st.Lease.ClaimID != "" {
					t.Fatal("probe acquired claim", err)
				}
			})
		}
	}
}
