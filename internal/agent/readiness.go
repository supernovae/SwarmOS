package agent

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// CheckConfiguredReadiness selects the same offline tool boundary as the
// configured worker. Inspection-only callers supply a reason to skip execution.
func CheckConfiguredReadiness(ctx context.Context, projects []sandbox.ReadinessProject, name, role string, cfg *contract.SwarmConfig, local bool, options RunnerOptions, deps sandbox.Dependencies, deferred string) ([]sandbox.ReadinessRow, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	execution, err := contract.ResolveExecution(cfg, getenvWith(options.LookupEnv, "SWARM_EXECUTION_MODE"))
	if err != nil {
		return nil, err
	}
	driver, _, _ := ConfiguredMetadata(name, cfg)
	boundary := execution.Backend
	if execution.Backend == contract.BackendNative {
		switch driver {
		case "codex":
			boundary = "codex-native"
		case "opencode":
			boundary = "harness-tools-bwrap"
		}
	}
	if deferred == "" && (runtime.GOOS != "linux" || execution.Mode != contract.ExecutionIsolated ||
		(execution.Backend == contract.BackendNative && driver != "codex" && driver != "opencode")) {
		deferred = "readiness probes are available for Linux isolated Codex, OpenCode or offline OCI workers"
	}
	if execution.Backend == contract.BackendOCI {
		worker := cfg.Agents[name]
		runtimeName, image := execution.ContainerRuntime, execution.ContainerImage
		if worker.ContainerRuntime != "" {
			runtimeName = worker.ContainerRuntime
		}
		if worker.ContainerImage != "" {
			image = worker.ContainerImage
		}
		if runtimeName == "" {
			runtimeName = "docker"
		}
		boundary = "oci:" + runtimeName + "/" + image
	}
	failedSelection := func(err error) ([]sandbox.ReadinessRow, error) {
		rows, _ := sandbox.CheckReadiness(ctx, projects, role, name, boundary, err.Error(), nil)
		for i := range rows {
			rows[i].Required = true
			if rows[i].Check == "toolchain" {
				rows[i].Status = "failed"
			}
		}
		return rows, err
	}
	if deferred != "" {
		return sandbox.CheckReadiness(ctx, projects, role, name, boundary, deferred, nil)
	}
	runner, err := NewConfiguredRunnerWithOptions(name, cfg, local, options)
	if err != nil {
		return failedSelection(err)
	}
	configured := runner.(*configuredRunner)
	if err := configured.validate(RunRequest{Role: role}); err != nil {
		return failedSelection(err)
	}
	a := configured.Runner.(*CLIAdapter)
	worker, err := a.isolatedWorkerOptions(RunRequest{Role: role, Dependencies: deps})
	if err != nil {
		return failedSelection(fmt.Errorf("readiness worker options: %w", err))
	}
	worker.DiscardChanges = true
	run := func(ctx context.Context, dir, command string, stdout, stderr io.Writer) error {
		switch a.sandboxRuntime {
		case "codex-native":
			return sandbox.RunNativeReadiness(ctx, dir, a.binary, command, worker, stdout, stderr)
		case "harness-tools-bwrap":
			return sandbox.RunDelegatedReadiness(ctx, dir, command, worker, stdout, stderr)
		case "container":
			return sandbox.RunWorker(ctx, dir, "/bin/sh", []string{"-c", command}, worker, nil, stdout, stderr)
		default:
			return fmt.Errorf("readiness boundary unavailable for %s", name)
		}
	}
	return sandbox.CheckReadiness(ctx, projects, role, name, boundary, "", run)
}
