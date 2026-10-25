package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveReadOnlyRuntime(t *testing.T) {
	for _, tc := range []struct{ os, runtime, cli, image, want, failure string }{
		{"darwin", "", "", "", "sandbox-exec", ""}, {"darwin", "sandbox-exec", "", "", "sandbox-exec", ""},
		{"darwin", "bwrap", "", "", "", "macOS"}, {"darwin", "", "", "", "sandbox-exec", "missing"},
		{"linux", "", "", "", "bwrap", ""}, {"linux", "bwrap", "", "", "bwrap", ""},
		{"linux", "sandbox-exec", "", "", "", "Linux"}, {"linux", "", "", "", "bwrap", "missing"},
		{"windows", "", "", "", "", "unsupported"},
		{"windows", "container", "", "image", "docker", ""}, {"linux", "container", "docker", "image", "docker", ""},
		{"darwin", "container", "podman", "image", "podman", ""}, {"linux", "container", "podman", "", "", "image"},
		{"linux", "container", "podman", "image", "podman", "missing"},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			called := ""
			path, err := resolveReadOnlyRuntime(tc.runtime, tc.cli, tc.image, tc.os, func(name string) (string, error) {
				called = name
				if tc.failure == "missing" {
					return "", fmt.Errorf("missing")
				}
				return "/fixture/" + name, nil
			})
			if called != tc.want {
				t.Fatalf("lookup %q, want %q", called, tc.want)
			}
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("expected %q error, got %v", tc.failure, err)
				}
				return
			}
			if err != nil || path != "/fixture/"+tc.want {
				t.Fatalf("path=%q err=%v", path, err)
			}
		})
	}
}

func TestCheckReadOnlyRuntime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	bin := "podman"
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, bin), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{"", "image"} {
		path, err := ResolveReadOnlyRuntime("container", "podman", image)
		compat := CheckReadOnlyRuntime("container", "podman", image)
		if (err == nil) != (compat == nil) {
			t.Fatalf("resolver=%v compatibility=%v", err, compat)
		}
		if image != "" && (err != nil || path != filepath.Join(dir, bin)) {
			t.Fatalf("%q %v", path, err)
		}
	}
}

func TestCheckReadOnlyRuntimeMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := ResolveReadOnlyRuntime("container", "docker", "image")
	compat := CheckReadOnlyRuntime("container", "docker", "image")
	if err == nil || compat == nil || err.Error() != compat.Error() {
		t.Fatalf("resolver=%v compatibility=%v", err, compat)
	}
}
