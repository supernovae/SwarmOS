package coordination

import (
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"io"
	"path/filepath"

	"github.com/supernovae/swarmos/internal/safefs"
)

// InventoryPreviewStore is optional. A missing capability never permits an
// inventory caller to fall back to the larger or creating Store.Load path.
type InventoryPreviewStore interface {
	LoadInventoryPreview(context.Context, int, int64, func(int64) error) ([]byte, error)
}

// LoadInventoryPreview reads atomically replaced local authority without locks.
// It reserves raw input and parser space before reading; callers still validate
// the projected state identity and treat failures as unknown retention.
func (s *FileStore) LoadInventoryPreview(ctx context.Context, n int, maxBytes int64, reserve func(int64) error) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n < 1 || maxBytes < 1 || maxBytes > 1<<20 || reserve == nil {
		return nil, fmt.Errorf("invalid inventory preview budget")
	}
	path, err := filepath.Abs(s.repoRoot)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(path) + string(filepath.Separator)
	root, err := safefs.Open(volume)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rel, err := filepath.Rel(volume, filepath.Join(path, ".swarm", "coordination", fmt.Sprintf("issue-%d.json", n)))
	if err != nil {
		return nil, err
	}
	f, err := root.OpenRegular(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() < 0 || before.Size() > maxBytes {
		return nil, fmt.Errorf("inventory state exceeds limit")
	}
	if err = reserve(8192 + 128*before.Size()); err != nil {
		return nil, err
	}
	// The file descriptor is pinned. Read at most the observed size plus one,
	// rejecting replacement/growth instead of allocating beyond the reservation.
	data := make([]byte, before.Size()+1)
	used := 0
	for used < len(data) {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		n, e := f.Read(data[used:])
		used += n
		if e != nil {
			if e != io.EOF {
				return nil, e
			}
			break
		}
	}
	if int64(used) != before.Size() {
		return nil, fmt.Errorf("inventory state changed")
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("inventory state changed")
	}
	return data[:used], nil
}

// LoadInventoryObject reads a coordinator-pinned ledger object already present
// locally. It never fetches or resolves a candidate-supplied symbolic ref.
func (s *GitStore) LoadInventoryObject(ctx context.Context, sha string, reserve func(int64) error) ([]byte, error) {
	if !inventorySHA(sha) || reserve == nil {
		return nil, fmt.Errorf("invalid inventory object identity")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := reserve(65536); err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := gitcredential.ObservationCommand(child, s.root, "show", sha+":state.json")
	if err != nil {
		return nil, err
	}
	out := &inventoryOutput{remaining: 128 << 10, reserve: reserve, cancel: cancel}
	cmd.Stdout = out
	// A shared pipe serializes shared-budget admissions from both streams.
	// Diagnostics mixed with state fail subsequent JSON validation.
	cmd.Stderr = out
	if err = cmd.Run(); err != nil {
		return nil, fmt.Errorf("pinned inventory object unavailable: %w", err)
	}
	return out.data, nil
}
func inventorySHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

type inventoryOutput struct {
	data      []byte
	remaining int
	reserve   func(int64) error
	cancel    context.CancelFunc
}

func (w *inventoryOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.cancel()
		return 0, fmt.Errorf("inventory Git output limit")
	}
	if err := w.reserve(128 * int64(len(p))); err != nil {
		w.cancel()
		return 0, err
	}
	w.remaining -= len(p)
	w.data = append(w.data, p...)
	return len(p), nil
}
