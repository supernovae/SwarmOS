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

func TestExecutionBackendSelection(t *testing.T) {
	for _, backend := range []string{"", BackendNative, BackendOCI, "external", "host"} {
		cfg := &SwarmConfig{Execution: ExecutionConfig{Backend: backend}}
		e, err := ResolveExecution(cfg, "")
		if backend == "external" || backend == "host" {
			if err == nil {
				t.Fatalf("accepted unimplemented backend %q", backend)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := backend
		if want == "" {
			want = BackendNative
		}
		if e.Backend != want {
			t.Fatalf("got %q, want %q", e.Backend, want)
		}
	}
}

func TestContainerPolicyCannotBecomeNativeImplicitly(t *testing.T) {
	cfg := &SwarmConfig{Execution: ExecutionConfig{ContainerRuntime: "podman", ContainerImage: "approved-image"}}
	e, err := ResolveExecution(cfg, "")
	if err != nil || e.Backend != BackendOCI {
		t.Fatalf("container requirement lost: %+v, %v", e, err)
	}
	cfg.Execution.Backend = BackendNative
	if _, err := ResolveExecution(cfg, ""); err == nil {
		t.Fatal("accepted conflicting native/container requirements")
	}
}
