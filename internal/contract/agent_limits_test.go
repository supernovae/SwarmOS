package contract

import (
	"strings"
	"testing"
	"time"
)

func durationPointer(s string) *string { return &s }
func TestAgentLimits(t *testing.T) {
	for _, role := range []string{"planner", "reviewer", "developer", "tester"} {
		for _, backend := range []string{BackendNative, BackendOCI} {
			for _, interactive := range []bool{false, true} {
				for _, configured := range []bool{false, true} {
					e := ExecutionConfig{Backend: backend}
					want := time.Hour
					if configured {
						e.AgentTimeout = durationPointer("90m")
						e.RoleTimeouts = map[string]string{role: "2h"}
						want = 2 * time.Hour
					}
					got, err := ResolveAgentLimits(e, role, interactive, 0)
					if err != nil {
						t.Fatal(err)
					}
					if interactive && backend == BackendNative {
						if got.Active != want || got.Wall != 4*time.Hour || !got.ExcludeIdle {
							t.Fatalf("%+v", got)
						}
					} else {
						if interactive && !configured {
							want = 4 * time.Hour
						}
						if got.Wall != want || got.ExcludeIdle {
							t.Fatalf("%+v want %s", got, want)
						}
					}
					shortened, err := ResolveAgentLimits(e, role, interactive, time.Minute)
					if err != nil || (shortened.Active != time.Minute && shortened.Wall != time.Minute) {
						t.Fatalf("shortening: %+v %v", shortened, err)
					}
				}
			}
		}
	}
}
func TestInvalidAgentLimits(t *testing.T) {
	for _, value := range []string{"", "bogus", "0s", "-1m", "999999999999999999h"} {
		for _, e := range []ExecutionConfig{{AgentTimeout: durationPointer(value)}, {RoleTimeouts: map[string]string{"developer": value}}} {
			if _, err := ResolveAgentLimits(e, "developer", false, 0); err == nil || !strings.Contains(err.Error(), "execution.") {
				t.Fatalf("accepted %+v: %v", e, err)
			}
		}
	}
	if _, err := ResolveAgentLimits(ExecutionConfig{RoleTimeouts: map[string]string{"invalid": "1m"}}, "developer", false, 0); err == nil {
		t.Fatal("accepted unknown role")
	}
	for _, limit := range []time.Duration{time.Second, time.Minute, time.Hour} {
		if got := AgentWarningLead(limit); got != min(2*time.Minute, limit/10) {
			t.Fatal(got)
		}
	}
}
func TestAgentLimitsCannotBeExtendedByOperator(t *testing.T) {
	e := ExecutionConfig{AgentTimeout: durationPointer("1m")}
	got, err := ResolveAgentLimits(e, "tester", false, time.Hour)
	if err != nil || got.Wall != time.Minute {
		t.Fatalf("%+v %v", got, err)
	}
	got, err = ResolveAgentLimits(ExecutionConfig{Backend: BackendOCI, AgentTimeout: durationPointer("8h")}, "tester", true, 0)
	if err != nil || got.Wall != 4*time.Hour {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestOperatorShortensDefaultInteractiveOCIWall(t *testing.T) {
	got, err := ResolveAgentLimits(ExecutionConfig{Backend: BackendOCI}, "developer", true, 2*time.Hour)
	if err != nil || got.Wall != 2*time.Hour {
		t.Fatalf("%+v %v", got, err)
	}
}
