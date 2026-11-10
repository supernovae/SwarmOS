package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The reviewer boundary hides host runtime state. This canary runs a real
// sandboxed command and fails if a file placed in the user's runtime directory
// is visible, or if /run has any content, inside the boundary.
func TestReadOnlyCommandHidesHostRuntimeState(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux reviewer boundary")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap unavailable")
	}
	work, home := t.TempDir(), t.TempDir()
	output := filepath.Join(work, "out")
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	// A directory outside /tmp, /home and /run stands in for a host runtime
	// directory when the real one is not writable in this environment.
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		if probe, err := os.MkdirTemp(dir, "swarm-canary-*"); err == nil {
			runtimeDir = probe
			t.Cleanup(func() { os.RemoveAll(probe) })
		}
	}
	t.Setenv("XDG_RUNTIME_DIR", filepath.Dir(runtimeDir))
	marker := filepath.Join(runtimeDir, "marker")
	if err := os.WriteFile(marker, []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}
	script := `if [ -e "$1" ]; then echo "marker visible"; exit 1; fi; if [ -n "$(ls -A /run 2>/dev/null)" ]; then echo "/run visible"; exit 1; fi; echo hidden`
	shell, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Skip("no shell")
	}
	if probe, probeErr := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-user", "--", shell, "-c", "true").CombinedOutput(); probeErr != nil {
		t.Skipf("bwrap cannot create a user namespace here: %s", probe)
	}
	cmd, err := ReadOnlyCommand(work, output, shell, []string{"-c", script, "sh", marker}, Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "hidden" {
		t.Fatalf("host runtime state reached the reviewer boundary: %v\n%s", err, out)
	}
}

func TestLinuxRuntimeMasksCoverRunAndResolver(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux reviewer boundary")
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	joined := strings.Join(linuxRuntimeMasks(false), " ")
	if info, err := os.Lstat("/run"); err == nil && info.IsDir() && !strings.Contains(joined, "--tmpfs /run") {
		t.Fatal(joined)
	}
	if strings.Contains(joined, "--ro-bind") {
		t.Fatal("offline reviewer kept a runtime file:", joined)
	}
	// Relative and root values never become mounts.
	for _, bad := range []string{"relative/dir", "/"} {
		t.Setenv("XDG_RUNTIME_DIR", bad)
		if got := strings.Join(linuxRuntimeMasks(false), " "); strings.Contains(got, "--tmpfs "+bad+" ") || strings.HasSuffix(got, "--tmpfs "+bad) {
			t.Fatal(got)
		}
	}
}
