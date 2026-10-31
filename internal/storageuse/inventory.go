package storageuse

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

// VisitResources unions existing durable registrations into an inventory. It
// reads one capped record at a time and never follows its Location. The caller
// must separately admit that location under its trusted discovery roots.
func VisitResources(ctx context.Context, path string, maxRecords int, entry func() error, reserve func(int64) error, visit func(Resource) error) error {
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
		if count >= maxRecords {
			return fmt.Errorf("storage-use registration limit reached")
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
		count++
		if err = visit(record.Resource); err != nil {
			return err
		}
	}
}
