package gitcredential

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/testutil"
)

func clearTokenEnv(t *testing.T) {
	t.Helper()
	for _, key := range append([]string{TokenCommandEnv}, tokenEnvKeys...) {
		t.Setenv(key, "")
	}
	resetTokenCache()
	t.Cleanup(resetTokenCache)
}

func stubTokenCommand(t *testing.T, out string, err error) *int {
	t.Helper()
	calls := 0
	previous := runTokenCommand
	runTokenCommand = func(context.Context, string) ([]byte, error) {
		calls++
		return []byte(out), err
	}
	t.Cleanup(func() { runTokenCommand = previous })
	return &calls
}

func TestGitHubTokenPrecedence(t *testing.T) {
	clearTokenEnv(t)
	stubTokenCommand(t, "from-command\n", nil)
	t.Setenv("GITHUB_TOKEN", "from-github-token")
	t.Setenv("GH_TOKEN", "from-gh-token")
	t.Setenv(TokenCommandEnv, "print-token")
	if token, source, err := GitHubToken(); err != nil || token != "from-command" || source != TokenCommandEnv {
		t.Fatalf("command should outrank GH_TOKEN: %q %q %v", token, source, err)
	}

	resetTokenCache()
	t.Setenv(TokenCommandEnv, "")
	if token, source, err := GitHubToken(); err != nil || token != "from-gh-token" || source != "GH_TOKEN" {
		t.Fatalf("GH_TOKEN should outrank GITHUB_TOKEN: %q %q %v", token, source, err)
	}

	resetTokenCache()
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	if token, source, err := GitHubToken(); err != nil || token != "" || source != "" {
		t.Fatalf("no source should resolve to empty: %q %q %v", token, source, err)
	}
}

func TestGitHubTokenRejectsAmbiguousSources(t *testing.T) {
	clearTokenEnv(t)
	calls := stubTokenCommand(t, "from-command", nil)
	t.Setenv("SWARM_GITHUB_TOKEN", "explicit")
	t.Setenv(TokenCommandEnv, "print-token")
	if _, _, err := GitHubToken(); err == nil {
		t.Fatal("expected an error when both SWARM_GITHUB_TOKEN and the command are set")
	}
	if *calls != 0 {
		t.Fatal("credential command ran despite ambiguous configuration")
	}
}

func TestGitHubTokenCommandRunsOnce(t *testing.T) {
	clearTokenEnv(t)
	calls := stubTokenCommand(t, "  once-token \n", nil)
	t.Setenv(TokenCommandEnv, "print-token")
	for i := 0; i < 3; i++ {
		if token, _, err := GitHubToken(); err != nil || token != "once-token" {
			t.Fatalf("unexpected token %q: %v", token, err)
		}
	}
	if *calls != 1 {
		t.Fatalf("credential command ran %d times", *calls)
	}
}

func TestGitHubTokenCommandFailuresDoNotLeakOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture; Windows command construction is covered separately")
	}
	for name, command := range map[string]string{
		"exit":      "printf partial-secret-value; exit 3",
		"empty":     "printf '   '",
		"multiline": "printf 'partial-secret-value\\nsecond'",
		"oversized": "head -c 70000 /dev/zero | tr '\\0' a; printf partial-secret-value",
	} {
		t.Run(name, func(t *testing.T) {
			clearTokenEnv(t)
			t.Setenv(TokenCommandEnv, command)
			token, _, err := GitHubToken()
			if err == nil || token != "" {
				t.Fatalf("expected failure, got token %q", token)
			}
			if strings.Contains(err.Error(), "partial-secret-value") {
				t.Fatalf("error leaked command output: %v", err)
			}
		})
	}
}

func TestCommandTokenStaysOutOfArgumentsAndAgentInputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture; Windows command construction is covered separately")
	}
	clearTokenEnv(t)
	const token = "synthetic-command-secret"
	script := filepath.Join(t.TempDir(), "token.sh")
	if err := testutil.WriteExecutable(script, []byte("#!/bin/sh\nprintf '%s\\n' "+token+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(TokenCommandEnv, script)
	// Raw command tests production credential/environment hardening via Configure.
	cmd := exec.Command("git", "ls-remote", "origin")
	redact, err := Configure(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), token) {
		t.Fatal("credential appears in argv")
	}
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, TokenCommandEnv+"=") {
			t.Fatal("credential command forwarded to Git child")
		}
		if strings.Contains(entry, token) {
			t.Fatal("raw token placed in Git child environment")
		}
	}
	if got := redact("failure " + token); strings.Contains(got, token) {
		t.Fatal("diagnostics leaked credentials")
	}
}
