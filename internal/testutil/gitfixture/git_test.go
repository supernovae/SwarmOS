package gitfixture

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicy(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "gc.auto")
	t.Setenv("GIT_CONFIG_VALUE_0", "99")
	t.Setenv("GIT_DIR", "/missing")
	root := Init(t, "", "-b", "fixture")
	for key, value := range map[string]string{"gc.auto": "1", "maintenance.auto": "true"} {
		Run(t, root, "config", key, value)
	}
	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false"} {
		if got := strings.TrimSpace(Run(t, root, "config", "--get", key)); got != want {
			t.Fatalf("%s=%q", key, got)
		}
	}
	if got := strings.TrimSpace(Run(t, root, "symbolic-ref", "--short", "HEAD")); got != "fixture" {
		t.Fatal(got)
	}
	bare := Init(t, "", "--bare")
	if strings.TrimSpace(Run(t, bare, "rev-parse", "--is-bare-repository")) != "true" {
		t.Fatal("not bare")
	}
	cmd := Command(t, root, "hash-object", "--stdin")
	cmd.Stdin = strings.NewReader("exact\n")
	out, err := cmd.Output()
	path := filepath.Join(root, "input")
	if err := os.WriteFile(path, []byte("exact\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err != nil || string(out) != Run(t, root, "hash-object", path) {
		t.Fatalf("stdin output %q: %v", out, err)
	}
	Run(t, root, "add", "input")
	if got := Commit(t, root, "staged"); got != strings.TrimSpace(Run(t, root, "rev-parse", "HEAD")) {
		t.Fatal("commit helper returned a different HEAD")
	}
	if err := Command(t, root, "rev-parse", "missing").Run(); err == nil {
		t.Fatal("expected failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CommandContext(t, ctx, root, "status").Run(); err == nil {
		t.Fatal("expected cancellation")
	}
	seen := map[string]bool{}
	env := map[string]string{}
	for _, entry := range Command(t, root, "status").Env {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
		if seen[key] {
			t.Fatalf("duplicate %s", key)
		}
		seen[key] = true
	}
	if env["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Fatal("system config enabled")
	}
	if data, err := os.ReadFile(env["GIT_CONFIG_GLOBAL"]); err != nil || len(data) != 0 {
		t.Fatalf("global config not isolated: %q %v", data, err)
	}
}

func TestIgnoresHostGlobalConfig(t *testing.T) {
	home := t.TempDir()
	hostile := "[fixture]\n sentinel = hostile\n[user]\n name = Host\n email = host@example.invalid\n"
	for i, path := range []string{filepath.Join(home, ".gitconfig"), filepath.Join(home, "git", "config"), filepath.Join(home, "inherited")} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(hostile, "hostile", []string{"home", "xdg", "inherited"}[i])), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "inherited"))
	t.Setenv("GIT_AUTHOR_NAME", "Host")
	t.Setenv("GIT_COMMITTER_EMAIL", "host@example.invalid")
	root := Init(t, "")
	if out, err := Command(t, root, "config", "--get", "fixture.sentinel").Output(); err == nil || len(out) != 0 {
		t.Fatalf("host config read: %q %v", out, err)
	}
	Commit(t, root, "identity", "--allow-empty")
	if got := Run(t, root, "log", "-1", "--format=%an <%ae>|%cn <%ce>"); got != "Fixture <fixture@example.invalid>|Fixture <fixture@example.invalid>\n" {
		t.Fatal(got)
	}
}

func TestCommitDoesNotSpawnMaintenance(t *testing.T) {
	root := Init(t, "")
	for key, value := range map[string]string{"gc.auto": "1", "maintenance.auto": "true", "maintenance.strategy": "incremental"} {
		Run(t, root, "config", key, value)
	}
	trace := filepath.Join(t.TempDir(), "trace.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	if len(Commit(t, root, "trace", "--allow-empty")) != 40 {
		t.Fatal("missing commit")
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	start, exit := false, false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		args := strings.Join(event.Argv, " ")
		if event.Event == "start" && strings.Contains(args, "commit") {
			start = true
		}
		if event.Event == "exit" {
			exit = true
		}
		if event.Event == "child_start" && (strings.Contains(args, "maintenance run") || strings.Contains(args, "gc --auto")) {
			t.Fatalf("maintenance child: %s", line)
		}
	}
	if !start || !exit {
		t.Fatal("missing trace start/exit")
	}
}

func TestRunReportsStderr(t *testing.T) {
	if os.Getenv("GITFIXTURE_FAIL") == "1" {
		Run(t, t.TempDir(), "fixture-invalid-command")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunReportsStderr$")
	cmd.Env = append(os.Environ(), "GITFIXTURE_FAIL=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "is not a git command") {
		t.Fatalf("missing failure diagnostics: %v %s", err, out)
	}
}
