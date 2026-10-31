package storageuse

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

// VisitResources unions existing durable registrations into an inventory. It
// reads one capped record at a time and never follows its Location. The caller
// must separately admit that location under its trusted discovery roots.
func VisitResources(ctx context.Context, path string, maxRecords int, entry func() error, reserve func(int64) error, visit func(Resource) error, includeAbsent ...bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) || maxRecords < 1 || entry == nil || reserve == nil {
		return ErrUnsafe
	}
	volume := filepath.VolumeName(path) + string(filepath.Separator)
	base, err := safefs.Open(volume)
	if err != nil {
		return err
	}
	defer base.Close()
	rel, err := filepath.Rel(volume, path)
	if err != nil {
		return err
	}
	root, err := base.Sub(rel, false, 0)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenDirectory(".")
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return ErrUnsafe
	}
	if err = reserve(16384); err != nil {
		return err
	}
	count := 0
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = entry(); err != nil {
			return err
		}
		entries, e := f.ReadDir(1)
		if len(entries) == 0 {
			if e == io.EOF {
				return nil
			}
			return e
		}
		name := entries[0].Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		if len(key) != 64 {
			return ErrUnsafe
		}
		if _, e := hex.DecodeString(key); e != nil {
			return ErrUnsafe
		}
		info, e := root.Lstat(name)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 16384 {
			return ErrUnsafe
		}
		if err = reserve(8192 + 128*info.Size()); err != nil {
			return err
		}
		data, e := root.ReadFile(name, info.Size())
		if e != nil {
			return e
		}
		var record record
		if e = json.Unmarshal(data, &record); e != nil {
			return e
		}
		expected, e := (&Manager{root: root, path: path}).key(record.Resource)
		if e != nil || expected != key || record.Version != 1 {
			return ErrUnsafe
		}
		// Completed absent registrations consume only serial parser workspace.
		// Pending records remain evidence even when the producer location vanished.
		completed := record.CompleteCoverage && record.Uses != nil
		for _, done := range record.Uses {
			completed = completed && done
		}
		dead := completed && LocationAbsent(record.Resource.Location)
		if dead && len(includeAbsent) > 0 && !includeAbsent[0] {
			continue
		}
		if !dead {
			if count >= maxRecords {
				return fmt.Errorf("storage-use registration limit reached")
			}
			count++
		}
		if err = visit(record.Resource); err != nil {
			return err
		}
	}
}

// LocationAbsent proves absence through existing, root-bound path components.
// Symlinks, special files and permission failures never establish absence.
func LocationAbsent(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	volume := filepath.VolumeName(path) + string(filepath.Separator)
	base, err := safefs.Open(volume)
	if err != nil {
		return false
	}
	defer base.Close()
	rel, err := filepath.Rel(volume, filepath.Dir(path))
	if err != nil {
		return false
	}
	parent, err := base.Sub(rel, false, 0)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	defer parent.Close()
	_, err = parent.Lstat(filepath.Base(path))
	return errors.Is(err, os.ErrNotExist)
}

const prunedMarker = "swarm-use: completed absent registration pruned\n"

// PruneAbsent removes only completed registration evidence while holding the
// exact resource's admission exclusion. The lock inode must remain: producers
// may already have opened it and be waiting to acquire it.
func (m *Manager) PruneAbsent(ctx context.Context, r Resource) (bool, error) {
	guard, err := m.AcquireMaintenance(ctx, r)
	if err != nil {
		return false, err
	}
	defer guard.Close()
	if !LocationAbsent(r.Location) {
		return false, nil
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	key, err := m.key(r)
	if err != nil {
		return false, err
	}
	if _, err = guard.file.WriteAt([]byte(prunedMarker), 0); err != nil {
		return false, err
	}
	if err = guard.file.Truncate(int64(len(prunedMarker))); err != nil {
		return false, err
	}
	if err = guard.file.Sync(); err != nil {
		return false, err
	}
	if err = m.root.Remove(key + ".json"); err != nil {
		return false, err
	}
	return true, nil
}
