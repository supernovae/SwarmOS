package contract

import "testing"

func TestExecutionModesAndRepositoryMinimum(t *testing.T) {
	for _, tc := range []struct {
		mode, local, want string
		required, fail    bool
	}{
		{want: ExecutionIsolated},
		{mode: ExecutionTrustedLocal, want: ExecutionTrustedLocal},
		{mode: ExecutionTrustedLocal, local: ExecutionIsolated, want: ExecutionIsolated},
		{local: ExecutionTrustedLocal, want: ExecutionTrustedLocal},
		{local: ExecutionTrustedLocal, required: true, fail: true},
		{mode: ExecutionTrustedLocal, required: true, fail: true},
		{mode: "community", fail: true},
		{local: "managed-isolated", fail: true},
	} {
		cfg := &SwarmConfig{Execution: ExecutionConfig{Mode: tc.mode}, Security: SecurityConfig{RequireIsolation: tc.required}}
		got, err := ResolveExecution(cfg, tc.local)
		if tc.fail {
			if err == nil {
				t.Fatalf("accepted invalid policy %+v", tc)
			}
			continue
		}
		if err != nil || got.Mode != tc.want {
			t.Fatalf("policy %+v: got %+v, %v", tc, got, err)
		}
	}
}
