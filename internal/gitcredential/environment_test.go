package gitcredential

import (
	"context"
	"encoding/base64"
	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCredentialsNeverEnterArgumentsOrDiagnostics(t *testing.T) {
	const token = "synthetic-secret-for-test"
	t.Setenv("SWARM_GITHUB_TOKEN", token)
	t.Setenv(TokenCommandEnv, "")
	resetTokenCache()
	t.Cleanup(resetTokenCache)
	t.Setenv("GIT_TRACE_CURL", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "untrusted-helper")
	// Raw command tests production credential/environment hardening via Configure.
	cmd := exec.Command("git", "ls-remote", "origin")
	redact, err := Configure(cmd)
	if err != nil {
		t.Fatal(err)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, token) || strings.Contains(args, basic) {
		t.Fatal("credential appears in argv")
	}
	env := strings.Join(cmd.Env, "\n")
	for _, forbidden := range []string{"GIT_TRACE_CURL=", "SWARM_GITHUB_TOKEN=", "untrusted-helper"} {
		if strings.Contains(env, forbidden) {
			t.Fatalf("inherited forbidden credential/config: %s", forbidden)
		}
	}
	if !strings.Contains(env, "http.https://github.com/.extraheader") || !strings.Contains(env, "AUTHORIZATION: basic "+basic) {
		t.Fatal("missing scoped authentication configuration")
	}
	if got := redact("failure " + token + " " + basic); strings.Contains(got, token) || strings.Contains(got, basic) {
		t.Fatal("diagnostics leaked credentials")
	}
}

func TestCoordinatorGitDoesNotExecuteCandidateSelectedHelpers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell canary fixture; Windows command construction is covered separately")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := gitfixture.Command(t, "", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git %v: %v: %s", args, err, out)
		}
	}
	git("init")
	git("config", "user.name", "Security fixture")
	git("config", "user.email", "fixture@example.invalid")
	script := filepath.Join(root, "helper.sh")
	if err := testutil.WriteExecutable(script, []byte("#!/bin/sh\nprintf invoked > helper-invoked\ncat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	git("config", "filter.candidate.clean", script)
	git("config", "filter.candidate.smudge", script)
	git("config", "filter.candidate.process", script)
	git("config", "filter.candidate.required", "true")
	git("config", "core.fsmonitor", script)
	git("config", "core.hooksPath", filepath.Join(root, "hooks"))
	git("config", "commit.gpgsign", "true")
	git("config", "gpg.program", script)
	if err := os.Mkdir(filepath.Join(root, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := testutil.WriteExecutable(filepath.Join(root, "hooks", "pre-commit"), []byte("#!/bin/sh\nprintf invoked > hook-invoked\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.txt filter=candidate diff=candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "candidate.txt"), []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("config", "diff.candidate.command", script)
	git("config", "diff.candidate.textconv", script)
	for _, args := range [][]string{{"status", "--porcelain"}, {"add", "candidate.txt", ".gitattributes"}, {"commit", "-m", "candidate"}, {"show", "HEAD"}} {
		cmd, _, err := Command(context.Background(), root, args...)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hardened git %v: %v: %s", args, err, out)
		}
	}
	for _, name := range []string{"helper-invoked", "hook-invoked"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("host helper ran: %s (%v)", name, err)
		}
	}
}

func TestMergeTreeNeutralizesCustomDriver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell sentinel")
	}
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := gitfixture.Command(t, "", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	write(".gitattributes", "file merge=candidate filter=candidate\n")
	write("file", "base\n")
	git("add", ".")
	git("commit", "-m", "base")
	git("checkout", "-b", "candidate")
	write("file", "candidate\n")
	git("commit", "-am", "candidate")
	head := git("rev-parse", "HEAD")
	git("checkout", "main")
	write("file", "target\n")
	git("commit", "-am", "target")
	git("config", "merge.renormalize", "true")
	git("config", "filter.candidate.clean", "touch filter-sentinel; cat")
	git("config", "merge.candidate.driver", "touch sentinel; true")
	git("config", "merge.candidate.recursive", "candidate")
	cmd, _, err := Command(context.Background(), root, "merge-tree", "--write-tree", "--name-only", "-z", "--no-messages", "HEAD", head)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "merge.candidate.driver") || !strings.Contains(env, "=false") || !strings.Contains(env, "merge.candidate.recursive") || !strings.Contains(env, "=binary") {
		t.Fatal("missing driver neutralization")
	}
	_, err = cmd.Output()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("expected conservative conflict: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sentinel")); !os.IsNotExist(err) {
		t.Fatal("custom driver executed")
	}
	if _, err := os.Stat(filepath.Join(root, "filter-sentinel")); !os.IsNotExist(err) {
		t.Fatal("custom filter executed")
	}
	if got := git("config", "merge.candidate.driver"); got != "touch sentinel; true" {
		t.Fatal("persistent configuration changed")
	}
}

func TestMergeTreeConfigurationProbeFailsClosed(t *testing.T) {
	root := t.TempDir()
	cmd := gitfixture.Command(t, "", "init", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if cmd, _, err := Command(context.Background(), root, "merge-tree", "--write-tree", "HEAD", "HEAD"); err == nil || cmd != nil {
		t.Fatal("invalid configuration allowed inspection")
	}
}

func TestMergeTreeConfigurationBudgetFailsClosed(t *testing.T) {
	root := t.TempDir()
	cmd := gitfixture.Command(t, "", "init", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	config := "[merge \"" + strings.Repeat("x", (1<<20)+1) + "\"]\n driver = true\n"
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if cmd, _, err := Command(context.Background(), root, "merge-tree", "--write-tree", "HEAD", "HEAD"); err == nil || cmd != nil {
		t.Fatal("oversized driver configuration allowed inspection")
	}
}

func TestForkReadRejectsUnsafeRequests(t *testing.T) {
	for _, tc := range []struct{ host, slug, op, value string }{
		{"github.com", "alice/repo", "push", "main"}, {"github.com", "alice/repo", "fetch", "HEAD"}, {"github.com", "alice/repo", "ls-remote", "-x"}, {"github.com", "alice/repo", "ls-remote", "main:other"}, {"github.com", "alice/../repo", "fetch", strings.Repeat("a", 40)}, {"evil:443", "alice/repo", "fetch", strings.Repeat("a", 40)}, {"github.com", "alice/repo?x", "ls-remote", "main"},
	} {
		if _, err := ForkRead(context.Background(), t.TempDir(), tc.host, tc.slug, "sentinel", tc.op, tc.value); err == nil {
			t.Errorf("accepted %+v", tc)
		}
	}
}
func TestForkReadRejectsRepositoryTransportOverrides(t *testing.T) {
	for _, key := range []string{"url.https://evil/.insteadOf", "http.extraHeader", "credential.helper", "http.followRedirects", "include.path", "fetch.recurseSubmodules", "core.alternateRefsCommand", "remote.https://github.com/alice/repo.git.url"} {
		t.Run(key, func(t *testing.T) {
			root := t.TempDir()
			cmd := exec.Command("git", "init", root)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("init: %s %v", out, err)
			}
			cmd = exec.Command("git", "-C", root, "config", key, "hostile")
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			_, err := ForkRead(context.Background(), root, "github.com", "alice/repo", "sentinel", "ls-remote", "main")
			if err == nil || !strings.Contains(err.Error(), "configuration") {
				t.Fatalf("override not rejected: %v", err)
			}
		})
	}
}
func TestNormalGitScrubsForkReadTokenWithoutWriter(t *testing.T) {
	for _, name := range []string{"SWARM_GITHUB_TOKEN", "SWARM_GITHUB_TOKEN_COMMAND", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(name, "")
	}
	t.Setenv("SWARM_GITHUB_FORK_READ_TOKEN", "read-only-sentinel")
	cmd := exec.Command("git", "version")
	if _, err := Configure(cmd); err != nil {
		t.Fatal(err)
	}
	for _, entry := range cmd.Env {
		if strings.Contains(entry, "read-only-sentinel") || strings.HasPrefix(entry, "SWARM_GITHUB_FORK_READ_TOKEN=") {
			t.Fatal("read credential inherited")
		}
	}
}
