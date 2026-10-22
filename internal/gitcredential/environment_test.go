package gitcredential

import (
	"context"
	"encoding/base64"
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
	t.Setenv("GIT_TRACE_CURL", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "untrusted-helper")
	cmd := exec.Command("git", "ls-remote", "origin")
	redact := Configure(cmd)
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
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git %v: %v: %s", args, err, out)
		}
	}
	git("init")
	git("config", "user.name", "Security fixture")
	git("config", "user.email", "fixture@example.invalid")
	script := filepath.Join(root, "helper.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf invoked > helper-invoked\ncat\n"), 0700); err != nil {
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
	if err := os.WriteFile(filepath.Join(root, "hooks", "pre-commit"), []byte("#!/bin/sh\nprintf invoked > hook-invoked\n"), 0700); err != nil {
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
