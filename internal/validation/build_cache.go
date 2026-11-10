package validation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/gitcredential"
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

// Duration history is coordinator-only state, never a candidate cache mount.
type durationHistory struct {
	Version int             `json:"version"`
	Samples []time.Duration `json:"samples"`
}

func durationKey(name, command string) string {
	b, _ := json.Marshal([]string{name, command})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func durationRoot(ctx context.Context, repo string) (*safefs.Root, error) {
	common, err := gitcredential.CommonDirectory(ctx, repo)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(strings.TrimSpace(string(common)))
	if err != nil || !filepath.IsAbs(canonical) {
		return nil, errors.New("invalid repository identity")
	}
	sum := sha256.Sum256([]byte(filepath.Clean(canonical)))
	cache, err := userCacheDir()
	if err != nil || cache == "" {
		return nil, errors.New("duration cache unavailable")
	}
	canonicalCache, err := filepath.EvalSymlinks(cache)
	if err != nil {
		return nil, err
	}
	location := filepath.Join(canonicalCache, "swarm", "validate-duration", hex.EncodeToString(sum[:16]))
	worktrees, err := gitOutputContext(ctx, repo, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	for _, field := range strings.Split(string(worktrees), "\x00") {
		if work, ok := strings.CutPrefix(field, "worktree "); ok {
			work, err = filepath.EvalSymlinks(work)
			if err != nil {
				return nil, err
			}
			relative, err := filepath.Rel(work, location)
			if err != nil || relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
				return nil, errors.New("duration cache overlaps worktree")
			}
		}
	}
	// Pin the cache directory, then reject symlinks at every component below it.
	root, err := safefs.Open(cache)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Sub(filepath.Join("swarm", "validate-duration", hex.EncodeToString(sum[:16])), true, 0700)
}
func readDurations(root *safefs.Root, key string) durationHistory {
	var h durationHistory
	data, err := root.ReadFile(key+".json", 4096)
	if err != nil || json.Unmarshal(data, &h) != nil || h.Version != 1 || len(h.Samples) > 20 {
		return durationHistory{Version: 1}
	}
	for _, d := range h.Samples {
		if d <= 0 || d > maxCheckTimeout {
			return durationHistory{Version: 1}
		}
	}
	return h
}
func requiredCheckBudget(ctx context.Context, repo string, check contract.CheckSpec) time.Duration {
	budget := checkTimeout(check.Timeout)
	root, err := durationRoot(ctx, repo)
	if err != nil {
		return budget
	}
	defer root.Close()
	for _, d := range readDurations(root, durationKey(check.Name, check.Command)).Samples {
		// Bound before multiplying to avoid overflow.
		budget = max(budget, min(d, maxCheckTimeout/2)*2)
	}
	return min(budget, maxCheckTimeout)
}
func recordRequiredDurations(ctx context.Context, repo string, outcomes []CheckRunOutcome) {
	if ctx.Err() != nil {
		return
	}
	for _, c := range outcomes {
		if !c.Passed {
			return
		}
	}
	root, err := durationRoot(ctx, repo)
	if err != nil {
		return
	}
	defer root.Close()
	for _, c := range outcomes {
		if !c.Passed || c.Duration <= 0 || c.Duration > maxCheckTimeout {
			continue
		}
		key := durationKey(c.Name, c.Command)
		lock, err := root.OpenLock(key + ".lock")
		if err != nil {
			continue
		}
		// Bounded, cancellable lock acquisition serializes read/merge/write.
		lockCtx, cancel := context.WithTimeout(ctx, time.Second)
		err = lockDurationHistory(lockCtx, lock)
		cancel()
		if err != nil {
			lock.Close()
			continue
		}
		h := readDurations(root, key)
		h.Samples = append(h.Samples, c.Duration)
		if len(h.Samples) > 20 {
			h.Samples = h.Samples[len(h.Samples)-20:]
		}
		data, err := json.Marshal(h)
		if err == nil && ctx.Err() == nil {
			_ = root.WriteFile(key+".json", data, 0600)
		}
		_ = filelock.Unlock(lock)
		_ = lock.Close()
	}
}

func lockDurationHistory(ctx context.Context, lock *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := filelock.TryLockExclusive(lock); err == nil {
			return nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
