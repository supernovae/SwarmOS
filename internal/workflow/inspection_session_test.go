package workflow

import (
	"context"
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
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
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
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
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
