package safefs

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
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
