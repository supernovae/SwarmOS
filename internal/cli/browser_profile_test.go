package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/validation"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestBrowserProfileInternalTools(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cmd := newInternalToolsCmd()
		args := []string{"--workspace", t.TempDir()}
		if enabled {
			args = append(args, "--browser-profile")
		}
		cmd.SetArgs(args)
		cmd.SetIn(strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n"))
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		value, err := cmd.Flags().GetBool("browser-profile")
		if err != nil || value != enabled {
			t.Fatalf("startup opt-in: %v, %v", value, err)
		}
		if !strings.Contains(out.String(), "swarm_shell") || strings.Contains(out.String(), "browser_profile") {
			t.Fatalf("tool protocol changed: %s", out.String())
		}
	}
}

func TestBrowserProfileInternalHelperHidden(t *testing.T) {
	cmd := newInternalBrowserExecCmd()
	if !cmd.Hidden || !cmd.DisableFlagParsing {
		t.Fatal("scope helper must be hidden with literal argv")
	}
	if err := cmd.Args(cmd, []string{"only-unit"}); err == nil {
		t.Fatal("scope helper accepted missing executable")
	}
}

func TestBrowserProfileDoctor(t *testing.T) {
	for _, reason := range []string{"", "missing Bubblewrap", "missing Playwright Chromium cache", "unverifiable cgroup v2 memory cap", "TOKEN /home/private-user/secret"} {
		cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{BrowserProfile: true}}
		probe := func(context.Context) error {
			if reason == "" {
				return nil
			}
			return fmt.Errorf("%s", reason)
		}
		result := doctorBrowserProfileCheck(t.Context(), cfg, probe)
		if reason == "" {
			if result.Status != "OK" || !strings.Contains(result.Detail, "requested/available") {
				t.Fatalf("%+v", result)
			}
		} else if result.Status != "FAIL" || !strings.Contains(result.Detail, "requested/unavailable: unavailable") || strings.Contains(result.Detail, reason) {
			t.Fatalf("%+v", result)
		}
	}
	cfg := &contract.SwarmConfig{}
	result := doctorBrowserProfileCheck(t.Context(), cfg, func(context.Context) error { t.Fatal("disabled profile probed"); return nil })
	if result.Status != "OK" || !strings.Contains(result.Detail, "not requested") {
		t.Fatalf("%+v", result)
	}
}

// The live validator test exercises Chromium; this fixture independently
// exercises publication refusal for the resulting required-check failure.
func TestBrowserProfileSubmissionRefusesFailedAssertion(t *testing.T) {
	f := newCLIFixture(t)
	policy := "required_checks:\n  - name: browser-assertion\n    command: 'test -s browser-output/preview.png'\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(f.root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".")
	prGit(t, f.root, "commit", "-m", "browser required assertion")
	number := f.create(t, "none")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	if _, err := f.run("issue", "work", n, "--agent", "mock", "--progress=false"); !errors.Is(err, validation.ErrRequiredCheckFailed) {
		t.Fatalf("work failure: %v", err)
	}
	if _, err := f.run("issue", "submit", n, "--progress=false"); !errors.Is(err, validation.ErrRequiredCheckFailed) {
		t.Fatalf("submit failure: %v", err)
	}
	if state := f.state(t, number); state.PRNumber != 0 || state.PRURL != "" {
		t.Fatalf("failed browser assertion published: %+v", state)
	}
}
