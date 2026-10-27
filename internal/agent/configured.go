package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// RunnerOptions carries one invocation's environment into configured runners.
// A nil LookupEnv reads the process environment.
type RunnerOptions struct {
	LookupEnv func(string) (string, bool)
	// Inspections reuses harness interface inspection within one command,
	// partitioned by the approved config. Nil inspects on every call.
	Inspections *sandbox.InspectionSession
}

// NewConfiguredRunner creates only an explicitly allowlisted agent. It performs
// executable availability checks before a caller can claim work.
func NewConfiguredRunner(name string, cfg *contract.SwarmConfig, local bool) (Runner, error) {
	return NewConfiguredRunnerWithOptions(name, cfg, local, RunnerOptions{})
}

// NewConfiguredRunnerWithOptions is NewConfiguredRunner reading execution mode,
// allowlisted model variables and mock controls through opts.LookupEnv.
func NewConfiguredRunnerWithOptions(name string, cfg *contract.SwarmConfig, local bool, opts RunnerOptions) (Runner, error) {
	if cfg == nil {
		return nil, fmt.Errorf("agent policy is required")
	}
	execution, err := contract.ResolveExecution(cfg, getenvWith(opts.LookupEnv, "SWARM_EXECUTION_MODE"))
	if err != nil {
		return nil, err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil, fmt.Errorf("agent name is required")
	}
	if name == "mock" {
		if execution.Mode == contract.ExecutionIsolated {
			return nil, fmt.Errorf("mock runs host test helpers and requires explicit trusted-local execution")
		}
		_, allowlisted := cfg.Agents["mock"]
		if !local && !allowlisted {
			return nil, fmt.Errorf("mock runner is available only with the local backend")
		}
		return &configuredRunner{Runner: &MockRunner{lookupEnv: opts.LookupEnv}, name: name}, nil
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
	if err := contract.ValidateAgentEffort(name, ac); err != nil {
		return nil, err
	}
	a := &CLIAdapter{agentName: name, driver: driver, binary: ac.Binary, prefixArgs: append([]string(nil), ac.Args...), model: ac.Model, effort: ac.Effort, envAllow: append([]string(nil), ac.Env...), sandboxRuntime: ac.Sandbox, reviewNetwork: ac.ReviewNetwork, containerRuntime: ac.ContainerRuntime, container: ac.ContainerImage, lookupEnv: opts.LookupEnv}
	a.executionMode = execution.Mode
	a.executionPolicy = execution
	a.authentication = ac.Authentication
	if opts.Inspections != nil {
		if a.inspectionPolicy, err = sandbox.InspectionPolicy(cfg); err != nil {
			return nil, err
		}
		a.inspections = opts.Inspections
	}
	if execution.Mode == contract.ExecutionIsolated && execution.Backend == contract.BackendNative {
		if driver != "codex" && driver != "claude" && driver != "opencode" && driver != "omp" {
			return nil, fmt.Errorf("agent %q: no verified isolated adapter for native %s; choose a qualified Codex agent or explicitly configure execution.backend: oci", name, driver)
		}
		if len(ac.Args) != 0 || len(ac.Env) != 0 || ac.ReviewNetwork || ac.Sandbox != "" || ac.ContainerRuntime != "" || ac.ContainerImage != "" || (ac.Authentication != "" && ac.Authentication != "auto") {
			return nil, fmt.Errorf("isolated harnesses use harness-owned login and coordinator-selected permissions; remove args, env, sandbox, container and authentication overrides from agent %q", name)
		}
		a.sandboxRuntime = "codex-native"
		if driver != "codex" {
			a.sandboxRuntime = "harness-tools-bwrap"
		}
		a.authentication = "harness"
	}
	if execution.Mode == contract.ExecutionIsolated && execution.Backend == contract.BackendOCI {
		a.sandboxRuntime = "container"
		a.containerRuntime, a.container = execution.ContainerRuntime, execution.ContainerImage
		if ac.ContainerRuntime != "" {
			if ac.ContainerRuntime != "docker" && ac.ContainerRuntime != "podman" {
				return nil, fmt.Errorf("agent %q container_runtime must be docker or podman, not a worker-controlled executable path", name)
			}
			a.containerRuntime = ac.ContainerRuntime
		}
		if ac.ContainerImage != "" {
			a.container = ac.ContainerImage
		}
		if ac.ReviewNetwork {
			return nil, fmt.Errorf("agent %q: isolated execution does not permit unrestricted review_network", name)
		}
		if len(ac.Env) != 0 {
			return nil, fmt.Errorf("agent %q: OCI execution does not forward host environment credentials; authenticated OCI support is not yet available", name)
		}
		if ac.Authentication != "none" {
			return nil, fmt.Errorf("agent %q (%s): authenticated OCI execution is not yet supported; use native Codex for existing login, or an offline OCI image with authentication: none", name, driver)
		}
	}
	if a.sandboxRuntime != "container" {
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
	return PreflightWithOptions(name, cfg, local, role, RunnerOptions{})
}

// PreflightWithOptions is Preflight for a runner built with opts.
func PreflightWithOptions(name string, cfg *contract.SwarmConfig, local bool, role string, opts RunnerOptions) (string, error) {
	r, err := NewConfiguredRunnerWithOptions(name, cfg, local, opts)
	if err != nil {
		return "", err
	}
	cr := r.(*configuredRunner)
	if err := cr.validate(RunRequest{Role: role, Model: cr.model, Local: local}); err != nil {
		return "", err
	}
	if adapter, ok := cr.Runner.(*CLIAdapter); ok && adapter.sandboxRuntime == "codex-native" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		version, err := adapter.inspections.InspectNativeCodex(ctx, adapter.binary, adapter.inspectionPolicy)
		if err != nil {
			return "", err
		}
		auth, err := sandbox.CodexAuthentication(ctx, adapter.binary)
		if err != nil {
			return "", err
		}
		return "native:codex " + version + "; " + auth + " present; filesystem/network canaries passed; MCP, plugins and hooks disabled; interactive and unattended", nil
	}
	if adapter, ok := cr.Runner.(*CLIAdapter); ok && adapter.sandboxRuntime == "harness-tools-bwrap" {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		version, err := adapter.inspections.InspectDelegatedHarness(ctx, adapter.driver, adapter.binary, adapter.inspectionPolicy)
		if err != nil {
			return "", err
		}
		if err := sandbox.CheckToolExecutor(ctx); err != nil {
			return "", err
		}
		auth := "provider login: harness-managed, not probed (no model request)"
		if adapter.driver == "claude" {
			auth, err = sandbox.ClaudeAuthentication(ctx, adapter.binary)
			if err != nil {
				return "", err
			}
		}
		model := "harness default model"
		if cr.model != "" {
			model = "project model=" + cr.model
		}
		return fmt.Sprintf("%s %s; Linux isolated tools (Bubblewrap); %s; %s; workspace read/edit/write and shell; filesystem/network/read-only canaries passed; tool policy checked at launch; external MCP, hooks, plugins and subagents restricted; interactive and unattended", adapter.driver, version, auth, model), nil
	}
	if adapter, ok := cr.Runner.(*CLIAdapter); ok && adapter.sandboxRuntime == "container" {
		if adapter.executionMode == contract.ExecutionIsolated {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := sandbox.CheckWorkerRuntime(ctx, adapter.containerRuntime, adapter.container); err != nil {
				return "", err
			}
			return "container:" + adapter.container, nil
		}
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
	if adapter, ok := cr.Runner.(*CLIAdapter); ok && adapter.driver == "opencode" {
		version, err := adapter.inspections.InspectDelegatedHarness(context.Background(), adapter.driver, path, adapter.inspectionPolicy)
		if err != nil {
			return "", err
		}
		return path + " (OpenCode " + version + "; trusted-local)", nil
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
