package validation

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/progress"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/storageuse"
)

// userCacheDir is replaced by tests so they never touch the real user cache.
var userCacheDir = os.UserCacheDir

// validatorBuildCaches returns <user-cache>/swarm/validate-build/<key>/<eco>
// for each cached ecosystem, or scratch under checkHome when a cache cannot be
// prepared safely. The validator never reads an agent cache.
func validatorBuildCaches(ctx context.Context, worktree, checkHome string) ([]sandbox.BuildCache, error) {
	base, err := userCacheDir()
	if err != nil {
		base = ""
	}
	return sandbox.PrepareBuildCaches(ctx, sandbox.ValidatorBuildCache, worktree, base, checkHome)
}

// validatorDependencyCaches prefetches the approved base's dependencies into
// <user-cache>/swarm/validate-deps/<key>/<eco>. Unlike build caches there is
// no scratch fallback: an error stops validation.
func validatorDependencyCaches(ctx context.Context, worktree, baseSHA string, policy sandbox.DependencyPolicy) (sandbox.Dependencies, error) {
	base, err := userCacheDir()
	if err != nil {
		base = ""
	}
	return sandbox.PrepareDependencyCaches(ctx, sandbox.ValidatorDependencyCache, worktree, baseSHA, base, policy)
}

// warmBuildCaches runs the trusted required checks against a clean checkout
// of the approved base, once per base. It is the only run that writes the
// persistent validator caches; candidate runs reuse them read-only or not at
// all (sandbox.ValidatorCacheMounts). Warm-up is best-effort: it never changes
// the candidate's verdict. The returned note is added to the report.
func warmBuildCaches(ctx context.Context, r *checkRunner, checks []contract.CheckSpec, baseSHA string, timing *PhaseTiming, observe func(progress.Event)) (note string) {
	ctx, owner, err := r.storageScope(ctx)
	if owner != nil {
		var completionErr error
		defer func() {
			if completionErr != nil {
				note += fmt.Sprintf("\nBuild cache use completion failed: %v\n", completionErr)
			}
		}()
		defer owner.Complete(&completionErr)
	}
	if err != nil {
		if ctx.Err() != nil {
			timing.Status = "cancelled"
			return ""
		}
		timing.Status = "skipped-error"
		return fmt.Sprintf("\nBuild cache warm-up skipped: %v\n", err)
	}

	start := time.Now()
	timing.Status = "ran"
	phaseEvent(observe, "warm-up", "started", 0)
	defer func() {
		if ctx.Err() != nil {
			timing.Status = "cancelled"
		}
		timing.Duration = time.Since(start)
		phaseEvent(observe, "warm-up", timing.Status, timing.Duration)
	}()
	keyDir := ""
	for _, c := range r.buildCaches {
		if c.Persistent {
			keyDir = filepath.Dir(c.Dir)
			break
		}
	}
	if keyDir == "" || len(checks) == 0 {
		timing.Status = "no-applicable-cache"
		if keyDir != "" {
			timing.Status = "no-applicable-checks"
		}
		return ""
	}
	scope := storageuse.FromContext(ctx)
	if err := scope.Acquire(ctx, storageuse.ResourceFor(scope.Repository(), "validate-build", keyDir)); err != nil {
		timing.Status = "skipped-error"
		return fmt.Sprintf("\nBuild cache warm-up skipped: %v\n", err)
	}
	marker := ".warm-" + baseSHA
	if _, err := os.Lstat(filepath.Join(keyDir, marker)); !errors.Is(err, os.ErrNotExist) {
		timing.Status = "skipped-marker"
		if err != nil {
			timing.Status = "skipped-error"
		}
		return ""
	}
	tmp, err := storageuse.MkdirTemp(ctx, "", "swarm-warmup-*", "validation-scratch")
	if err != nil {
		timing.Status = "skipped-error"
		return fmt.Sprintf("\nBuild cache warm-up skipped: %v\n", err)
	}
	var cleanupErr error
	defer func() {
		if cleanupErr != nil {
			note += fmt.Sprintf("\nBuild cache warm-up cleanup failed: %v\n", cleanupErr)
		}
	}()
	defer cleanupValidationCheckout(storageuse.FromContext(ctx), r.worktree, tmp, &cleanupErr)
	checkout := filepath.Join(tmp, "base")
	if _, err := gitOutputContext(ctx, r.worktree, "worktree", "add", "--detach", "--", checkout, baseSHA); err != nil {
		timing.Status = "skipped-error"
		return fmt.Sprintf("\nBuild cache warm-up skipped: %v\n", err)
	}

	warm := *r
	warm.worktree, warm.warmup = checkout, true
	var failed []string
	for _, check := range checks {
		if _, err := warm.runCheck(ctx, check.Name, check.Command, checkTimeout(check.Timeout), nil); err != nil {
			failed = append(failed, "`"+check.Name+"`")
		}
	}
	if ctx.Err() != nil {
		timing.Status = "cancelled"
		return ""
	}
	// Record completion even when a base check failed, so a broken base is
	// not rebuilt by every later validation.
	if err := safefs.WriteFile(keyDir, marker, nil, 0o600); err != nil {
		timing.Status = "skipped-error"
		return fmt.Sprintf("\nBuild cache warm-up marker not written: %v\n", err)
	}
	note = fmt.Sprintf("\nWarmed the validator build cache from trusted base `%s`.\n", baseSHA)
	if len(failed) > 0 {
		timing.Status = "failed-checks"
		note += fmt.Sprintf("Warm-up check(s) %s failed on the base; this does not affect the validation result.\n", strings.Join(failed, ", "))
	}
	return note
}

// cleanupValidationCheckout preserves unresolved runs and removes the admitted
// temporary root through its pinned parent before pruning Git's stale entry.
func cleanupValidationCheckout(scope *storageuse.Scope, repo, tmp string, result *error) {
	if p := recover(); p != nil {
		scope.Retain()
		panic(p)
	}
	if scope.Retained() {
		return
	}
	var cleanupErr error
	scope.CleanupTemp(tmp, &cleanupErr)
	*result = errors.Join(*result, cleanupErr)
	if cleanupErr == nil {
		_, err := gitOutput(repo, "worktree", "prune")
		*result = errors.Join(*result, err)
	}
}
