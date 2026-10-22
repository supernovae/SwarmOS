package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workflow"
)

func TestDryRunReportsIsolationBlockersWithoutClaim(t *testing.T) {
	f := newCLIFixture(t)
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
