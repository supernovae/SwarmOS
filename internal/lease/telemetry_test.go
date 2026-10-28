package lease

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

func roleRunValue(t *testing.T, tel *contract.RoleRunTelemetry) string {
	t.Helper()
	value, err := tel.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestRoleRunHistoryThroughUpdateArtifact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, _, iss := testManager(t)
	first, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	planner := contract.NewRoleRunTelemetry("planner", "mock", time.Now())
	planner.ReplanCount, planner.SetupMS = contract.Count(0), contract.Count(0)
	value := roleRunValue(t, planner)
	design := contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock:planner", Summary: "plan", Body: "one", Metadata: map[string]string{contract.RoleRunTelemetryKey: value}}
	testPlan := contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "mock:planner", Summary: "tests", Body: "two", Metadata: map[string]string{contract.RoleRunTelemetryKey: value}}
	for _, art := range []contract.Artifact{design, testPlan} {
		if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", art); err != nil {
			t.Fatal(err)
		}
	}
	// A re-plan replaces both artifacts but keeps the superseded run.
	replanned := contract.NewRoleRunTelemetry("planner", "mock", time.Now())
	replanned.ReplanCount = contract.Count(1)
	design.Body, design.Metadata = "one v2", map[string]string{contract.RoleRunTelemetryKey: roleRunValue(t, replanned)}
	st, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", design)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.RoleRuns) != 2 || st.RoleRuns[0].RunID != planner.RunID || st.RoleRuns[1].RunID != replanned.RunID {
		t.Fatalf("planner history = %+v", st.RoleRuns)
	}
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactDesign && a.Metadata[contract.RoleRunTelemetryKey] != roleRunValue(t, replanned) {
			t.Fatalf("artifact metadata lost its run: %v", a.Metadata)
		}
	}
	// A forged, oversized or mismatched observation is dropped; the artifact still lands.
	forged := contract.NewRoleRunTelemetry("developer", "mock", time.Now())
	for _, bad := range []string{`{"version":1,"run_id":"x"}`, strings.Repeat("a", contract.MaxRoleRunTelemetry+1), roleRunValue(t, forged)} {
		st, err = m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactTestPlan, Producer: "mock:planner", Summary: "tests", Body: "two v2", Metadata: map[string]string{contract.RoleRunTelemetryKey: bad}})
		if err != nil {
			t.Fatalf("invalid telemetry failed the artifact: %v", err)
		}
		for _, a := range st.Artifacts {
			if _, ok := a.Metadata[contract.RoleRunTelemetryKey]; ok && a.Type == contract.ArtifactTestPlan {
				t.Fatalf("invalid telemetry was kept: %v", a.Metadata)
			}
		}
		if len(st.RoleRuns) != 2 {
			t.Fatalf("invalid telemetry entered history: %+v", st.RoleRuns)
		}
	}
	// A stale claim cannot record a run.
	m.SetNowFunc(func() time.Time { return first.Lease.ExpiresAt.Add(time.Second) })
	if _, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	m.BindClaim(iss.Number, first.Lease.ClaimID)
	stale := contract.NewRoleRunTelemetry("planner", "mock", time.Now())
	if _, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock:planner", Summary: "plan", Body: "stale", Metadata: map[string]string{contract.RoleRunTelemetryKey: roleRunValue(t, stale)}}); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("stale claim update = %v", err)
	}
	current, err := m.GetCurrentState(ctx, iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.RoleRuns) != 2 {
		t.Fatalf("fenced update changed history: %+v", current.RoleRuns)
	}
}

func TestRoleRunRecordDedupsAndBoundsHistory(t *testing.T) {
	t.Parallel()
	st := &IssueState{}
	reused := contract.NewRoleRunTelemetry("tester", "mock", time.Now())
	value := roleRunValue(t, reused)
	for i := 0; i < 2; i++ {
		// Reused validation carries the original run's metadata; it is not a new run.
		art := contract.Artifact{Type: contract.ArtifactValidation, Metadata: map[string]string{contract.RoleRunTelemetryKey: value}}
		recordRoleRun(st, &art)
	}
	if len(st.RoleRuns) != 1 {
		t.Fatalf("duplicate delivery created runs: %d", len(st.RoleRuns))
	}
	for i := 0; i < MaxRoleRuns+5; i++ {
		art := contract.Artifact{Type: contract.ArtifactImplementation, Metadata: map[string]string{contract.RoleRunTelemetryKey: roleRunValue(t, contract.NewRoleRunTelemetry("developer", "mock", time.Now()))}}
		recordRoleRun(st, &art)
	}
	if len(st.RoleRuns) != MaxRoleRuns || st.RoleRuns[0].Role != "developer" {
		t.Fatalf("history not bounded to newest %d runs: %d", MaxRoleRuns, len(st.RoleRuns))
	}
}

func TestRoleRunRepairAttempts(t *testing.T) {
	t.Parallel()
	st := &IssueState{}
	if got := NextRepairAttempt(st, "developer", false); got != 0 {
		t.Fatalf("initial attempt = %d", got)
	}
	dev := contract.NewRoleRunTelemetry("developer", "mock", time.Now())
	dev.RepairAttempt = contract.Count(1)
	st.RoleRuns = append(st.RoleRuns, *dev)
	if got := NextRepairAttempt(st, "developer", true); got != 2 {
		t.Fatalf("second repair = %d", got)
	}
	if got := NextRepairAttempt(st, "tester", false); got != 1 {
		t.Fatalf("tester of repair = %d", got)
	}
	if got := NextRepairAttempt(st, "planner", false); got != 0 {
		t.Fatalf("planner repair = %d", got)
	}
}

func TestRoleRunTableShowsUnavailableAndZero(t *testing.T) {
	t.Parallel()
	run := contract.NewRoleRunTelemetry("developer", "claude", time.Now())
	run.ElapsedMS, run.SetupMS, run.InputTokens, run.ToolCalls, run.RepeatedCommands = contract.Count(61500), contract.Count(0), contract.Count(12), contract.Count(0), contract.Count(0)
	run.ActualModel = contract.Label("claude-opus-4-5")
	table := RoleRunTable([]contract.RoleRunTelemetry{*run})
	for _, want := range []string{RoleRunTableHeader, "| developer | " + run.RunID[:8] + " | 61.5s | 0ms | unavailable |", "12/unavailable/unavailable", "| 0/0 |", "claude-opus-4-5/unavailable"} {
		if !strings.Contains(table, want) {
			t.Fatalf("table lacks %q:\n%s", want, table)
		}
	}
	if RoleRunTable(nil) != "" {
		t.Fatal("empty history must render nothing")
	}
	// Invalid stored entries are omitted from projections; legacy states have none.
	bad := *run
	bad.Role = "maintainer"
	if got := RoleRuns(&IssueState{RoleRuns: []contract.RoleRunTelemetry{bad, *run}}); len(got) != 1 {
		t.Fatalf("projection kept invalid run: %+v", got)
	}
	var legacy IssueState
	if err := json.Unmarshal([]byte(`{"lease":{},"task_digest":"","policy_digest":"","base_sha":""}`), &legacy); err != nil || len(RoleRuns(&legacy)) != 0 {
		t.Fatalf("legacy state: %v %+v", err, legacy.RoleRuns)
	}
}

func TestFailedRoleRunPersistence(t *testing.T) {
	t.Parallel()
	for _, class := range []string{"capacity", "usage_limit", "rate_limit", "network", "unknown"} {
		t.Run(class, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			m, _, iss := testManager(t)
			_, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			successful := contract.NewRoleRunTelemetry("developer", "mock", time.Now())
			successful.RepairAttempt = contract.Count(2)
			st, err := m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactImplementation, Producer: "mock:developer", Summary: "done", Body: "done", Metadata: map[string]string{contract.RoleRunTelemetryKey: roleRunValue(t, successful)}})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(st.Artifacts)
			failed := contract.NewRoleRunTelemetry("developer", "mock", time.Now())
			failed.FailureClass = class
			for _, attempt := range []*int64{nil, contract.Count(99)} {
				failed.RepairAttempt = attempt
				st, err = m.RecordFailedRoleRun(ctx, iss.Number, "local-dev", failed)
				if err != nil {
					t.Fatal(err)
				}
				if NextRepairAttempt(st, "developer", true) != 3 || NextRepairAttempt(st, "tester", false) != 2 {
					t.Fatal("failed run changed counter")
				}
			}
			if len(st.RoleRuns) != 2 {
				t.Fatal("duplicate failure recorded")
			}
			after, _ := json.Marshal(st.Artifacts)
			if string(before) != string(after) {
				t.Fatal("failure changed artifacts")
			}
			if !strings.Contains(RoleRunTable(RoleRuns(st)), class) {
				t.Fatal("missing failure in status")
			}

			successful = contract.NewRoleRunTelemetry("developer", "mock", time.Now())
			successful.RepairAttempt = contract.Count(3)
			st, err = m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactImplementation, Producer: "mock:developer", Summary: "repair", Body: "done", Metadata: map[string]string{contract.RoleRunTelemetryKey: roleRunValue(t, successful)}})
			if err != nil {
				t.Fatal(err)
			}
			if NextRepairAttempt(st, "tester", false) != 3 {
				t.Fatal("tester lost successful repair")
			}
			firstClaim := st.Lease.ClaimID
			if _, err := m.RecordFailedRoleRun(ctx, iss.Number, "wrong-owner", failed); err == nil {
				t.Fatal("unfenced mutation")
			}
			m.SetNowFunc(func() time.Time { return st.Lease.ExpiresAt.Add(time.Second) })
			if _, err := m.RecordFailedRoleRun(ctx, iss.Number, "local-dev", failed); !errors.Is(err, ErrClaimFence) {
				t.Fatalf("expired claim: %v", err)
			}
			if _, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
				t.Fatal(err)
			}
			m.BindClaim(iss.Number, firstClaim)
			if _, err := m.RecordFailedRoleRun(ctx, iss.Number, "local-dev", failed); !errors.Is(err, ErrClaimFence) {
				t.Fatalf("stale claim: %v", err)
			}

		})
	}
}
