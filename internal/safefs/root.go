// Package safefs confines coordinator file operations to a trusted directory.
// Roots are directory handles: renaming a parent cannot redirect an operation.
package safefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Root struct{ root *os.Root }

func Open(path string) (*Root, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &Root{r}, nil
}
func (r *Root) Close() error { return r.root.Close() }

func local(name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("unsafe relative path %q", name)
	}
	return filepath.Clean(name), nil
}

// directory opens and pins each directory component, rejecting symlinks. The
// underlying os.Root also enforces containment if a component changes mid-open.
func (r *Root) directory(name string, create bool, mode os.FileMode) (*os.Root, error) {
	name, err := local(name)
	if err != nil {
		return nil, err
	}
	current, err := r.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if name == "." {
		return current, nil
	}
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		before, err := current.Lstat(part)
		if errors.Is(err, os.ErrNotExist) && create {
			err = current.Mkdir(part, mode)
			if err != nil && !errors.Is(err, os.ErrExist) {
				current.Close()
				return nil, err
			}
			before, err = current.Lstat(part)
		}
		if err != nil {
			current.Close()
			return nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			current.Close()
			return nil, fmt.Errorf("unsafe directory component %q", part)
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			current.Close()
			return nil, err
		}
		after, statErr := next.Stat(".")
		entry, entryErr := current.Lstat(part)
		current.Close()
		if statErr != nil || entryErr != nil || !entry.IsDir() || !os.SameFile(before, after) || !os.SameFile(entry, after) {
			next.Close()
			return nil, fmt.Errorf("directory changed while opening %q", part)
		}
		current = next
	}
	return current, nil
}

func (r *Root) MkdirAll(name string, mode os.FileMode) error {
	d, err := r.directory(name, true, mode)
	if err != nil {
		return err
	}
	return d.Close()
}

// Sub opens a pinned child directory, optionally creating missing directories.
func (r *Root) Sub(name string, create bool, mode os.FileMode) (*Root, error) {
	d, err := r.directory(name, create, mode)
	if err != nil {
		return nil, err
	}
	return &Root{d}, nil
}

func regular(parent *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	before, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && flags&os.O_CREATE != 0 {
		f, createErr := parent.OpenFile(name, flags|os.O_EXCL|nonblock, mode)
		if createErr == nil {
			return f, nil
		}
		if !errors.Is(createErr, os.ErrExist) {
			return nil, createErr
		}
		before, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("refuse non-regular file %q", name)
	}
	f, err := openExisting(parent, name, flags&^os.O_CREATE)
	if err != nil {
		return nil, err
	}
	actual, statErr := f.Stat()
	after, afterErr := parent.Lstat(name)
	if statErr != nil || afterErr != nil || !actual.Mode().IsRegular() || !after.Mode().IsRegular() || !os.SameFile(before, actual) || !os.SameFile(after, actual) {
		f.Close()
		return nil, fmt.Errorf("file changed while opening %q", name)
	}
	if err := singleLink(f, actual); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// OpenDirectory returns a pinned directory descriptor for bounded ReadDir calls.
func (r *Root) OpenDirectory(name string) (*os.File, error) {
	d, err := r.directory(name, false, 0)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Open(".")
}

// OpenRegular returns a descriptor validated before any content is consumed.
// The caller must close it. Concurrent writers remain possible; workers must be
// stopped before importing artifacts that require a consistent snapshot.
func (r *Root) OpenRegular(name string) (*os.File, error) {
	name, err := local(name)
	if err != nil {
		return nil, err
	}
	d, err := r.directory(filepath.Dir(name), false, 0)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return regular(d, filepath.Base(name), os.O_RDONLY, 0)
}

func (r *Root) ReadFile(name string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("positive file size limit required")
	}
	f, err := r.OpenRegular(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("file %q exceeds %d byte limit", name, maxBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("file %q exceeds %d byte limit", name, maxBytes)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(info, after) || after.Size() != info.Size() || after.Size() != int64(len(b)) || !after.ModTime().Equal(info.ModTime()) {
		return nil, fmt.Errorf("file %q changed while reading", name)
	}
	if err := singleLink(f, after); err != nil {
		return nil, err
	}
	return b, nil
}

func (r *Root) OpenLock(name string) (*os.File, error) {
	name, err := local(name)
	if err != nil {
		return nil, err
	}
	d, err := r.directory(filepath.Dir(name), true, 0700)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return regular(d, filepath.Base(name), os.O_CREATE|os.O_RDWR, 0600)
}

func (r *Root) WriteFile(name string, data []byte, mode os.FileMode) error {
	name, err := local(name)
	if err != nil {
		return err
	}
	d, err := r.directory(filepath.Dir(name), true, 0755)
	if err != nil {
		return err
	}
	defer d.Close()
	base := filepath.Base(name)
	if info, err := d.Lstat(base); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refuse non-regular destination %q", name)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	tmp := ".swarm-write-" + hex.EncodeToString(random[:])
	f, err := d.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer d.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Rename replaces the directory entry; it never writes through a substituted
	// destination symlink or hard link. Both names use the same pinned directory.
	return d.Rename(tmp, base)
}

func (r *Root) Remove(name string) error {
	name, err := local(name)
	if err != nil {
		return err
	}
	d, err := r.directory(filepath.Dir(name), false, 0)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Remove(filepath.Base(name))
}
func ReadFile(root, name string, maxBytes int64) ([]byte, error) {
	r, err := Open(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.ReadFile(name, maxBytes)
}
func WriteFile(root, name string, data []byte, mode os.FileMode) error {
	r, err := Open(root)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.WriteFile(name, data, mode)
}
