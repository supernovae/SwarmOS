package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

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

// NativeValidationReusesCache reports whether native validation candidates
// under cfg can reuse a warmed validator cache.
func NativeValidationReusesCache(cfg *contract.SwarmConfig) bool {
	if nativeValidationUsesCodex(cfg) {
		return ValidatorCacheReusable(ValidationCodex, false)
	}
	return ValidatorCacheReusable(ValidationBubblewrap, bubblewrapOverlay())
}

// RunConfiguredNativeValidation runs command with the validator's caches.
// warmup is true only for the trusted approved base; see ValidatorCacheMounts.
// deps are mounted read-only in every run.
func RunConfiguredNativeValidation(ctx context.Context, dir string, cfg *contract.SwarmConfig, command string, caches []BuildCache, deps Dependencies, warmup bool, stdout, stderr io.Writer) error {
	if nativeValidationUsesCodex(cfg) {
		binary, err := NativeValidationBinary(cfg)
		if err != nil {
			return err
		}
		return RunNativeValidation(ctx, dir, binary, command, caches, deps, warmup, stdout, stderr)
	}
	if err := CheckToolExecutor(ctx); err != nil {
		return err
	}
	return withNativeWorkspace(ctx, dir, WorkerOptions{DiscardChanges: true}, func(ctx context.Context, work, scratch string) error {
		mounts, err := ValidatorCacheMounts(caches, ValidationBubblewrap, warmup, !warmup && bubblewrapOverlay(), scratch)
		if err != nil {
			return err
		}
		return (ToolExecutor{Work: work, BuildCaches: mounts, Dependencies: deps}).Run(ctx, command, stdout, stderr)
	})
}

// bubblewrapOverlay reports, once per process, whether the installed
// Bubblewrap and kernel can mount a read-only cache with a discarded write
// layer. Replaced in tests.
var bubblewrapOverlay = sync.OnceValue(func() bool { return probeBubblewrapOverlay(context.Background()) })

// probeBubblewrapOverlay runs the real executor with an overlay cache. It
// passes only if the cache is readable, writable inside, and unchanged after.
func probeBubblewrapOverlay(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("", "swarm-overlay-check-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(root)
	work, lower := filepath.Join(root, "workspace"), filepath.Join(root, "cache")
	for _, dir := range []string{work, lower} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return false
		}
	}
	if err := os.WriteFile(filepath.Join(lower, "warm"), []byte("warm"), 0o600); err != nil {
		return false
	}
	e := ToolExecutor{Work: work, BuildCaches: []BuildCache{{Ecosystem: "go", EnvKey: "GOCACHE", Dir: lower, Persistent: true, Overlay: true}}}
	if err := e.Run(ctx, `set -eu; test -f "$GOCACHE/warm"; printf x > "$GOCACHE/written"`, io.Discard, io.Discard); err != nil {
		return false
	}
	_, err = os.Lstat(filepath.Join(lower, "written"))
	return os.IsNotExist(err)
}
