package workspace

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type advanceFixture struct {
	root, path, branch, previous, base string
}

func (f advanceFixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func advanceWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newAdvanceFixture provisions an untouched task worktree at the previous
// base, then moves main to a descendant base that edits and adds files.
func newAdvanceFixture(t *testing.T) advanceFixture {
	t.Helper()
	f := advanceFixture{root: t.TempDir()}
	f.git(t, f.root, "init", "-b", "main")
	f.git(t, f.root, "config", "user.name", "Test")
	f.git(t, f.root, "config", "user.email", "test@example.test")
	advanceWrite(t, f.root, "AGENTS.md", "instructions\n")
	advanceWrite(t, f.root, "source", "old\n")
	advanceWrite(t, f.root, ".gitignore", "*.log\nbuild/\n")
	f.git(t, f.root, "add", ".")
	f.git(t, f.root, "commit", "-m", "old")
	f.previous = f.git(t, f.root, "rev-parse", "HEAD")
	var err error
	f.path, f.branch, err = ProvisionAt(f.root, 7, "task", f.previous, "", "")
	if err != nil {
		t.Fatal(err)
	}
	advanceWrite(t, f.root, "source", "new\n")
	advanceWrite(t, f.root, "added.log", "tracked despite ignore\n")
	advanceWrite(t, f.root, "added.txt", "added\n")
	f.git(t, f.root, "add", "-f", "source", "added.log", "added.txt")
	f.git(t, f.root, "commit", "-m", "new")
	f.base = f.git(t, f.root, "rev-parse", "HEAD")
	return f
}

// snapshot captures refs, raw index bytes, status and file bytes.
func (f advanceFixture) snapshot(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(f.git(t, f.root, "rev-parse", "refs/heads/"+f.branch) + "\n")
	index := f.git(t, f.path, "rev-parse", "--path-format=absolute", "--git-path", "index")
	raw, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	b.Write(raw)
	b.WriteString(f.git(t, f.path, "--no-optional-locks", "status", "--porcelain", "--ignored", "--untracked-files=all") + "\n")
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "source", "added.log", "extra", ".swarm/active/PLAN.md"} {
		data, err := os.ReadFile(filepath.Join(f.path, name))
		if err == nil {
			b.WriteString(name + ":" + string(data) + "\n")
		}
	}
	return b.String()
}

func TestProvisionUntouchedBase(t *testing.T) {
	pointer := "<!-- swarm:active:start -->\npointer\n<!-- swarm:active:end -->\n\n"
	for _, scenario := range []string{"clean", "markers", "staged markers", "branch only", "repeat"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdvanceFixture(t)
			advanceWrite(t, f.path, ".swarm/active/PLAN.md", "stale plan\n")
			advanceWrite(t, f.path, ".swarm/active/REVIEW.md", "VERDICT: APPROVED\nstale\n")
			advanceWrite(t, f.path, ".swarm/active/REPAIR.md", "stale brief\n")
			advanceWrite(t, f.path, ".swarm/active/LESSONS.md", "kept\n")
			switch scenario {
			case "markers", "staged markers":
				advanceWrite(t, f.path, "AGENTS.md", pointer+"instructions\n")
				advanceWrite(t, f.path, "CLAUDE.md", pointer)
				if scenario == "staged markers" {
					f.git(t, f.path, "add", "AGENTS.md")
				}
			case "branch only":
				f.git(t, f.root, "worktree", "remove", "--force", f.path)
			}
			pc := ProvisionContext{PreviousBase: f.previous}
			path, branch, err := ProvisionWithContext(f.root, "origin", 7, "task", f.base, "", "", pc)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "repeat" {
				if path, branch, err = ProvisionWithContext(f.root, "origin", 7, "task", f.base, "", "", pc); err != nil {
					t.Fatalf("retry at the new base: %v", err)
				}
			}
			if path != f.path || branch != f.branch {
				t.Fatalf("provisioned %s %s", path, branch)
			}
			if head := f.git(t, path, "rev-parse", "HEAD"); head != f.base {
				t.Fatalf("HEAD %s, want %s", head, f.base)
			}
			if status := f.git(t, path, "status", "--porcelain", "--untracked-files=all"); status != "" {
				t.Fatalf("worktree not clean at new base: %q", status)
			}
			if got, _ := os.ReadFile(filepath.Join(path, "source")); string(got) != "new\n" {
				t.Fatalf("source = %q", got)
			}
			if scenario != "branch only" {
				for _, name := range []string{"PLAN.md", "REVIEW.md", "REPAIR.md"} {
					if _, err := os.Stat(filepath.Join(path, ".swarm", "active", name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("stale %s kept: %v", name, err)
					}
				}
				if got, _ := os.ReadFile(filepath.Join(path, ".swarm", "active", "LESSONS.md")); string(got) != "kept\n" {
					t.Fatal("lessons removed")
				}
			}
		})
	}
}

func TestProvisionUntouchedBaseRefusals(t *testing.T) {
	for _, scenario := range []string{"commit", "dirty", "staged", "untracked", "instruction edit", "mode", "symlink", "ignored collision", "divergent base", "previous mismatch", "wrong branch"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAdvanceFixture(t)
			previous := f.previous
			advanceWrite(t, f.path, ".swarm/active/PLAN.md", "stale plan\n")
			switch scenario {
			case "commit":
				advanceWrite(t, f.path, "extra", "work\n")
				f.git(t, f.path, "add", "extra")
				f.git(t, f.path, "commit", "-m", "task work")
			case "dirty":
				advanceWrite(t, f.path, "source", "user edit\n")
			case "staged":
				advanceWrite(t, f.path, "source", "user edit\n")
				f.git(t, f.path, "add", "source")
			case "untracked":
				advanceWrite(t, f.path, "extra", "user file\n")
			case "instruction edit":
				advanceWrite(t, f.path, "AGENTS.md", "<!-- swarm:active:start -->\np\n<!-- swarm:active:end -->\n\nuser edit\n")
			case "mode":
				if err := os.Chmod(filepath.Join(f.path, "source"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(filepath.Join(f.path, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("source", filepath.Join(f.path, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
			case "ignored collision":
				advanceWrite(t, f.path, "added.log", "ignored user output\n")
			case "divergent base":
				f.git(t, f.root, "checkout", "-q", "--orphan", "other")
				advanceWrite(t, f.root, "source", "unrelated\n")
				f.git(t, f.root, "add", "source")
				f.git(t, f.root, "commit", "-m", "unrelated")
				f.base = f.git(t, f.root, "rev-parse", "HEAD")
			case "previous mismatch":
				// Re-approved twice: state names an intermediate base.
				previous = f.base
				f.git(t, f.root, "commit", "--allow-empty", "-m", "newer")
				f.base = f.git(t, f.root, "rev-parse", "HEAD")
			case "wrong branch":
				f.git(t, f.path, "switch", "-q", "-c", "elsewhere")
			}
			before := f.snapshot(t)
			_, _, err := ProvisionWithContext(f.root, "origin", 7, "task", f.base, f.branch, "", ProvisionContext{PreviousBase: previous})
			if err == nil {
				t.Fatal("ineligible worktree was advanced")
			}
			for _, want := range []string{"git worktree remove --force -- '" + f.path + "'", "swarm issue work 7"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err, want)
				}
			}
			after := f.snapshot(t)
			if after != before {
				t.Fatalf("refusal changed the worktree:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}

func TestProvisionContextIgnoredOutsideSameRepositoryTasks(t *testing.T) {
	f := newAdvanceFixture(t)
	// A fork checkpoint remote never advances, so the ordinary refusal stays.
	if err := advanceUntouchedBase(f.root, 7, f.branch, "", f.base, nil); err != nil {
		t.Fatalf("missing previous base must be a no-op: %v", err)
	}
	if _, _, err := ProvisionWithContext(f.root, "fork", 7, "task", f.base, f.branch, "", ProvisionContext{PreviousBase: f.previous}); err == nil {
		t.Fatal("fork provisioning advanced the worktree")
	}
	if head := f.git(t, f.path, "rev-parse", "HEAD"); head != f.previous {
		t.Fatalf("HEAD moved to %s", head)
	}
}

func TestAdvanceUntouchedBaseCASRace(t *testing.T) {
	f := newAdvanceFixture(t)
	advanceWrite(t, f.path, "AGENTS.md", "<!-- swarm:active:start -->\npointer\n<!-- swarm:active:end -->\n\ninstructions\n")
	advanceWrite(t, f.path, ".swarm/active/PLAN.md", "local plan\n")
	successor := f.git(t, f.root, "commit-tree", "-p", f.previous, "-m", "concurrent", f.previous+"^{tree}")
	var before string
	err := advanceUntouchedBase(f.root, 7, f.branch, f.previous, f.base, func() error {
		before = f.snapshot(t)
		f.git(t, f.root, "update-ref", "refs/heads/"+f.branch, successor, f.previous)
		before = strings.Replace(before, f.previous, successor, 1)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "branch moved") {
		t.Fatalf("CAS did not fail: %v", err)
	}
	if tip := f.git(t, f.root, "rev-parse", "refs/heads/"+f.branch); tip != successor {
		t.Fatalf("successor tip overwritten: %s", tip)
	}
	if after := f.snapshot(t); after != before {
		t.Fatalf("failed CAS changed the worktree:\nbefore %q\nafter  %q", before, after)
	}
}

func TestAdvanceUntouchedBaseRollsBackFailedCheckout(t *testing.T) {
	for _, name := range []string{"added.txt", "added.log"} {
		t.Run(name, func(t *testing.T) {
			f := newAdvanceFixture(t)
			err := advanceUntouchedBase(f.root, 7, f.branch, f.previous, f.base, func() error {
				// A file appearing after the checks obstructs the checkout.
				advanceWrite(t, f.path, name, "late user output\n")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "git worktree remove") {
				t.Fatalf("obstructed checkout not reported: %v", err)
			}
			if tip := f.git(t, f.root, "rev-parse", "refs/heads/"+f.branch); tip != f.previous {
				t.Fatalf("branch not restored: %s", tip)
			}
			if got, _ := os.ReadFile(filepath.Join(f.path, name)); string(got) != "late user output\n" {
				t.Fatalf("user file overwritten: %q", got)
			}
			if status := f.git(t, f.path, "status", "--porcelain"); status != "?? "+name && name == "added.txt" {
				t.Fatalf("worktree not left at the previous base: %q", status)
			}
		})
	}
}
