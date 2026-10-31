package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/storageuse"
)

func nativeValidationUsesCodex(cfg *contract.SwarmConfig) bool {
	name := cfg.Roles["tester"]
	if name == "" {
		name = cfg.DefaultAgent
	}
	ac, ok := cfg.Agents[name]
	return ok && (ac.Harness == "codex" || ac.Harness == "" && name == "codex")
}

// CheckNativeValidationAvailable resolves the configured boundary without
// executing a canary or creating any files.
func CheckNativeValidationAvailable(cfg *contract.SwarmConfig) error {
	if nativeValidationUsesCodex(cfg) {
		_, err := NativeValidationBinary(cfg)
		return err
	}
	return CheckReadOnlyRuntime("", "", "")
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
func probeBubblewrapOverlay(ctx context.Context) (ok bool) {
	ctx, own, err := storageuse.Ensure(ctx, "host")
	if err != nil {
		return false
	}
	if own != nil {
		defer func() {
			if p := recover(); p != nil {
				own.Retain()
				_ = own.Finish()
				panic(p)
			}
			if own.Finish() != nil {
				ok = false
			}
		}()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	root, err := storageuse.FromContext(ctx).TempDir(ctx, "", "swarm-overlay-check-*", "sandbox-scratch")
	if err != nil {
		return false
	}
	defer func() {
		if p := recover(); p != nil {
			storageuse.FromContext(ctx).Retain()
			panic(p)
		}
		var cleanupErr error
		storageuse.FromContext(ctx).CleanupTemp(root, &cleanupErr)
		if cleanupErr != nil {
			ok = false
		}
	}()
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
