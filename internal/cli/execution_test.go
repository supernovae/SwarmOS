package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/workflow"
	"gopkg.in/yaml.v3"
)

func TestDryRunReportsIsolationBlockersWithoutClaim(t *testing.T) {
	f := newCLIFixture(t)
	// Exercise a missing harness even when the developer has Codex installed.
	cfg, err := harness.LoadConfig(f.root)
	if err != nil {
		t.Fatal(err)
	}
	ac := cfg.Agents["codex"]
	ac.Binary = filepath.Join(t.TempDir(), "missing-codex")
	cfg.Agents["codex"] = ac
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, ".swarm", "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".swarm/config.yaml"}, {"commit", "-m", "configure unavailable test harness"}} {
		if out, err := exec.Command("git", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	t.Setenv("SWARM_EXECUTION_MODE", "")
	var result struct {
		Plan workflow.PreflightResult `json:"effective_plan"`
	}
	if err := json.Unmarshal([]byte(f.must(t, "work", n, "--dry-run")), &result); err != nil {
		t.Fatal(err)
	}
	if result.Plan.Execution.Mode != contract.ExecutionIsolated || result.Plan.Execution.Backend != contract.BackendNative || len(result.Plan.Blockers) == 0 {
		t.Fatalf("missing default boundary diagnostics: %+v", result.Plan)
	}
	issue, _ := strconv.Atoi(n)
	if st := f.state(t, issue); st.Lease.Status == "active" || st.Lease.ClaimID != "" {
		t.Fatal("inspection acquired a lease")
	}
	if out, err := f.run("work", n); err == nil || !strings.Contains(out+err.Error(), "preflight") {
		t.Fatalf("work did not fail closed: %v %s", err, out)
	}
	if st := f.state(t, issue); st.Lease.Status == "active" || st.Lease.ClaimID != "" {
		t.Fatal("blocked execution acquired a lease")
	}
}
