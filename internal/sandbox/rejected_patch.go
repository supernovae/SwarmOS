package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// RecoverableImport is implemented by import-policy errors whose rejected
// paths are only other roles' outputs, so the rest of the run may be
// recovered by a maintainer. The sandbox itself stays policy-free.
type RecoverableImport interface {
	RecoverableImport() bool
}

// preserveRejected hands the coordinator a patch of a rejected run before the
// sandbox deletes it. A nil patch means no complete patch could be built. The
// import error is always returned unchanged; nothing reaches the worktree.
func preserveRejected(ctx context.Context, opts WorkerOptions, input, result string, importErr error) error {
	var recoverable RecoverableImport
	if opts.PreserveRejected == nil || !errors.As(importErr, &recoverable) || !recoverable.RecoverableImport() {
		return importErr
	}
	patchCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer stop()
	patch, err := rejectedPatch(patchCtx, input, result)
	if err != nil {
		patch = nil
	}
	opts.PreserveRejected(ctx, importErr, patch)
	return importErr
}

func rejectedPatch(ctx context.Context, input, result string) ([]byte, error) {
	baseline, err := os.MkdirTemp(filepath.Dir(input), "rejected-baseline-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(baseline)
	if _, err := unpackWorker(ctx, input, baseline); err != nil {
		return nil, err
	}
	before, err := timeoutFiles(ctx, baseline)
	if err != nil {
		return nil, err
	}
	after, err := timeoutFiles(ctx, result)
	if err != nil {
		return nil, err
	}
	patch, omitted, err := buildPatch(ctx, before, after)
	if err != nil {
		return nil, err
	}
	// A partial patch would silently drop work, so it is not offered.
	if omitted > 0 || len(patch) == 0 {
		return nil, errors.New("incomplete patch")
	}
	return patch, nil
}
