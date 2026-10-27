package validation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
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

// warmBuildCaches runs the trusted required checks against a clean checkout
// of the approved base, once per base. It is the only run that writes the
// persistent validator caches; candidate runs reuse them read-only or not at
// all (sandbox.ValidatorCacheMounts). Warm-up is best-effort: it never changes
// the candidate's verdict. The returned note is added to the report.
func warmBuildCaches(ctx context.Context, r *checkRunner, checks []contract.CheckSpec, baseSHA string) string {
	keyDir := ""
	for _, c := range r.buildCaches {
		if c.Persistent {
			keyDir = filepath.Dir(c.Dir)
			break
		}
	}
	if keyDir == "" || len(checks) == 0 {
		return ""
	}
	marker := ".warm-" + baseSHA
	if _, err := os.Lstat(filepath.Join(keyDir, marker)); !errors.Is(err, os.ErrNotExist) {
		return ""
	}
	tmp, err := os.MkdirTemp("", "swarm-warmup-*")
	if err != nil {
		return fmt.Sprintf("\nBuild cache warm-up skipped: %v\n", err)
	}
	defer os.RemoveAll(tmp)
	checkout := filepath.Join(tmp, "base")
	if _, err := gitOutputContext(ctx, r.worktree, "worktree", "add", "--detach", "--", checkout, baseSHA); err != nil {
		return fmt.Sprintf("\nBuild cache warm-up skipped: %v\n", err)
	}
	defer gitOutput(r.worktree, "worktree", "remove", "--force", "--", checkout)
	warm := *r
	warm.worktree, warm.warmup = checkout, true
	var failed []string
	for _, check := range checks {
		if _, err := warm.runCheck(ctx, check.Name, check.Command, checkTimeout(check.Timeout), nil); err != nil {
			failed = append(failed, "`"+check.Name+"`")
		}
	}
	if ctx.Err() != nil {
		return ""
	}
	// Record completion even when a base check failed, so a broken base is
	// not rebuilt by every later validation.
	if err := safefs.WriteFile(keyDir, marker, nil, 0o600); err != nil {
		return fmt.Sprintf("\nBuild cache warm-up marker not written: %v\n", err)
	}
	note := fmt.Sprintf("\nWarmed the validator build cache from trusted base `%s`.\n", baseSHA)
	if len(failed) > 0 {
		note += fmt.Sprintf("Warm-up check(s) %s failed on the base; this does not affect the validation result.\n", strings.Join(failed, ", "))
	}
	return note
}
