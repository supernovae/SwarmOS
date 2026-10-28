package contract

import (
	"strings"
	"testing"
	"time"
)

func TestTelemetryRoundTripKeepsNullAndMeasuredZero(t *testing.T) {
	tel := NewRoleRunTelemetry("planner", "codex", time.Unix(1700000000, 123456789))
	tel.ElapsedMS, tel.SetupMS = Count(1500), Count(0)
	tel.ActualModel = Label("gpt-5-codex")
	value, err := tel.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"setup_ms":0`, `"preflight_ms":null`, `"input_tokens":null`, `"effort":null`, `"actual_model":"gpt-5-codex"`, `"usage_source":"none"`} {
		if !strings.Contains(value, want) {
			t.Fatalf("encoded telemetry lacks %s: %s", want, value)
		}
	}
	parsed, err := ParseRoleRunTelemetry(value)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RunID != tel.RunID || *parsed.SetupMS != 0 || parsed.PreflightMS != nil || *parsed.ActualModel != "gpt-5-codex" || !parsed.StartedAt.Equal(tel.StartedAt) {
		t.Fatalf("round trip changed telemetry: %+v", parsed)
	}
	if other := NewRoleRunTelemetry("planner", "codex", time.Now()); other.RunID == tel.RunID {
		t.Fatal("run ids must be unique per run")
	}
}

func TestTelemetryValidationRejectsUntrustedValues(t *testing.T) {
	valid := func() *RoleRunTelemetry { return NewRoleRunTelemetry("developer", "claude", time.Now()) }
	bad := "evil|`<b>\x1b[31m"
	for _, tc := range []struct {
		name   string
		mutate func(*RoleRunTelemetry)
	}{
		{"version", func(t *RoleRunTelemetry) { t.Version = 2 }},
		{"run id", func(t *RoleRunTelemetry) { t.RunID = "../../etc" }},
		{"role", func(t *RoleRunTelemetry) { t.Role = "maintainer" }},
		{"harness", func(t *RoleRunTelemetry) { t.Harness = bad }},
		{"source", func(t *RoleRunTelemetry) { t.UsageSource = "stdout" }},
		{"start", func(t *RoleRunTelemetry) { t.StartedAt = time.Time{} }},
		{"negative", func(t *RoleRunTelemetry) { t.InputTokens = Count(-1) }},
		{"overflow", func(t *RoleRunTelemetry) { t.ToolCalls = Count(1 << 60) }},
		{"model injection", func(t *RoleRunTelemetry) { t.ActualModel = &bad }},
		{"long effort", func(t *RoleRunTelemetry) { long := strings.Repeat("a", MaxTelemetryLabel+1); t.Effort = &long }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := valid()
			tc.mutate(tel)
			if _, err := tel.Encode(); err == nil {
				t.Fatal("invalid telemetry encoded")
			}
		})
	}
	if Label(bad) != nil || Label("") != nil || NewRoleRunTelemetry("tester", bad, time.Now()).Harness != "unknown" {
		t.Fatal("unsafe labels must be unavailable")
	}
	for _, value := range []string{"{", `{"version":1}`, strings.Repeat(" ", MaxRoleRunTelemetry+1)} {
		if _, err := ParseRoleRunTelemetry(value); err == nil {
			t.Fatalf("accepted invalid metadata %.40q", value)
		}
	}
}

func TestTelemetryMetadataPassesArtifactValidation(t *testing.T) {
	value, err := NewRoleRunTelemetry("tester", "mock", time.Now()).Encode()
	if err != nil {
		t.Fatal(err)
	}
	art := Artifact{Version: 1, ID: "art-1-validation", IssueNumber: 1, Type: ArtifactValidation, Producer: "mock:tester", Summary: "s", Body: "b", CreatedAt: time.Now(), Metadata: map[string]string{RoleRunTelemetryKey: value}}
	if err := ValidateArtifact(&art); err != nil {
		t.Fatal(err)
	}
}

func TestFailureClassValidation(t *testing.T) {
	for _, class := range []string{"", "capacity", "usage_limit", "rate_limit", "network", "unknown", "secret-token", "\x1b"} {
		run := NewRoleRunTelemetry("developer", "codex", time.Now())
		run.FailureClass = class
		value, err := run.Encode()
		invalid := class == "secret-token" || class == "\x1b"
		if invalid {
			if err == nil {
				t.Fatal("accepted unsafe class")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseRoleRunTelemetry(value)
		if err != nil || parsed.FailureClass != class {
			t.Fatal("class round trip")
		}
	}
}
