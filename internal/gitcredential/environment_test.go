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
	"sync"
	"sync/atomic"
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

func TestObservationCommandRejectsMutationAndCredentials(t *testing.T) {
	for _, args := range [][]string{{"fetch"}, {"worktree", "prune"}, {"status"}, {"show", "HEAD"}} {
		if _, err := ObservationCommand(context.Background(), t.TempDir(), args...); err == nil {
			t.Fatal("accepted", args)
		}
	}
	cmd, err := ObservationCommand(context.Background(), t.TempDir(), "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range cmd.Env {
		if strings.HasPrefix(value, "SWARM_") || strings.HasPrefix(value, "GH_TOKEN=") || strings.HasPrefix(value, "SSH_AUTH_SOCK=") {
			t.Fatal(value)
		}
	}
}

func TestLocalObservationNeverResolvesCredentials(t *testing.T) {
	t.Setenv("SWARM_GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	marker := filepath.Join(t.TempDir(), "token-command")
	t.Setenv(TokenCommandEnv, "touch "+marker)
	resetTokenCache()
	t.Cleanup(resetTokenCache)
	ctx := WithLocalObservation(context.Background())
	cmd, _, err := Command(ctx, t.TempDir(), "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range cmd.Env {
		if strings.HasPrefix(item, TokenCommandEnv+"=") {
			t.Fatal("forwarded token command")
		}
	}
	if _, err := Output(ctx, t.TempDir(), "ls-remote", "origin"); err == nil {
		t.Fatal("allowed remote verification")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("resolved credentials", err)
	}
	t.Setenv("GH_TOKEN", "secret-for-observation")
	cmd, _, err = Command(ctx, t.TempDir(), "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cmd.Env, "\n"), "secret-for-observation") {
		t.Fatal("forwarded credentials")
	}
}

func TestCommandLookupCopiesAndKeys(t *testing.T) {
	s := NewCommandLookupSession()
	calls := 0
	s.run = func(_ context.Context, root string, args ...string) ([]byte, error) {
		calls++
		return []byte(root + strings.Join(args, " ")), nil
	}
	ctx := WithCommandLookupSession(context.Background(), s)
	sha := strings.Repeat("a", 40)
	_, err := ReadBaseFile(ctx, t.TempDir(), sha, "config")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	first, err := ReadBaseFile(ctx, root, sha, "config")
	if err != nil {
		t.Fatal(err)
	}
	want := string(first)
	first[0] = '!'
	second, err := ReadBaseFile(ctx, root, sha, "config")
	if err != nil || string(second) != want || calls != 2 {
		t.Fatalf("copy/reuse: calls=%d bytes=%q err=%v", calls, second, err)
	}
	for _, key := range []struct{ root, sha, path string }{{root, strings.Repeat("b", 40), "config"}, {root, sha, "other"}, {t.TempDir(), sha, "config"}} {
		if _, err := ReadBaseFile(ctx, key.root, key.sha, key.path); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 5 {
		t.Fatalf("key partitioning: %d calls", calls)
	}
}

func TestCommandLookupMutableRefs(t *testing.T) {
	s := NewCommandLookupSession()
	calls := 0
	s.run = func(context.Context, string, ...string) ([]byte, error) { calls++; return []byte{byte(calls)}, nil }
	// Ineligible refs must bypass even the session runner: fake Git returns current bytes.
	root := t.TempDir()
	count := filepath.Join(root, "count")
	script := filepath.Join(root, "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf x >> '"+count+"'\ncat '"+count+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := WithCommandLookupSession(context.Background(), s)
	for _, ref := range []string{"main", "abcdef1", strings.Repeat("a", 64)} {
		a, err := ReadBaseFile(ctx, root, ref, "config")
		if err != nil {
			t.Fatal(err)
		}
		b, err := ReadBaseFile(ctx, root, ref, "config")
		if err != nil {
			t.Fatal(err)
		}
		if string(a) == string(b) {
			t.Fatalf("cached mutable ref %s", ref)
		}
	}
	if calls != 0 {
		t.Fatal("ineligible refs entered cache")
	}
}

func TestCommandLookupConcurrentAndRetry(t *testing.T) {
	s := NewCommandLookupSession()
	var calls atomic.Int32
	s.run = func(context.Context, string, ...string) ([]byte, error) { calls.Add(1); return []byte("ok"), nil }
	ctx := WithCommandLookupSession(context.Background(), s)
	root := t.TempDir()
	sha := strings.Repeat("a", 40)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ReadBaseFile(ctx, root, sha, "config"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent calls: %d", calls.Load())
	}
	s = NewCommandLookupSession()
	attempts := 0
	s.run = func(context.Context, string, ...string) ([]byte, error) {
		attempts++
		if attempts == 1 {
			return nil, context.Canceled
		}
		return []byte("ok"), nil
	}
	ctx = WithCommandLookupSession(context.Background(), s)
	if _, err := ReadBaseFile(ctx, root, sha, "config"); err == nil {
		t.Fatal("missing failure")
	}
	if _, err := ReadBaseFile(ctx, root, sha, "config"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatal(attempts)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ReadBaseFile(canceled, root, sha, "config"); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestCommonDirectoryCommandCache(t *testing.T) {
	root := t.TempDir()
	calls := 0
	session := func() *CommandLookupSession {
		s := NewCommandLookupSession()
		s.run = func(context.Context, string, ...string) ([]byte, error) {
			calls++
			if calls == 1 {
				return nil, context.Canceled
			}
			return []byte("/common"), nil
		}
		return s
	}
	ctx := WithCommandLookupSession(context.Background(), session())
	if _, err := CommonDirectory(ctx, root); err == nil {
		t.Fatal("expected failure")
	}
	for i := 0; i < 2; i++ {
		if _, err := CommonDirectory(ctx, root); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	if _, err := CommonDirectory(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := CommonDirectory(WithCommandLookupSession(ctx, session()), root); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatal(calls)
	}
}

func TestCommandLookupCanceledWaiter(t *testing.T) {
	s := NewCommandLookupSession()
	started, release := make(chan struct{}), make(chan struct{})
	s.run = func(context.Context, string, ...string) ([]byte, error) {
		close(started)
		<-release
		return []byte("ok"), nil
	}
	ctx := WithCommandLookupSession(context.Background(), s)
	root := t.TempDir()
	sha := strings.Repeat("a", 40)
	done := make(chan error, 1)
	go func() { _, err := ReadBaseFile(ctx, root, sha, "config"); done <- err }()
	<-started
	waiter, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ReadBaseFile(waiter, root, sha, "config"); err != context.Canceled {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if data, err := ReadBaseFile(ctx, root, sha, "config"); err != nil || string(data) != "ok" {
		t.Fatalf("waiter poisoned owner: %q %v", data, err)
	}
}

func TestCommonDirectoryCommandCacheInvalidOutputRetries(t *testing.T) {
	s := NewCommandLookupSession()
	calls := 0
	s.run = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("relative"), nil
		}
		return []byte("/common"), nil
	}
	ctx := WithCommandLookupSession(context.Background(), s)
	root := t.TempDir()
	if _, err := CommonDirectory(ctx, root); err == nil {
		t.Fatal("accepted relative common directory")
	}
	for i := 0; i < 2; i++ {
		if _, err := CommonDirectory(ctx, root); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("invalid output cached or successful output not reused: %d", calls)
	}
}
