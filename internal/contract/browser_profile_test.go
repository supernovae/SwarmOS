package contract

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestBrowserProfileExecution(t *testing.T) {
	for _, tc := range []struct {
		name, mode, backend, operator string
		enabled, fail                 bool
	}{
		{name: "default"},
		{name: "native", enabled: true},
		{name: "explicit", mode: ExecutionIsolated, backend: BackendNative, enabled: true},
		{name: "oci", backend: BackendOCI, enabled: true, fail: true},
		{name: "trusted-local", mode: ExecutionTrustedLocal, enabled: true, fail: true},
		{name: "operator override", operator: ExecutionTrustedLocal, enabled: true, fail: true},
		{name: "disabled oci", backend: BackendOCI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &SwarmConfig{Execution: ExecutionConfig{Mode: tc.mode, Backend: tc.backend, BrowserProfile: tc.enabled}}
			got, err := ResolveExecution(cfg, tc.operator)
			if (err != nil) != tc.fail {
				t.Fatalf("ResolveExecution: %+v, %v", got, err)
			}
			if err == nil && got.BrowserProfile != tc.enabled {
				t.Fatal("lost approved profile")
			}
		})
	}
}

func TestBrowserProfileYAML(t *testing.T) {
	for _, value := range []string{"true", "false", "invalid", "[true]", "{}"} {
		var cfg SwarmConfig
		err := yaml.Unmarshal([]byte("execution:\n  browser_profile: "+value+"\n"), &cfg)
		valid := value == "true" || value == "false"
		if (err == nil) != valid {
			t.Fatalf("value %q: %v", value, err)
		}
		if err == nil && cfg.Execution.BrowserProfile != (value == "true") {
			t.Fatalf("value %q not decoded", value)
		}
	}
}
