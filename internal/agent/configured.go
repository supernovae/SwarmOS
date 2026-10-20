package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// NewConfiguredRunner creates only an explicitly allowlisted agent. It performs
// executable availability checks before a caller can claim work.
func NewConfiguredRunner(name string, cfg *contract.SwarmConfig, local bool) (Runner, error) {
	if cfg == nil {
		return nil, fmt.Errorf("agent policy is required")
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil, fmt.Errorf("agent name is required")
	}
	if name == "mock" {
		if !local {
			return nil, fmt.Errorf("mock runner is available only with the local backend")
		}
		return &configuredRunner{Runner: &MockRunner{}, name: name}, nil
	}
	ac, ok := cfg.Agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %q is not allowlisted", name)
	}
	if strings.TrimSpace(ac.Binary) == "" {
		return nil, fmt.Errorf("agent %q has no configured binary", name)
	}
	if err := validateAgentEnv(ac.Env); err != nil {
		return nil, fmt.Errorf("agent %q environment policy: %w", name, err)
	}
	driver, err := resolveHarness(name, ac.Harness)
	if err != nil {
		return nil, err
	}
	a := &CLIAdapter{agentName: name, driver: driver, binary: ac.Binary, prefixArgs: append([]string(nil), ac.Args...), model: ac.Model, envAllow: append([]string(nil), ac.Env...), sandboxRuntime: ac.Sandbox, reviewNetwork: ac.ReviewNetwork, containerRuntime: ac.ContainerRuntime, container: ac.ContainerImage}
	if ac.Sandbox != "container" {
		if path, ok := a.Available(); !ok {
			return nil, fmt.Errorf("agent %q binary %q is unavailable", name, ac.Binary)
		} else {
			canonical, err := filepath.EvalSymlinks(path)
			if err != nil {
				return nil, fmt.Errorf("resolve agent %q binary %q: %w", name, ac.Binary, err)
			}
			if !filepath.IsAbs(canonical) {
				return nil, fmt.Errorf("agent %q resolved non-absolute binary %q", name, canonical)
			}
			a.binary = canonical
		}
	}
	return &configuredRunner{Runner: a, name: name, roles: allowedRoles(ac.Roles), model: ac.Model}, nil
}

// Preflight resolves and validates the selected configured agent before leasing work.
func Preflight(name string, cfg *contract.SwarmConfig, local bool, role string) (string, error) {
	r, err := NewConfiguredRunner(name, cfg, local)
	if err != nil {
		return "", err
	}
	cr := r.(*configuredRunner)
	if err := cr.validate(RunRequest{Role: role, Model: cr.model, Local: local}); err != nil {
		return "", err
	}
	if adapter, ok := cr.Runner.(*CLIAdapter); ok && adapter.sandboxRuntime == "container" {
		if !strings.EqualFold(role, "reviewer") {
			return "", fmt.Errorf("container-isolated agent %q is supported only for reviewer role", name)
		}
		if err := sandbox.CheckReadOnlyRuntime(adapter.sandboxRuntime, adapter.containerRuntime, adapter.container); err != nil {
			return "", err
		}
		return "container:" + adapter.container, nil
	}
	if strings.EqualFold(role, "reviewer") {
		if adapter, ok := cr.Runner.(*CLIAdapter); ok {
			if err := sandbox.CheckReadOnlyRuntime(adapter.sandboxRuntime, adapter.containerRuntime, adapter.container); err != nil {
				return "", err
			}
		}
	}
	path, ok := r.Available()
	if !ok {
		return "", fmt.Errorf("agent %q became unavailable", name)
	}
	return path, nil
}

type configuredRunner struct {
	Runner
	name, model string
	roles       map[string]struct{}
}

func allowedRoles(roles []string) map[string]struct{} {
	m := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		if role = strings.ToLower(strings.TrimSpace(role)); role != "" {
			m[role] = struct{}{}
		}
	}
	return m
}
func (r *configuredRunner) validate(req RunRequest) error {
	role := strings.ToLower(strings.TrimSpace(req.Role))
	if role == "" {
		return fmt.Errorf("agent role is required")
	}
	if len(r.roles) != 0 {
		if _, ok := r.roles[role]; !ok {
			return fmt.Errorf("agent %q is not permitted for role %q", r.name, role)
		}
	}
	if r.model != "" && req.Model != "" && req.Model != r.model {
		return fmt.Errorf("agent %q does not permit model %q", r.name, req.Model)
	}
	return nil
}
func (r *configuredRunner) Provenance(role string, readOnly bool) contract.RoleAttestation {
	att := r.Runner.Provenance(role, readOnly)
	if r.name != "" {
		att.Agent = r.name
	}
	if (att.Model == "" || att.Model == "default") && strings.TrimSpace(r.model) != "" {
		att.Model = strings.TrimSpace(r.model)
	}
	if att.Model == "" {
		att.Model = "default"
	}
	return att
}

func (r *configuredRunner) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	if err := r.validate(req); err != nil {
		return nil, err
	}
	if req.Model == "" {
		req.Model = r.model
	}
	res, err := r.Runner.Run(ctx, req)
	if res != nil {
		if r.name != "" {
			res.Attestation.Agent = r.name
		}
		if strings.TrimSpace(req.Model) != "" {
			res.Attestation.Model = strings.TrimSpace(req.Model)
		}
		if res.Attestation.Model == "" {
			res.Attestation.Model = "default"
		}
	}
	return res, err
}

func validateAgentEnv(names []string) error {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("empty variable name")
		}
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "GITHUB_") || strings.HasPrefix(upper, "GH_") || strings.HasPrefix(upper, "SWARM_GITHUB_") || strings.HasPrefix(upper, "GIT_") || strings.HasPrefix(upper, "SSH_") || upper == "HOME" || strings.HasPrefix(upper, "XDG_") {
			return fmt.Errorf("%q is a coordinator or host credential variable, not model authentication", name)
		}
	}
	return nil
}

func resolveHarness(name, harness string) (string, error) {
	driver := strings.ToLower(strings.TrimSpace(harness))
	if driver == "" {
		driver = name
	}
	switch driver {
	case "claude", "codex", "omp", "opencode":
		return driver, nil
	default:
		return "", fmt.Errorf("agent %q uses unsupported harness %q", name, driver)
	}
}
