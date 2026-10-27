//go:build unix

package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestTelemetrySessionLogRefusesLinksAndSpecialFiles(t *testing.T) {
	outside := t.TempDir()
	writeLog(t, outside, ".claude/projects/p/s.jsonl", `{"type":"assistant","message":{"id":"m","content":[],"usage":{"input_tokens":9,"output_tokens":9}}}`+"\n")
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, home string)
	}{
		{"root component", func(t *testing.T, home string) {
			if err := os.Symlink(filepath.Join(outside, ".claude"), filepath.Join(home, ".claude")); err != nil {
				t.Fatal(err)
			}
		}},
		{"intermediate directory", func(t *testing.T, home string) {
			writeLog(t, home, ".claude/projects/keep.txt", "x")
			if err := os.Symlink(filepath.Join(outside, ".claude", "projects", "p"), filepath.Join(home, ".claude", "projects", "p")); err != nil {
				t.Fatal(err)
			}
		}},
		{"leaf file", func(t *testing.T, home string) {
			writeLog(t, home, ".claude/projects/p/keep.txt", "x")
			if err := os.Symlink(filepath.Join(outside, ".claude", "projects", "p", "s.jsonl"), filepath.Join(home, ".claude", "projects", "p", "s.jsonl")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hard link", func(t *testing.T, home string) {
			writeLog(t, home, ".claude/projects/p/keep.txt", "x")
			if err := os.Link(filepath.Join(outside, ".claude", "projects", "p", "s.jsonl"), filepath.Join(home, ".claude", "projects", "p", "s.jsonl")); err != nil {
				t.Skip(err)
			}
		}},
		{"fifo", func(t *testing.T, home string) {
			writeLog(t, home, ".claude/projects/p/keep.txt", "x")
			if err := syscall.Mkfifo(filepath.Join(home, ".claude", "projects", "p", "s.jsonl"), 0o600); err != nil {
				t.Skip(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.setup(t, home)
			if got := usageSummary(collectSessionUsage("claude", home)); got != unavailableUsage {
				t.Fatalf("collector followed an unsafe entry: %s", got)
			}
		})
	}
}
