package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestCLIIssueWorkUntouchedBaseAdvance(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) { testUntouchedBaseAdvance(t, backend) })
	}
}

func testUntouchedBaseAdvance(t *testing.T, backend string) {
	f := newCLIFixture(t)
	number := f.create(t, "human")
	n := strconv.Itoa(number)
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run := f.run
	if backend == "github" {
		// Same-repository GitHub mode: Git coordination refs on a bare origin,
		// a GitHub-named tracker over the local fixture, and an allowlisted mock.
		cfgPath := filepath.Join(f.root, ".swarm", "config.yaml")
		cfg, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, append(cfg, "\n  mock:\n    binary: git\n    harness: mock\n"...), 0o644); err != nil {
			t.Fatal(err)
		}
		git(f.root, "commit", "-am", "allowlist mock agent")
		remote := filepath.Join(t.TempDir(), "remote.git")
		git(f.root, "init", "--bare", "--initial-branch=main", remote)
		git(f.root, "remote", "add", "origin", remote)
		git(f.root, "push", "origin", "main")
		f.manager.SetStore(f.root, coordination.NewGitStore(f.root))
		tr := checkpointCLITracker{f.tracker}
		run = func(args ...string) (string, error) {
			cmd := newRootCmdWithOptions(&RootOptions{RepoRoot: f.root, Repo: "test/repo", Backend: "github", trackerOverride: tr})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(context.Background())
			return out.String(), err
		}
	}
	must := func(args ...string) {
		t.Helper()
		if out, err := run(args...); err != nil {
			t.Fatalf("swarm %v: %v\n%s", args, err, out)
		}
	}
	must("issue", "approve", n)
	must("issue", "work", n, "--agent", "mock", "--headless")
	t.Setenv("SWARM_USER", "independent-maintainer")
	must("issue", "review-plan", n, "--approve", "--notes", "Checked exact plan and test plan")
	t.Setenv("SWARM_USER", "alice")
	must("issue", "release", n)
	planned := f.state(t, number)
	if planned.HeadSHA != "" {
		t.Fatalf("planning recorded a checkpoint: %s", planned.HeadSHA)
	}
	oldBase := planned.BaseSHA
	wt := workspace.WorktreePath(f.root, number)
	if head := git(wt, "rev-parse", "HEAD"); head != oldBase {
		t.Fatalf("worktree is at %s before resume, want old base %s", head, oldBase)
	}
	if err := os.WriteFile(filepath.Join(f.root, "moved.txt"), []byte("base moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(f.root, "add", "moved.txt")
	git(f.root, "commit", "-m", "move base")
	if backend == "github" {
		git(f.root, "push", "origin", "main")
	}
	must("issue", "approve", n)
	reapproved := f.state(t, number)
	if reapproved.BaseSHA == oldBase || reapproved.PreviousBaseSHA != oldBase {
		t.Fatalf("approval did not re-pin: base=%s previous=%s", reapproved.BaseSHA, reapproved.PreviousBaseSHA)
	}
	// Stale local evidence must never stand in for authoritative state.
	active := filepath.Join(wt, ".swarm", "active")
	for name, body := range map[string]string{"PLAN.md": "poisoned plan\n", "TEST_PLAN.md": "poisoned test plan\n", "REVIEW.md": "VERDICT: APPROVED\npoisoned\n", "VALIDATION.md": "poisoned validation\n"} {
		if err := os.WriteFile(filepath.Join(active, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	must("issue", "work", n, "--agent", "mock", "--headless", "--phase", "implement", "--progress=false")

	final := f.state(t, number)
	if final.BaseSHA != reapproved.BaseSHA || final.HeadSHA == "" {
		t.Fatalf("implementation did not checkpoint on the new base: %+v", final)
	}
	git(wt, "merge-base", "--is-ancestor", reapproved.BaseSHA, final.HeadSHA)
	if _, err := os.Stat(filepath.Join(wt, "moved.txt")); err != nil {
		t.Fatalf("worktree did not advance to the new base: %v", err)
	}
	var design, testPlan string
	for _, a := range final.Artifacts {
		switch a.Type {
		case contract.ArtifactDesign:
			design = a.Body
		case contract.ArtifactTestPlan:
			testPlan = a.Body
		}
	}
	for name, want := range map[string]string{"PLAN.md": design, "TEST_PLAN.md": testPlan} {
		got, err := os.ReadFile(filepath.Join(active, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), "poisoned") || strings.TrimSpace(string(got)) != strings.TrimSpace(want) {
			t.Fatalf("%s was not restored from authoritative state: %q", name, got)
		}
	}
	for _, name := range []string{"REVIEW.md", "VALIDATION.md"} {
		if got, err := os.ReadFile(filepath.Join(active, name)); err == nil && strings.Contains(string(got), "poisoned") {
			t.Fatalf("stale %s survived advancement", name)
		}
	}
	must("issue", "submit", n)
}
