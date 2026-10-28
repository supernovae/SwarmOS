package main

import (
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/cli"
	"github.com/supernovae/swarmos/internal/harness"
)

// TestMain lets the test binary act as the swarm executable, so exit codes are
// observed at the real process boundary.
func TestMain(m *testing.M) {
	if os.Getenv("SWARM_MAIN_HELPER") == "1" {
		os.Args = append([]string{"swarm"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type process struct {
	t    *testing.T
	root string
}

func (p process) run(args ...string) (string, int) {
	p.t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"--root", p.root, "--backend", "local"}, args...)...)
	cmd.Env = append(os.Environ(), "SWARM_MAIN_HELPER=1")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		p.t.Fatal(err)
	}
	return string(out), 0
}

func (p process) must(args ...string) string {
	p.t.Helper()
	out, code := p.run(args...)
	if code != 0 {
		p.t.Fatalf("swarm %v exited %d: %s", args, code, out)
	}
	return out
}

func (p process) git(args ...string) string {
	p.t.Helper()
	out, err := gitfixture.Command(p.t, "", append([]string{"-C", p.root}, args...)...).CombinedOutput()
	if err != nil {
		p.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (p process) create(title string) string {
	p.t.Helper()
	var created struct {
		Issue struct {
			Number int `json:"number"`
		} `json:"issue"`
	}
	out := p.must("issue", "create", "--title", title, "--objective", "Exercise exit codes", "--constraint", "Keep policy unchanged", "--acceptance", "Exit with the documented code", "--gate", "agent", "--json")
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.Issue.Number == 0 {
		p.t.Fatalf("issue create: %v %s", err, out)
	}
	return strconv.Itoa(created.Issue.Number)
}

func newProcess(t *testing.T) process {
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SWARM_USER", "alice")
	t.Setenv("SWARM_REGISTRY_PATH", filepath.Join(t.TempDir(), "repos.yaml"))
	t.Setenv("SWARM_NODE_CONFIG", filepath.Join(t.TempDir(), "node.yaml"))
	t.Setenv("SWARM_BACKEND", "")
	t.Setenv("SWARM_REPO", "")
	p := process{t: t, root: t.TempDir()}
	p.git("init", "-b", "main")
	p.git("config", "user.name", "Exit Test")
	p.git("config", "user.email", "test@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: p.root}); err != nil {
		t.Fatal(err)
	}
	policy := "required_checks:\n  - name: git-available\n    command: git --version\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(p.root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	p.git("add", ".")
	p.git("commit", "-m", "approved harness")
	return p
}

func TestProcessExitCodes(t *testing.T) {
	p := newProcess(t)
	if _, code := p.run("issue", "work", "not-a-number"); code != cli.ExitFailure {
		t.Fatalf("usage error exit = %d", code)
	}

	unapproved := p.create("Unapproved")
	for _, mode := range [][]string{nil, {"--json"}} {
		if _, code := p.run(append([]string{"issue", "work", unapproved, "--agent", "mock", "--progress=false"}, mode...)...); code != cli.ExitNeedsApproval {
			t.Fatalf("needs approval %v exit = %d", mode, code)
		}
	}

	for _, mode := range [][]string{nil, {"--json"}} {
		rejected := p.create("Rejected")
		p.must("issue", "approve", rejected)
		t.Setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
		if _, code := p.run(append([]string{"issue", "work", rejected, "--agent", "mock", "--progress=false"}, mode...)...); code != cli.ExitChangesRequested {
			t.Fatalf("changes requested %v exit = %d", mode, code)
		}
	}

	t.Setenv("SWARM_MOCK_VERDICT", "APPROVED")
	issue := p.create("Conflicting")
	p.must("issue", "approve", issue)
	p.must("issue", "work", issue, "--agent", "mock", "--progress=false")
	var submitted struct {
		PRNumber int    `json:"pr_number"`
		HeadSHA  string `json:"head_sha"`
	}
	if out := p.must("issue", "submit", issue, "--json", "--progress=false"); json.Unmarshal([]byte(out), &submitted) != nil || submitted.PRNumber == 0 {
		t.Fatalf("submit: %s", out)
	}
	changed := strings.Fields(p.git("diff", "--name-only", "main", submitted.HeadSHA))
	if len(changed) == 0 {
		t.Fatal("PR changed no files")
	}
	if err := os.WriteFile(filepath.Join(p.root, changed[0]), []byte("conflicting upstream content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.git("add", changed[0])
	p.git("commit", "-m", "Conflicting upstream change")
	pull := strconv.Itoa(submitted.PRNumber)
	if _, code := p.run("pr", "rebase", pull); code != cli.ExitNeedsApproval {
		t.Fatalf("moved base exit = %d", code)
	}
	p.must("issue", "approve", issue)
	for _, mode := range [][]string{nil, {"--json"}} {
		if _, code := p.run(append([]string{"pr", "rebase", pull}, mode...)...); code != cli.ExitConflict {
			t.Fatalf("conflict %v exit = %d", mode, code)
		}
	}
	p.must("pr", "rebase", pull, "--abort")
}
