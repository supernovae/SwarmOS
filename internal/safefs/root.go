// Package safefs confines coordinator file operations to a trusted directory.
// Roots are directory handles: renaming a parent cannot redirect an operation.
package safefs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	return r.writeFile(name, data, mode, false)
}

// WriteFileDurable also syncs the pinned parent directory after replacement.
func (r *Root) WriteFileDurable(name string, data []byte, mode os.FileMode) error {
	return r.writeFile(name, data, mode, true)
}

func (r *Root) writeFile(name string, data []byte, mode os.FileMode, durable bool) error {
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
	if err := d.Rename(tmp, base); err != nil {
		return err
	}
	if durable {
		parent, err := d.Open(".")
		if err != nil {
			return err
		}
		defer parent.Close()
		return parent.Sync()
	}
	return nil
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

// Rename moves oldname to newname within the root. Both parent directories
// must already exist and contain no symlinked components; the final names are
// replaced as directory entries, so a symlink is moved rather than followed.
func (r *Root) Rename(oldname, newname string) error {
	oldname, err := local(oldname)
	if err != nil {
		return err
	}
	newname, err = local(newname)
	if err != nil {
		return err
	}
	for _, parent := range []string{filepath.Dir(oldname), filepath.Dir(newname)} {
		d, err := r.directory(parent, false, 0)
		if err != nil {
			return err
		}
		d.Close()
	}
	// os.Root confines both names even if a parent changes after the check.
	return r.root.Rename(oldname, newname)
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

// Lstat observes an entry without following its final symlink or opening file
// contents. Parent components are pinned and must be real directories. Unlike
// OpenRegular, this accepts hardlinks because no content is read or written.
func (r *Root) Lstat(name string) (os.FileInfo, error) {
	name, err := local(name)
	if err != nil {
		return nil, err
	}
	d, err := r.directory(filepath.Dir(name), false, 0)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Lstat(filepath.Base(name))
}

// Mkdir exclusively creates a direct child of the pinned root.
func (r *Root) Mkdir(name string, mode os.FileMode) error {
	if name != filepath.Base(name) || !filepath.IsLocal(name) {
		return fmt.Errorf("unsafe directory name %q", name)
	}
	return r.root.Mkdir(name, mode)
}

// ErrCleanupIncomplete identifies ordinary removal exhaustion. Callers must
// retain ownership evidence, but may preserve a successful primary operation.
var ErrCleanupIncomplete = errors.New("managed cleanup incomplete")

// ErrCleanupLimit means preflight refused a traversal before removal began.
var ErrCleanupLimit = fmt.Errorf("%w: cleanup traversal limit reached", ErrCleanupIncomplete)

// RemoveAllChecked removes a direct child only if its identity is unchanged.
// Permission repair uses opened directory handles, never symlink targets.
func (r *Root) RemoveAllChecked(name string, expected os.FileInfo) error {
	return r.removeAllChecked(name, expected, r.root.RemoveAll)
}

func (r *Root) removeAllChecked(name string, expected os.FileInfo, remove func(string) error) error {
	if name != filepath.Base(name) || !filepath.IsLocal(name) {
		return fmt.Errorf("unsafe directory name %q", name)
	}
	for attempt := 0; attempt < 3; attempt++ {
		info, err := r.root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || expected == nil || !os.SameFile(info, expected) {
			return fmt.Errorf("managed directory changed before cleanup")
		}
		err = remove(name)
		if err == nil {
			return nil
		}
		if !ordinaryCleanupError(err) {
			return err
		}
		// Verify and repair even on the last attempt: an unsafe replacement must
		// never be classified as an ordinary removal failure.
		if repairErr := repairCleanupDirectories(r.root, name, expected, 0); repairErr != nil {
			return repairErr
		}
		if attempt == 2 {
			return errors.Join(ErrCleanupIncomplete, err)
		}
	}
	panic("unreachable")
}

func ordinaryCleanupError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EBUSY)
}

func repairCleanupDirectories(parent *os.Root, name string, expected os.FileInfo, depth int) error {
	if depth > 256 {
		return fmt.Errorf("cleanup directory nesting exceeds limit")
	}
	f, err := openCleanupDirectory(parent, name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() || !os.SameFile(info, expected) {
		return fmt.Errorf("cleanup directory replaced")
	}
	current, err := parent.Lstat(name)
	if err != nil || !os.SameFile(current, info) {
		return fmt.Errorf("cleanup directory changed: %w", err)
	}
	if info.Mode().Perm()&0700 != 0700 {
		if err := f.Chmod(info.Mode() | 0700); err != nil {
			return err
		}
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	pinned, err := dir.Stat(".")
	if err != nil || !os.SameFile(pinned, info) {
		return fmt.Errorf("cleanup directory replaced while opening: %w", err)
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child, err := dir.Lstat(entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		switch {
		case child.IsDir():
			if err := repairCleanupDirectories(dir, entry.Name(), child, depth+1); err != nil {
				return err
			}
		case child.Mode().IsRegular(), child.Mode()&os.ModeSymlink != 0:
			// Removal unlinks these entries; permission repair never opens them.
		default:
			return fmt.Errorf("special file in cleanup directory")
		}
	}
	current, err = parent.Lstat(name)
	if err != nil || !os.SameFile(current, info) {
		return fmt.Errorf("cleanup directory changed after repair: %w", err)
	}
	return nil
}

// RemoveAllCheckedContext removes one checked directory with bounded, cancellable
// traversal. Both passes reject links, special files and device boundaries.
// Permission repair uses pinned directory handles after identity checks. An error can follow
// partial removal; callers must preserve recovery and admission evidence.
func (r *Root) RemoveAllCheckedContext(ctx context.Context, name string, expected os.FileInfo, visit func() error) error {
	if name != filepath.Base(name) || !filepath.IsLocal(name) || expected == nil || !expected.IsDir() || visit == nil {
		return errors.New("invalid checked removal")
	}
	device := func(i os.FileInfo) (uint64, bool) {
		st, ok := i.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}
		return uint64(st.Dev), true
	}
	dev, ok := device(expected)
	if !ok {
		return errors.New("unknown cleanup device")
	}
	inspected := map[string]os.FileInfo{}
	// Bound retained paths, map overhead and FileInfo objects before admission.
	// Ordinary module leaves with 100,000 entries fit within this allowance.
	var identityBytes int64
	const identityLimit = 128 << 20
	const entryLimit = 200000
	var walk func(*os.Root, string, string, os.FileInfo, int, bool) error
	walk = func(parent *os.Root, child, relative string, want os.FileInfo, depth int, remove bool) error {
		if remove {
			var known bool
			want, known = inspected[relative]
			if !known {
				return errors.New("uninspected cleanup entry")
			}
		} else {
			charge := int64(512 + 2*len(relative))
			if len(inspected) >= entryLimit || charge > identityLimit-identityBytes {
				return fmt.Errorf("%w: descendant identity limit reached", ErrCleanupLimit)
			}
			identityBytes += charge
			inspected[relative] = want
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(); err != nil {
			return err
		}
		if depth > 64 {
			return fmt.Errorf("%w: cleanup depth exceeded", ErrCleanupLimit)
		}
		check := func() error {
			now, err := parent.Lstat(child)
			if err != nil {
				return err
			}
			d, known := device(now)
			if !known || d != dev || !os.SameFile(now, want) || now.Mode() != want.Mode() {
				return errors.New("cleanup entry changed")
			}
			return nil
		}
		if err := check(); err != nil {
			return err
		}
		if want.Mode().IsRegular() {
			if remove {
				if err := ctx.Err(); err != nil {
					return err
				}
				return parent.Remove(child)
			}
			return nil
		}
		if !want.IsDir() {
			return errors.New("unsafe cleanup entry")
		}
		dir, err := parent.OpenRoot(child)
		if err != nil {
			return err
		}
		defer dir.Close()
		pinned, err := dir.Stat(".")
		if err != nil || !os.SameFile(pinned, want) {
			return errors.New("cleanup directory changed")
		}
		f, err := dir.Open(".")
		if err != nil {
			return err
		}
		defer f.Close()
		if remove && want.Mode().Perm()&0700 != 0700 {
			if err := f.Chmod(want.Mode() | 0700); err != nil {
				return err
			}
			want, err = f.Stat()
			if err != nil {
				return err
			}
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, err := f.ReadDir(1)
			if len(entries) == 0 {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
			if err := check(); err != nil {
				return err
			}
			info, err := dir.Lstat(entries[0].Name())
			if err != nil {
				return err
			}
			if err := walk(dir, entries[0].Name(), filepath.Join(relative, entries[0].Name()), info, depth+1, remove); err != nil {
				return err
			}
		}
		if err := check(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if remove {
			return parent.Remove(child)
		}
		return nil
	}
	if err := walk(r.root, name, name, expected, 0, false); err != nil {
		return err
	}
	return walk(r.root, name, name, expected, 0, true)
}
