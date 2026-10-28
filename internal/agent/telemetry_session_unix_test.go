//go:build unix

package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// moveBehindLink moves path elsewhere and leaves a symlink in its place.
func moveBehindLink(t *testing.T, path string) {
	t.Helper()
	moved := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, path); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryHarnessSessionRefusesLinks(t *testing.T) {
	for _, tc := range []struct {
		driver string
		target func(f *sessionFixture) string
	}{
		{"claude", func(f *sessionFixture) string { return filepath.Join(f.session.Home, ".claude") }},
		{"claude", func(f *sessionFixture) string { return filepath.Dir(f.files[0]) }},
		{"claude", func(f *sessionFixture) string { return f.files[0] }},
		{"omp", func(f *sessionFixture) string { return filepath.Join(f.session.Home, ".omp") }},
		{"omp", func(f *sessionFixture) string { return filepath.Dir(f.files[0]) }},
		{"omp", func(f *sessionFixture) string { return f.files[0] }},
		{"codex", func(f *sessionFixture) string { return filepath.Join(f.session.Home, ".codex", "sessions") }},
		{"codex", func(f *sessionFixture) string { return filepath.Dir(f.files[0]) }},
		{"codex", func(f *sessionFixture) string { return f.files[0] }},
	} {
		f := newSessionFixture(t, tc.driver)
		target := tc.target(f)
		t.Run(tc.driver+"/"+filepath.Base(target), func(t *testing.T) {
			moveBehindLink(t, target)
			if got := usageSummaryR(f.collect()); got != unavailableHarnessUsage {
				t.Fatalf("followed a link: %s", got)
			}
		})
	}
}

// A FIFO blocks whoever opens it, so these prove the locators open only the
// derived session files: names are matched before anything is opened.
func TestTelemetryHarnessSessionNeverOpensOtherFiles(t *testing.T) {
	for _, driver := range []string{"claude", "codex", "omp"} {
		t.Run(driver, func(t *testing.T) {
			f := newSessionFixture(t, driver)
			want := usageSummaryR(f.collect())
			dir := filepath.Dir(f.files[0])
			fifos := []string{filepath.Join(f.session.Home, "canary.jsonl"), filepath.Join(dir, "canary")}
			switch driver {
			case "codex":
				fifos = append(fifos, filepath.Join(dir, "rollout-x-11111111-2222-4333-8444-555555555555.jsonl"))
			case "claude":
				fifos = append(fifos, filepath.Join(dir, "11111111-2222-4333-8444-555555555555.jsonl"))
			}
			for _, path := range fifos {
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Skipf("mkfifo unavailable: %v", err)
				}
			}
			done := make(chan string, 1)
			go func() { done <- usageSummaryR(f.collect()) }()
			select {
			case got := <-done:
				if got != want {
					t.Fatalf("usage = %s, want %s", got, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("locator opened a file outside the run's session")
			}
		})
	}
	// A matching name that is not a regular file is refused, not opened.
	f := newSessionFixture(t, "omp")
	if err := os.Remove(f.files[0]); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(f.files[0], 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if got := usageSummaryR(f.collect()); got != unavailableHarnessUsage {
		t.Fatalf("usage = %s", got)
	}
}
