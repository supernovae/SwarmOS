package changelog

import (
	"github.com/supernovae/swarmos/internal/testutil"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// snapshot records every entry under root without following symlinks or
// opening anything but regular files, so it never blocks on a FIFO.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		entry := info.Mode().String() + " " + info.ModTime().String()
		switch {
		case info.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			entry += " " + string(b)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			entry += " -> " + target
		}
		tree[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestCheck(t *testing.T) {
	mkfifo, fifoErr := exec.LookPath("mkfifo")
	tests := []struct {
		name  string
		setup func(t *testing.T, r, outside string)
		want  []string
		skip  []string
		fails bool
	}{
		{name: "missing", setup: func(t *testing.T, r, outside string) {}},
		{name: "empty", setup: func(t *testing.T, r, outside string) {
			if err := os.MkdirAll(filepath.Join(r, "changes/unreleased"), 0755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "non-md only", setup: func(t *testing.T, r, outside string) {
			put(t, r, "changes/unreleased/keep.txt", "- a\n- b")
		}},
		{name: "valid", setup: func(t *testing.T, r, outside string) {
			put(t, r, "changes/unreleased/01.md", "- One")
			put(t, r, "changes/unreleased/02.md", "- Two\n  more")
		}},
		{name: "two-bullet", setup: func(t *testing.T, r, outside string) {
			put(t, r, "changes/unreleased/78.md", "- One\n- Two")
		}, want: []string{"fragment 78.md: each fragment holds exactly one bullet"}, fails: true},
		{name: "reports all", setup: func(t *testing.T, r, outside string) {
			put(t, r, "changes/unreleased/01.md", "- Valid")
			put(t, r, "changes/unreleased/02.md", "- a\n- b")
			put(t, r, "changes/unreleased/03.md", "paragraph")
			put(t, r, "changes/unreleased/04.md", "")
		}, want: []string{
			"fragment 02.md: each fragment holds exactly one bullet; expected only indented prose after the bullet",
			"fragment 03.md: each fragment holds exactly one bullet; expected one nonempty '- ' bullet",
			"fragment 04.md: each fragment holds exactly one bullet; expected one nonempty '- ' bullet",
		}, fails: true},
		{name: "directory md", setup: func(t *testing.T, r, outside string) {
			if err := os.MkdirAll(filepath.Join(r, "changes/unreleased/dir.md"), 0755); err != nil {
				t.Fatal(err)
			}
		}, want: []string{"fragment dir.md: refuse non-regular file"}, fails: true},
		{name: "symlink md", setup: func(t *testing.T, r, outside string) {
			put(t, outside, "target.md", "- Outside")
			put(t, r, "changes/unreleased/01.md", "- Valid")
			if err := os.Symlink(filepath.Join(outside, "target.md"), filepath.Join(r, "changes/unreleased/link.md")); err != nil {
				t.Skip(err)
			}
		}, want: []string{"fragment link.md: refuse non-regular file"}, fails: true},
		{name: "fifo md", setup: func(t *testing.T, r, outside string) {
			if fifoErr != nil {
				t.Skip("mkfifo unavailable")
			}
			if err := os.MkdirAll(filepath.Join(r, "changes/unreleased"), 0755); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(mkfifo, filepath.Join(r, "changes/unreleased/pipe.md")).CombinedOutput(); err != nil {
				t.Fatalf("mkfifo: %v: %s", err, out)
			}
		}, want: []string{"fragment pipe.md: refuse non-regular file"}, fails: true},
		{name: "unsafe dir", setup: func(t *testing.T, r, outside string) {
			put(t, outside, "unreleased/1.md", "- Outside")
			if err := os.MkdirAll(filepath.Join(r, "changes"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "unreleased"), filepath.Join(r, "changes/unreleased")); err != nil {
				t.Skip(err)
			}
		}, want: []string{"unsafe directory component"}, skip: []string{"1.md"}, fails: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, outside := fixture(t), t.TempDir()
			tt.setup(t, r, outside)
			before, beforeOutside := snapshot(t, r), snapshot(t, outside)
			done := make(chan error, 1)
			go func() { done <- Check(r) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Check blocked")
			}
			if !tt.fails {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("accepted malformed fragments")
				}
				lines := strings.Split(err.Error(), "\n")
				if len(lines) != len(tt.want) {
					t.Fatalf("got %d problems, want %d: %v", len(lines), len(tt.want), err)
				}
				for i, want := range tt.want {
					if !strings.Contains(lines[i], want) {
						t.Fatalf("line %d = %q, want %q", i, lines[i], want)
					}
				}
				for _, name := range append([]string{"01.md"}, tt.skip...) {
					if strings.Contains(err.Error(), name) {
						t.Fatalf("error names %s: %v", name, err)
					}
				}
			}
			if !reflect.DeepEqual(before, snapshot(t, r)) || !reflect.DeepEqual(beforeOutside, snapshot(t, outside)) {
				t.Fatal("Check changed the tree")
			}
			if read(t, r, "CHANGELOG.md") != baseline {
				t.Fatal("CHANGELOG.md changed")
			}
		})
	}
}

func TestMakeChangelogCheck(t *testing.T) {
	if _, e := exec.LookPath("make"); e != nil {
		t.Skip(e)
	}
	r := fixture(t)
	binary := filepath.Join(t.TempDir(), "changelog")
	cmd := exec.Command("go", "build", "-o", binary, "../../cmd/changelog")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	mf, e := os.ReadFile("../../Makefile")
	if e != nil {
		t.Fatal(e)
	}
	put(t, r, "Makefile", string(mf))
	wrapper := filepath.Join(t.TempDir(), "go-wrapper")
	if e := testutil.WriteExecutable(wrapper, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(binary, "'", "'\\''")+"' \"$3\"\n"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(wrapper, 0755); e != nil {
		t.Fatal(e)
	}
	run := func() ([]byte, error) {
		c := exec.Command("make", "changelog-check", "GO="+wrapper)
		c.Dir = r
		return c.CombinedOutput()
	}
	if out, e := run(); e != nil {
		t.Fatalf("no fragments: %v: %s", e, out)
	}
	put(t, r, "changes/unreleased/1.md", "- New")
	if out, e := run(); e != nil {
		t.Fatalf("valid fragment: %v: %s", e, out)
	}
	put(t, r, "changes/unreleased/bad.md", "- One\n- Two")
	before := snapshot(t, r)
	if out, e := run(); e == nil || !strings.Contains(string(out), "bad.md") || !strings.Contains(string(out), "each fragment holds exactly one bullet") {
		t.Fatalf("%v: %s", e, out)
	}
	if !reflect.DeepEqual(before, snapshot(t, r)) {
		t.Fatal("make changelog-check changed the tree")
	}
}

func TestRepositoryFragmentsValid(t *testing.T) {
	if err := Check(filepath.Join("..", "..")); err != nil {
		t.Fatalf("malformed changelog fragments under changes/unreleased/:\n%v", err)
	}
}
