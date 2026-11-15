//go:build !windows

package process

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStopVerifiedWaitsForDescendantExit(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sh", "-c", "sleep 30 </dev/null >/dev/null 2>&1 & printf '%d\n' $!")
	Configure(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(out.String())
	if pid == "" {
		t.Fatal("missing child pid")
	}
	if err := StopVerified(cmd); err != nil {
		t.Fatalf("StopVerified: %v", err)
	}
	if runtime.GOOS == "linux" {
		if raw, err := os.ReadFile(filepath.Join("/proc", pid, "stat")); err == nil {
			end := bytes.LastIndexByte(raw, ')')
			if end < 0 || !bytes.HasPrefix(raw[end+1:], []byte(" Z ")) && !bytes.HasPrefix(raw[end+1:], []byte(" X ")) {
				t.Fatalf("descendant still running after StopVerified: %s", raw)
			}
		}
	}
}
