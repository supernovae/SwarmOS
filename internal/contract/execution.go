package contract

import (
	"fmt"
	"strings"
)

const (
	ExecutionIsolated     = "isolated"
	ExecutionTrustedLocal = "trusted-local"
)

// ExecutionConfig selects the default worker boundary. RequireIsolation is the
// repository minimum; an operator may always request a stronger boundary.
type ExecutionConfig struct {
	Mode             string `yaml:"mode,omitempty" json:"mode,omitempty"`
	ContainerRuntime string `yaml:"container_runtime,omitempty" json:"container_runtime,omitempty"`
	ContainerImage   string `yaml:"container_image,omitempty" json:"container_image,omitempty"`
}

func ResolveExecution(cfg *SwarmConfig, operatorMode string) (ExecutionConfig, error) {
	if cfg == nil {
		return ExecutionConfig{}, fmt.Errorf("execution requires approved repository policy")
	}
	e := cfg.Execution
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
	if e.ContainerRuntime == "" {
		e.ContainerRuntime = "docker"
	}
	if e.ContainerRuntime != "docker" && e.ContainerRuntime != "podman" {
		return e, fmt.Errorf("execution.container_runtime must be docker or podman")
	}
	if strings.ContainsAny(e.ContainerImage, "\r\n\x00") {
		return e, fmt.Errorf("invalid execution container image")
	}
	return e, nil
}
