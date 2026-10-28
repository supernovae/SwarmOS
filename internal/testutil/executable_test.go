package testutil

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestWriteExecutableModes(t *testing.T) {
	for _, mode := range []os.FileMode{0700, 0755, 0100} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			dir := t.TempDir()
			// A directory provides the effective umask without changing process state.
			control := filepath.Join(dir, "control")
			if err := os.Mkdir(control, 0777); err != nil {
				t.Fatal(err)
			}
			effective, err := os.Stat(control)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "command")
			want := []byte("#!/bin/sh\nexit 0\n")
			if err := WriteExecutable(path, want, mode); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != mode&effective.Mode().Perm() {
				t.Fatalf("mode = %o, want %o", info.Mode().Perm(), mode&effective.Mode().Perm())
			}
			// Read execute-only fixtures after checking their original permissions.
			if err := os.Chmod(path, info.Mode().Perm()|0400); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("bytes = %q, want %q", got, want)
			}
		})
	}
}

func TestWriteExecutableRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "command")
	original := []byte("#!/bin/sh\nexit 0\n")
	if err := WriteExecutable(path, original, 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{append(append([]byte(nil), original...), []byte("# grown\n")...), []byte("short\n")} {
		if err := WriteExecutable(path, want, 0755); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if after.Mode() != before.Mode() {
			t.Fatalf("mode changed: %v to %v", before.Mode(), after.Mode())
		}
		if runtime.GOOS != "windows" && !os.SameFile(before, after) {
			t.Fatal("rewrite replaced inode")
		}
		if after.Size() != int64(len(want)) {
			t.Fatalf("size = %d, want %d", after.Size(), len(want))
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("bytes = %q, want %q", got, want)
		}
	}
}

func TestWriteExecutableErrorsReleaseLock(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "missing", "command"), dir} {
		if err := WriteExecutable(path, nil, 0700); err == nil {
			t.Fatalf("write to %s succeeded", path)
		}
		// An exclusive acquisition detects a leaked read lock, unlike another writer.
		acquired := make(chan struct{})
		go func() {
			syscall.ForkLock.Lock()
			close(acquired)
			syscall.ForkLock.Unlock()
		}()
		select {
		case <-acquired:
		case <-time.After(5 * time.Second):
			t.Fatal("lock was not released after error")
		}
		if err := WriteExecutable(filepath.Join(dir, "command"), []byte("ok"), 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWriteExecutableWaitsForForkLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "command")
	started := make(chan struct{})
	done := make(chan error, 1)
	syscall.ForkLock.Lock()
	go func() {
		close(started)
		done <- WriteExecutable(path, []byte("payload"), 0700)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		syscall.ForkLock.Unlock()
		t.Fatal("writer goroutine did not start")
	}
	var completed bool
	var writeErr error
	select {
	case writeErr = <-done:
		completed = true
	case <-time.After(100 * time.Millisecond):
	}
	_, statErr := os.Stat(path)
	// Release before assertions or any testing cleanup that might fork.
	syscall.ForkLock.Unlock()
	if completed {
		t.Fatalf("write completed while fork lock held: %v", writeErr)
	}
	if !os.IsNotExist(statErr) {
		t.Fatalf("file appeared while fork lock held: %v", statErr)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write did not complete after unlock")
	}
}
