package safefs

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestReadAndWriteRejectSymlinks(t *testing.T) {
	for _, parentLink := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "parent"}[parentLink], func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			target := filepath.Join(outside, "canary")
			if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			name := "link"
			linkTarget := target
			if parentLink {
				linkTarget = outside
				name = "link/canary"
			}
			if err := os.Symlink(linkTarget, filepath.Join(root, "link")); err != nil {
				t.Skip(err)
			}
			if _, err := ReadFile(root, name, 1024); err == nil {
				t.Fatal("symlink read accepted")
			}
			if err := WriteFile(root, name, []byte("replaced"), 0600); err == nil {
				t.Fatal("symlink write accepted")
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "unchanged" {
				t.Fatalf("outside file changed: %q %v", data, err)
			}
		})
	}
}

func TestInternalSymlinksAlsoRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real"), []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Skip(err)
	}
	if _, err := ReadFile(root, "link", 10); err == nil {
		t.Fatal("internal symlink accepted")
	}
}

func TestBoundedReadAndHardLink(t *testing.T) {
	root := t.TempDir()
	if err := WriteFile(root, "nested/file", bytes.Repeat([]byte("a"), 1025), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(root, "nested/file", 1024); err == nil {
		t.Fatal("oversized read accepted")
	}
	if _, err := ReadFile(root, "../outside", 1024); err == nil {
		t.Fatal("traversal accepted")
	}
	if err := os.Link(filepath.Join(root, "nested/file"), filepath.Join(root, "alias")); err != nil {
		t.Skip(err)
	}
	if _, err := ReadFile(root, "alias", 2048); err == nil {
		t.Fatal("hard link accepted")
	}
	if err := WriteFile(root, "alias", []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "nested/file"))
	if err != nil || len(b) != 1025 {
		t.Fatal("atomic replacement modified hard-link source")
	}
}

func TestLockRejectsSymlinksAndHardLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("canary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Skip(err)
	}
	if err := os.Link(filepath.Join(root, "target"), filepath.Join(root, "hard")); err != nil {
		t.Skip(err)
	}
	r, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, name := range []string{"link", "hard"} {
		if f, err := r.OpenLock(name); err == nil {
			f.Close()
			t.Fatalf("accepted %s lock", name)
		}
	}
}

func TestParentReplacementCannotEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "canary"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "slot"), 0700); err != nil {
		t.Fatal(err)
	}
	// Probe symlink support before starting the adversarial replacement loop.
	if err := os.Symlink(outside, filepath.Join(root, "probe")); err != nil {
		t.Skip(err)
	}
	if err := os.Remove(filepath.Join(root, "probe")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			if os.Rename(filepath.Join(root, "slot"), filepath.Join(root, "held")) != nil {
				continue
			}
			_ = os.Symlink(outside, filepath.Join(root, "slot"))
			_ = os.Remove(filepath.Join(root, "slot"))
			_ = os.Rename(filepath.Join(root, "held"), filepath.Join(root, "slot"))
		}
	}()
	for i := 0; i < 300; i++ {
		_ = WriteFile(root, "slot/canary", []byte("inside"), 0600)
		b, err := ReadFile(root, "slot/canary", 100)
		if err == nil && string(b) == "outside" {
			t.Error("read escaped root")
		}
	}
	wg.Wait()
	b, err := os.ReadFile(filepath.Join(outside, "canary"))
	if err != nil || string(b) != "outside" {
		t.Fatalf("outside canary changed: %q %v", b, err)
	}
}

func TestRootRename(t *testing.T) {
	tests := []struct {
		name     string
		from, to string
		setup    func(t *testing.T, root, outside string)
		ok       bool
	}{
		{"moves directory", "a/src", "b/dst", func(t *testing.T, root, _ string) {
			for _, dir := range []string{"a/src", "b"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
		}, true},
		{"escape", "a/src", "../dst", func(t *testing.T, root, _ string) {
			if err := os.MkdirAll(filepath.Join(root, "a/src"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"symlinked source parent", "a/src", "b/dst", func(t *testing.T, root, outside string) {
			if err := os.MkdirAll(filepath.Join(outside, "src"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "b"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
				t.Skip(err)
			}
		}, false},
		{"symlinked destination parent", "a/src", "b/dst", func(t *testing.T, root, outside string) {
			if err := os.MkdirAll(filepath.Join(root, "a/src"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "b")); err != nil {
				t.Skip(err)
			}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			tt.setup(t, root, outside)
			r, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			err = r.Rename(tt.from, tt.to)
			if (err == nil) != tt.ok {
				t.Fatalf("Rename(%q, %q) = %v, want ok=%v", tt.from, tt.to, err, tt.ok)
			}
			if tt.ok {
				if info, err := os.Lstat(filepath.Join(root, tt.to)); err != nil || !info.IsDir() {
					t.Fatalf("destination missing: %v", err)
				}
			}
			if entries, _ := os.ReadDir(outside); !tt.ok && len(entries) > 1 {
				t.Fatalf("rename reached outside the root: %v", entries)
			}
			if _, err := os.Lstat(filepath.Join(outside, "dst")); !tt.ok && err == nil {
				t.Fatal("rename created a destination outside the root")
			}
		})
	}
}

func TestRemoveAllCheckedReadOnlyTree(t *testing.T) {
	parent := t.TempDir()
	tree := filepath.Join(parent, "scratch")
	nested := filepath.Join(tree, "module", "nested")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "module.go"), []byte("module"), 0444); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{nested, filepath.Dir(nested)} {
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0700)
	}
	r, err := Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	info, err := os.Lstat(tree)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveAllChecked("scratch", info); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatalf("scratch remains: %v", err)
	}
}

func TestRemoveAllCheckedRetryAndConfinement(t *testing.T) {
	for _, mode := range []string{"exhaustion", "transient", "symlink", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			parent, outside := t.TempDir(), t.TempDir()
			sentinel := filepath.Join(outside, "keep")
			if err := os.WriteFile(sentinel, []byte("outside"), 0444); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(outside, 0555); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(outside, 0700)
			tree := filepath.Join(parent, "scratch")
			if err := os.Mkdir(tree, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(tree, "link")); err != nil {
				t.Skip(err)
			}
			r, err := Open(parent)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			info, err := os.Lstat(tree)
			if err != nil {
				t.Fatal(err)
			}
			attempts := 0
			err = r.removeAllChecked("scratch", info, func(name string) error {
				attempts++
				if attempts == 1 && (mode == "replacement" || mode == "symlink") {
					if err := os.Rename(tree, tree+"-original"); err != nil {
						t.Fatal(err)
					}
					if mode == "symlink" {
						if err := os.Symlink(outside, tree); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.Mkdir(tree, 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
				if mode == "transient" && attempts == 2 {
					return r.root.RemoveAll(name)
				}
				return syscall.ENOTEMPTY
			})
			switch mode {
			case "transient":
				if err != nil || attempts != 2 {
					t.Fatalf("retry: %d %v", attempts, err)
				}
			case "exhaustion":
				if !errors.Is(err, ErrCleanupIncomplete) || attempts != 3 {
					t.Fatalf("exhaustion: %d %v", attempts, err)
				}
			default:
				if err == nil || errors.Is(err, ErrCleanupIncomplete) || attempts != 1 {
					t.Fatalf("unsafe: %d %v", attempts, err)
				}
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "outside" {
				t.Fatalf("outside changed: %q %v", data, err)
			}
			st, err := os.Stat(sentinel)
			if err != nil || st.Mode().Perm() != 0444 {
				t.Fatalf("outside mode changed: %v %v", st, err)
			}
			st, err = os.Stat(outside)
			if err != nil || st.Mode().Perm() != 0555 {
				t.Fatalf("outside directory changed: %v %v", st, err)
			}
		})
	}
}

func TestCleanupRepairRejectsSpecialFile(t *testing.T) {
	parent := t.TempDir()
	tree := filepath.Join(parent, "scratch")
	if err := os.Mkdir(tree, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(tree, "socket"))
	if err != nil {
		t.Skipf("Unix socket fixture unavailable: %v", err)
	}
	defer listener.Close()
	r, err := Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	info, err := os.Lstat(tree)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	err = r.removeAllChecked("scratch", info, func(string) error { attempts++; return syscall.ENOTEMPTY })
	if err == nil || errors.Is(err, ErrCleanupIncomplete) || attempts != 1 {
		t.Fatalf("special file accepted: attempts=%d error=%v", attempts, err)
	}
}

func TestCleanupRepairUsesPinnedParent(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	parent := filepath.Join(base, "parent")
	tree := filepath.Join(parent, "scratch")
	if err := os.MkdirAll(filepath.Join(tree, "module"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(tree, "module"), 0555); err != nil {
		t.Fatal(err)
	}
	r, err := Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	info, err := os.Lstat(tree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent, parent+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Skip(err)
	}
	sentinel := filepath.Join(outside, "keep")
	if err := os.WriteFile(sentinel, []byte("outside"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveAllChecked("scratch", info); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "outside" {
		t.Fatalf("outside changed: %q %v", data, err)
	}
	st, err := os.Stat(sentinel)
	if err != nil || st.Mode().Perm() != 0444 {
		t.Fatalf("outside permissions changed: %v %v", st, err)
	}
}

func TestCleanupRepairPreservesDirectoryModeBits(t *testing.T) {
	parent := t.TempDir()
	tree := filepath.Join(parent, "scratch")
	if err := os.Mkdir(tree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tree, os.ModeSticky|0555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(tree, 0700)
	r, err := Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	info, err := os.Lstat(tree)
	if err != nil {
		t.Fatal(err)
	}
	err = r.removeAllChecked("scratch", info, func(string) error { return syscall.ENOTEMPTY })
	if !errors.Is(err, ErrCleanupIncomplete) {
		t.Fatal(err)
	}
	after, err := os.Lstat(tree)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode()&os.ModeSticky != info.Mode()&os.ModeSticky || after.Mode().Perm() != 0755 {
		t.Fatalf("repair changed unrelated mode bits: %v -> %v", info.Mode(), after.Mode())
	}
}
