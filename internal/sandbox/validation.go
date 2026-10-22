package sandbox

import (
	"context"
	"io"

	"github.com/supernovae/swarmos/internal/contract"
)

func nativeValidationUsesCodex(cfg *contract.SwarmConfig) bool {
	name := cfg.Roles["tester"]
	if name == "" {
		name = cfg.DefaultAgent
	}
	ac, ok := cfg.Agents[name]
	return ok && (ac.Harness == "codex" || ac.Harness == "" && name == "codex")
}

func CheckNativeValidation(ctx context.Context, cfg *contract.SwarmConfig) error {
	if nativeValidationUsesCodex(cfg) {
		binary, err := NativeValidationBinary(cfg)
		if err != nil {
			return err
		}
		return CheckNativeCodex(ctx, binary)
	}
	return CheckToolExecutor(ctx)
}

func RunConfiguredNativeValidation(ctx context.Context, dir string, cfg *contract.SwarmConfig, command string, stdout, stderr io.Writer) error {
	if nativeValidationUsesCodex(cfg) {
		binary, err := NativeValidationBinary(cfg)
		if err != nil {
			return err
		}
		return RunNativeValidation(ctx, dir, binary, command, stdout, stderr)
	}
	if err := CheckToolExecutor(ctx); err != nil {
		return err
	}
	return withNativeWorkspace(ctx, dir, WorkerOptions{DiscardChanges: true}, func(ctx context.Context, work, _ string) error {
		return (ToolExecutor{Work: work}).Run(ctx, command, stdout, stderr)
	})
}
