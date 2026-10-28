package contract

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// RoleRunTelemetryKey is the artifact metadata key carrying one role run's
// observed breakdown. The value is untrusted descriptive data: it never feeds
// approval, validation, review verdicts or any other gate.
const RoleRunTelemetryKey = "role_run_telemetry"

const (
	RoleRunTelemetryVersion = 1
	MaxRoleRunTelemetry     = 8 << 10
	MaxTelemetryLabel       = 128
	maxTelemetryValue       = 1 << 53
)

// Usage sources say where token, model and tool counts came from.
const (
	UsageSourceNone       = "none"        // nothing was observed
	UsageSourceSessionLog = "session-log" // the harness's own session log in the run's scratch HOME
	UsageSourceMock       = "mock"        // deterministic mock adapter values
)

var (
	telemetryRunID = regexp.MustCompile(`^[a-f0-9]{32}$`)
	telemetryLabel = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]+$`)
)

// RoleRunTelemetry is the per-run breakdown. Every nil pointer means the value
// was not observed and is shown as unavailable; a measured zero stays zero.
// Durations are milliseconds. Repair and re-plan counters are coordinator
// attempt numbers (initial run = 0), not inferred agent retries.
type RoleRunTelemetry struct {
	FailureClass      string    `json:"failure_class,omitempty"`
	Version           int       `json:"version"`
	RunID             string    `json:"run_id"`
	Role              string    `json:"role"`
	Harness           string    `json:"harness"`
	StartedAt         time.Time `json:"started_at"`
	ElapsedMS         *int64    `json:"elapsed_ms"`
	SetupMS           *int64    `json:"setup_ms"`
	PreflightMS       *int64    `json:"preflight_ms"`
	SnapshotMS        *int64    `json:"snapshot_ms"`
	ModelMS           *int64    `json:"model_ms"`
	ToolMS            *int64    `json:"tool_ms"`
	InputTokens       *int64    `json:"input_tokens"`
	OutputTokens      *int64    `json:"output_tokens"`
	CachedInputTokens *int64    `json:"cached_input_tokens"`
	ToolCalls         *int64    `json:"tool_calls"`
	RepeatedCommands  *int64    `json:"repeated_commands"`
	RepairAttempt     *int64    `json:"repair_attempt"`
	ReplanCount       *int64    `json:"replan_count"`
	ActualModel       *string   `json:"actual_model"`
	Effort            *string   `json:"effort"`
	UsageSource       string    `json:"usage_source"`
}

// NewRoleRunTelemetry starts a coordinator-identified run observation.
func NewRoleRunTelemetry(role, harness string, started time.Time) *RoleRunTelemetry {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	if !telemetryLabel.MatchString(harness) || len(harness) > MaxTelemetryLabel {
		harness = "unknown"
	}
	return &RoleRunTelemetry{Version: RoleRunTelemetryVersion, RunID: hex.EncodeToString(id), Role: role,
		Harness: harness, StartedAt: started.UTC().Truncate(time.Millisecond), UsageSource: UsageSourceNone}
}

// Millis converts a measured duration into a telemetry value.
func Millis(d time.Duration) *int64 {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return &ms
}

// Count wraps an observed count.
func Count(n int64) *int64 { return &n }

// Label wraps an observed label, or reports unavailable when it is unsafe.
func Label(s string) *string {
	if len(s) == 0 || len(s) > MaxTelemetryLabel || !telemetryLabel.MatchString(s) {
		return nil
	}
	return &s
}

// Validate rejects telemetry that is malformed, out of range or carries
// labels that could inject Markdown or terminal control sequences.
func (t *RoleRunTelemetry) Validate() error {
	if t == nil {
		return fmt.Errorf("telemetry is nil")
	}
	switch t.FailureClass {
	case "", "capacity", "usage_limit", "rate_limit", "network", "unknown":
	default:
		return fmt.Errorf("invalid telemetry failure class")
	}
	if t.Version != RoleRunTelemetryVersion {
		return fmt.Errorf("unsupported telemetry version %d", t.Version)
	}
	if !telemetryRunID.MatchString(t.RunID) {
		return fmt.Errorf("invalid telemetry run id")
	}
	switch t.Role {
	case "planner", "reviewer", "developer", "tester":
	default:
		return fmt.Errorf("invalid telemetry role")
	}
	if len(t.Harness) > MaxTelemetryLabel || !telemetryLabel.MatchString(t.Harness) {
		return fmt.Errorf("invalid telemetry harness")
	}
	switch t.UsageSource {
	case UsageSourceNone, UsageSourceSessionLog, UsageSourceMock:
	default:
		return fmt.Errorf("invalid telemetry usage source")
	}
	if t.StartedAt.IsZero() {
		return fmt.Errorf("telemetry start time is required")
	}
	for _, v := range []*int64{t.ElapsedMS, t.SetupMS, t.PreflightMS, t.SnapshotMS, t.ModelMS, t.ToolMS,
		t.InputTokens, t.OutputTokens, t.CachedInputTokens, t.ToolCalls, t.RepeatedCommands, t.RepairAttempt, t.ReplanCount} {
		if v != nil && (*v < 0 || *v > maxTelemetryValue) {
			return fmt.Errorf("telemetry value out of range")
		}
	}
	for _, v := range []*string{t.ActualModel, t.Effort} {
		if v != nil && Label(*v) == nil {
			return fmt.Errorf("invalid telemetry label")
		}
	}
	return nil
}

// Encode returns the bounded metadata value for a valid observation.
func (t *RoleRunTelemetry) Encode() (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	if len(data) > MaxRoleRunTelemetry {
		return "", fmt.Errorf("telemetry exceeds %d bytes", MaxRoleRunTelemetry)
	}
	return string(data), nil
}

// ParseRoleRunTelemetry decodes and validates an untrusted metadata value.
func ParseRoleRunTelemetry(value string) (*RoleRunTelemetry, error) {
	if len(value) > MaxRoleRunTelemetry {
		return nil, fmt.Errorf("telemetry exceeds %d bytes", MaxRoleRunTelemetry)
	}
	var t RoleRunTelemetry
	if err := json.Unmarshal([]byte(value), &t); err != nil {
		return nil, fmt.Errorf("decode telemetry: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}
