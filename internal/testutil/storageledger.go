package testutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// CheckStorageLedger confines a resolved ledger to a private package root.
// It never opens the ledger or the operator's cache directory.
func CheckStorageLedger(root, control string, resolveErr error) error {
	if resolveErr != nil {
		return resolveErr
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return err
	}
	// Resolve existing ancestors to catch symlink escapes without requiring the
	// read-only resolver to create its destination.
	path, err := filepath.Abs(control)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return fmt.Errorf("storage ledger escapes private package root")
	}
	tail := []string{}
	for {
		resolved, e := filepath.EvalSymlinks(path)
		if e == nil {
			path = resolved
			break
		}
		if !os.IsNotExist(e) {
			return e
		}
		parent := filepath.Dir(path)
		if parent == path {
			return e
		}
		tail = append(tail, filepath.Base(path))
		path = parent
	}
	for i := len(tail) - 1; i >= 0; i-- {
		path = filepath.Join(path, tail[i])
	}
	rel, err = filepath.Rel(base, path)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return fmt.Errorf("storage ledger escapes private package root")
	}
	return nil
}
