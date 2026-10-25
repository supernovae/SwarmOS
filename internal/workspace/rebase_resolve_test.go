package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasConflictMarkers(t *testing.T) {
	for text, want := range map[string]bool{
		"<<<<<<< HEAD\na\n=======\nb\n>>>>>>> pr\n": true,
		"a\n>>>>>>> pr\n":               true,
		"a\n||||||| base\n":             true,
		"<<<<<<<\n":                     true,
		"x\r\n<<<<<<< HEAD\r\n":         true,
		"Title\n=======\n":              false,
		"a <<<<<<< not at line start\n": false,
		"<<<<<<<<< nine\n":              false,
		"plain\n":                       false,
	} {
		if got := HasConflictMarkers([]byte(text)); got != want {
			t.Errorf("HasConflictMarkers(%q) = %v, want %v", text, got, want)
		}
	}
}

// pausedRebase builds a repository whose branch conflicts with main and leaves
// `git rebase main` stopped. setup writes the shared ancestor, onMain and
// onBranch apply each side's commit.
func pausedRebase(t *testing.T, setup, onMain, onBranch func(root string)) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
		if out, err := cmd.CombinedOutput(); err != nil && args[0] != "rebase" {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	setup(root)
	git("add", "-A")
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("checkout", "-q", "-b", "pr")
	onBranch(root)
	git("add", "-A")
	git("commit", "-q", "-m", "pr")
	git("checkout", "-q", "main")
	onMain(root)
	git("add", "-A")
	git("commit", "-q", "-m", "main")
	git("checkout", "-q", "pr")
	git("rebase", "main")
	return root
}

func write(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConflictInputsContentAndAddAdd(t *testing.T) {
	root := pausedRebase(t,
		func(r string) { write(t, r, "shared.txt", "base\n") },
		func(r string) { write(t, r, "shared.txt", "main\n"); write(t, r, "new.txt", "main new\n") },
		func(r string) { write(t, r, "shared.txt", "pr\n"); write(t, r, "new.txt", "pr new\n") })
	inputs, err := ConflictInputs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0].Path != "new.txt" || inputs[1].Path != "shared.txt" {
		t.Fatalf("inputs = %+v", inputs)
	}
	addAdd, content := inputs[0], inputs[1]
	if addAdd.Base != nil || string(addAdd.Ours.Data) != "main new\n" || string(addAdd.Theirs.Data) != "pr new\n" {
		t.Fatalf("add/add sides wrong: %+v", addAdd)
	}
	if content.Base == nil || string(content.Base.Data) != "base\n" || string(content.Ours.Data) != "main\n" || string(content.Theirs.Data) != "pr\n" {
		t.Fatalf("content sides wrong: %+v", content)
	}
	if !HasConflictMarkers(content.Worktree) {
		t.Fatal("worktree bytes should carry Git's markers")
	}
	stopped, err := StoppedCommit(context.Background(), root)
	if err != nil || len(stopped) != 40 {
		t.Fatalf("stopped commit = %q, %v", stopped, err)
	}
}

func TestConflictInputsRejectsIneligibleConflicts(t *testing.T) {
	cases := map[string]func() string{
		"symlink stage": func() string {
			return pausedRebase(t,
				func(r string) { write(t, r, "target", "x\n") },
				func(r string) { _ = os.Symlink("target", filepath.Join(r, "link")) },
				func(r string) { write(t, r, "link", "regular on the PR\n") })
		},
		"control path": func() string {
			return pausedRebase(t,
				func(r string) { write(t, r, ".swarm/active/x.md", "base\n") },
				func(r string) { write(t, r, ".swarm/active/x.md", "main\n") },
				func(r string) { write(t, r, ".swarm/active/x.md", "pr\n") })
		},
		"binary": func() string {
			return pausedRebase(t,
				func(r string) { write(t, r, "blob.bin", "\x00base\x00") },
				func(r string) { write(t, r, "blob.bin", "\x00main\x00") },
				func(r string) { write(t, r, "blob.bin", "\x00pr\x00") })
		},
		"deleted on base": func() string {
			return pausedRebase(t,
				func(r string) { write(t, r, "gone.txt", "base\n") },
				func(r string) { _ = os.Remove(filepath.Join(r, "gone.txt")) },
				func(r string) { write(t, r, "gone.txt", "pr\n") })
		},
	}
	for name, build := range cases {
		root := build()
		if _, err := ConflictInputs(context.Background(), root); !errors.Is(err, ErrConflictIneligible) {
			t.Errorf("%s: err = %v, want ErrConflictIneligible", name, err)
		}
	}

	// A conflicted path replaced by a symlink in the worktree is never followed.
	root := pausedRebase(t,
		func(r string) { write(t, r, "f.txt", "base\n") },
		func(r string) { write(t, r, "f.txt", "main\n") },
		func(r string) { write(t, r, "f.txt", "pr\n") })
	if err := os.Remove(filepath.Join(root, "f.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "f.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := ConflictInputs(context.Background(), root); !errors.Is(err, ErrConflictIneligible) || !strings.Contains(err.Error(), "f.txt") {
		t.Fatalf("symlink worktree entry: err = %v", err)
	}
}
