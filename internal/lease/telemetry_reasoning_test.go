package lease

import (
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestRoleRunTableShowsReasoning(t *testing.T) {
	t.Parallel()
	reported := contract.NewRoleRunTelemetry("developer", "omp", time.Now())
	reported.InputTokens, reported.OutputTokens, reported.CachedInputTokens, reported.ReasoningTokens = contract.Count(1806), contract.Count(66), contract.Count(1200), contract.Count(21)
	reported.ActualModel = contract.Label("fake-model")
	unreported := contract.NewRoleRunTelemetry("developer", "claude", time.Now())
	unreported.InputTokens = contract.Count(25)
	table := RoleRunTable([]contract.RoleRunTelemetry{*reported, *unreported})
	for _, want := range []string{
		"| Model/effort | Reasoning tokens |",
		"| 1806/66/1200 | unavailable/unavailable | unavailable/unavailable | fake-model/unavailable | 21 |",
		"| 25/unavailable/unavailable | unavailable/unavailable | unavailable/unavailable | unavailable/unavailable | unavailable |",
	} {
		if !strings.Contains(table, want) {
			t.Fatalf("table lacks %q:\n%s", want, table)
		}
	}
	lines := strings.Split(strings.TrimSpace(table), "\n")
	for _, line := range lines {
		if got, want := strings.Count(line, "|"), strings.Count(RoleRunTableHeader, "|"); got != want {
			t.Fatalf("row has %d separators, header %d: %s", got, want, line)
		}
	}
}
