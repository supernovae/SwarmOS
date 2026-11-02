package validation

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/sandbox"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBrowserProfileLiveRequiredChecks(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1")
	}
	if runtime.GOOS != "linux" {
		t.Skip("browser profile requires Linux")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skipf("cgroup v2 prerequisite: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(os.Getenv("PLAYWRIGHT_BROWSERS_PATH"), "chromium_headless_shell-*", "chrome-headless-shell-*", "chrome-headless-shell"))
	if len(matches) == 0 {
		t.Skip("cached Playwright Chromium headless shell unavailable; set PLAYWRIGHT_BROWSERS_PATH to the provisioned cache")
	}
	for _, binary := range []string{"bwrap", "systemd-run", "systemctl", "ldd"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("runtime prerequisite %s: %v", binary, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	manager := exec.CommandContext(ctx, "systemctl", "--user", "show-environment")
	runtimeDir := fmt.Sprintf("/run/user/%d", os.Getuid())
	manager.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "XDG_RUNTIME_DIR=" + runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus"}
	if err := manager.Run(); err != nil {
		t.Skipf("user-manager prerequisite: %v", err)
	}
	libraries, err := exec.CommandContext(ctx, "ldd", matches[0]).CombinedOutput()
	if err != nil || strings.Contains(string(libraries), "not found") {
		t.Skip("Chromium shared-library prerequisite unavailable")
	}
	if err := sandbox.CheckBrowserProfile(t.Context()); err != nil {
		t.Fatal(err)
	}
	repo := setupGitRepoForValidation(t)
	config := "version: 1\ndefault_agent: browser-tester\ndefault_gate: none\nexecution:\n  mode: isolated\n  backend: native\n  browser_profile: true\nlease: {duration: 1h, max_renewals: 1}\nsecurity: {require_isolation: true, require_ready_label: true, ready_label: swarm:ready}\nagents:\n  browser-tester:\n    binary: deliberately-unavailable-codex\n    harness: codex\n"
	commitRepoChange(t, repo, ".swarm/config.yaml", config)
	fixture, err := os.ReadFile("../sandbox/testdata/browser-preview.sh")
	if err != nil {
		t.Fatal(err)
	}
	commitRepoChange(t, repo, "browser-preview.sh", string(fixture))
	for _, fail := range []bool{false, true} {
		command := "sh browser-preview.sh"
		if fail {
			command += " && grep -q deliberate-missing-text browser-output/dom.html"
		}
		policy := "required_checks:\n  - name: browser\n    command: '" + command + "'\n    timeout: 1m\n  - name: still-runs\n    command: 'printf SECOND_CHECK_RAN'\n    timeout: 1m\nprotected_paths:\n  - .github/workflows/**\n"
		base := commitPolicy(t, repo, policy)
		// Candidate policy cannot disable the approved profile.
		if err := os.WriteFile(filepath.Join(repo, ".swarm/config.yaml"), []byte("execution:\n  browser_profile: false\n"), 0600); err != nil {
			t.Fatal(err)
		}
		report, err := Validate(t.Context(), repo, nil, ValidateOptions{BaseSHA: base, CIMode: true})
		if fail {
			if !errors.Is(err, ErrRequiredCheckFailed) || report == nil || report.Passed {
				t.Fatalf("failed browser assertion: %+v, %v", report, err)
			}
		} else if err != nil || report == nil || !report.Passed {
			t.Fatalf("passing browser check: %+v, %v", report, err)
		}
		if len(report.CheckOutcomes) != 2 || report.CheckOutcomes[1].Output != "SECOND_CHECK_RAN" {
			t.Fatalf("required check omitted: %+v", report.CheckOutcomes)
		}
		// Validation commands execute against disposable copies.
		if _, err := os.Stat(filepath.Join(repo, "browser-output")); !os.IsNotExist(err) {
			t.Fatalf("candidate check output imported: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".swarm/config.yaml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
