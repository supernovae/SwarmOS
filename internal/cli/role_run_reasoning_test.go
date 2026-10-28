package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
)

// issue status, pr status and PR evidence share writeRoleRuns / RoleRunTable
// and the issueStatus JSON; reasoning shows when reported and stays
// unavailable (JSON null) when not.
func TestRoleRunsShowReasoningTokens(t *testing.T) {
	omp := contract.NewRoleRunTelemetry("developer", "omp", time.Now())
	omp.InputTokens, omp.OutputTokens, omp.CachedInputTokens, omp.ReasoningTokens = contract.Count(1806), contract.Count(66), contract.Count(1200), contract.Count(21)
	omp.UsageSource = contract.UsageSourceSessionLog
	claude := contract.NewRoleRunTelemetry("tester", "claude", time.Now())
	st := &lease.IssueState{RoleRuns: []contract.RoleRunTelemetry{*omp, *claude}}
	runs := lease.RoleRuns(st)
	raw, err := json.Marshal(issueStatus{RoleRuns: runs})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		RoleRuns []map[string]any `json:"role_runs"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.RoleRuns) != 2 || decoded.RoleRuns[0]["reasoning_tokens"] != float64(21) || decoded.RoleRuns[0]["usage_source"] != "session-log" {
		t.Fatalf("status JSON = %s", raw)
	}
	if value, ok := decoded.RoleRuns[1]["reasoning_tokens"]; !ok || value != nil {
		t.Fatalf("unreported reasoning must be null: %s", raw)
	}
	var text strings.Builder
	writeRoleRuns(&text, runs)
	for _, want := range []string{"Role runs:\n" + lease.RoleRunTableHeader, "| 1806/66/1200 |", "| 21 | unavailable |\n", "unavailable/unavailable | unavailable | unavailable |\n"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("role runs lack %q:\n%s", want, text.String())
		}
	}
}
