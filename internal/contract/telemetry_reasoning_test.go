package contract

import (
	"strings"
	"testing"
	"time"
)

func TestTelemetryReasoningRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		value *int64
		want  string
	}{
		{nil, `"reasoning_tokens":null`},
		{Count(0), `"reasoning_tokens":0`},
		{Count(21), `"reasoning_tokens":21`},
	} {
		tel := NewRoleRunTelemetry("developer", "omp", time.Now())
		tel.ReasoningTokens = tc.value
		value, err := tel.Encode()
		if err != nil || !strings.Contains(value, tc.want) {
			t.Fatalf("encoded = %s, %v; want %s", value, err, tc.want)
		}
		parsed, err := ParseRoleRunTelemetry(value)
		if err != nil {
			t.Fatal(err)
		}
		if (parsed.ReasoningTokens == nil) != (tc.value == nil) || tc.value != nil && *parsed.ReasoningTokens != *tc.value {
			t.Fatalf("round trip changed reasoning: %v", parsed.ReasoningTokens)
		}
	}
	// Records written before reasoning existed parse as unavailable.
	legacy := NewRoleRunTelemetry("developer", "claude", time.Now())
	value, err := legacy.Encode()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRoleRunTelemetry(strings.Replace(value, `"reasoning_tokens":null,`, "", 1))
	if err != nil || parsed.ReasoningTokens != nil {
		t.Fatalf("legacy record = %+v, %v", parsed, err)
	}
	for _, bad := range []int64{-1, maxTelemetryValue + 1} {
		tel := NewRoleRunTelemetry("developer", "omp", time.Now())
		tel.ReasoningTokens = Count(bad)
		if tel.Validate() == nil {
			t.Fatalf("accepted reasoning %d", bad)
		}
	}
}
