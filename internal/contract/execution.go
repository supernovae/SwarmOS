package contract

import (
	"fmt"
	"strings"
)

const (
	ExecutionIsolated     = "isolated"
	ExecutionTrustedLocal = "trusted-local"
	BackendNative         = "native"
	BackendOCI            = "oci"
)

// ExecutionConfig selects the default worker boundary. RequireIsolation is the
// repository minimum; an operator may always request a stronger boundary.
type ExecutionConfig struct {
	AgentTimeout     *string           `yaml:"agent_timeout,omitempty" json:"agent_timeout,omitempty"`
	RoleTimeouts     map[string]string `yaml:"role_timeouts,omitempty" json:"role_timeouts,omitempty"`
	Mode             string            `yaml:"mode,omitempty" json:"mode,omitempty"`
	Backend          string            `yaml:"backend,omitempty" json:"backend,omitempty"`
	ContainerRuntime string            `yaml:"container_runtime,omitempty" json:"container_runtime,omitempty"`
	ContainerImage   string            `yaml:"container_image,omitempty" json:"container_image,omitempty"`
}

func ResolveExecution(cfg *SwarmConfig, operatorMode string) (ExecutionConfig, error) {
	if cfg == nil {
		return ExecutionConfig{}, fmt.Errorf("execution requires approved repository policy")
	}
	e := cfg.Execution
	if err := validateAgentTimeouts(e); err != nil {
		return e, err
	}
	for _, mode := range []string{e.Mode, operatorMode} {
		if mode != "" && mode != ExecutionIsolated && mode != ExecutionTrustedLocal {
			return e, fmt.Errorf("execution mode %q must be isolated or trusted-local", mode)
		}
	}
	if operatorMode != "" {
		e.Mode = operatorMode
	}
	if e.Mode == "" {
		e.Mode = ExecutionIsolated
	}
	if cfg.Security.RequireIsolation && e.Mode != ExecutionIsolated {
		return e, fmt.Errorf("approved repository policy requires isolated execution; trusted-local is prohibited")
	}
	if e.Backend == "" {
		e.Backend = BackendNative
		// Existing repository policies that explicitly configure a container
		// retain that boundary when upgrading from the original OCI-only path.
		if e.ContainerImage != "" || e.ContainerRuntime != "" {
			e.Backend = BackendOCI
		}
	}
	if e.Backend != BackendNative && e.Backend != BackendOCI {
		return e, fmt.Errorf("execution.backend must be native or oci")
	}
	if e.Backend == BackendNative && (e.ContainerImage != "" || e.ContainerRuntime != "") {
		return e, fmt.Errorf("native execution conflicts with container requirements; select execution.backend: oci or remove the container settings")
	}
	if e.ContainerRuntime == "" && e.Backend == BackendOCI {
		e.ContainerRuntime = "docker"
	}
	if e.ContainerRuntime != "" && e.ContainerRuntime != "docker" && e.ContainerRuntime != "podman" {
		return e, fmt.Errorf("execution.container_runtime must be docker or podman")
	}
	if strings.ContainsAny(e.ContainerImage, "\r\n\x00") {
		return e, fmt.Errorf("invalid execution container image")
	}
	return e, nil
}
