package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestValidationTreeCheckpointParity(t *testing.T) {
	for _, kind := range []string{"clean", "unstaged", "staged", "partial", "cancelled", "new", "deleted", "renamed", "executable", "ignored", "tracked-ignored", "literal", "pointer", "pointer-only", "tracked-pointer-only", "filter", "split-index", "deleted-instructions", "tracked-empty", "user-pointer", "missing-index", "staged-new-ignored", "intent-new-ignored", "racy", "racy-split", "racy-untracked", "racy-split-untracked"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				out, e := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
				if e != nil {
					t.Fatalf("git %v: %v %s", args, e, out)
				}
				return string(out)
			}
			write := func(name, body string) {
				t.Helper()
				if e := os.WriteFile(filepath.Join(root, name), []byte(body), 0644); e != nil {
					t.Fatal(e)
				}
			}
			git("init")
			git("config", "user.name", "Test")
			git("config", "user.email", "test@example.invalid")
			write("file", "initial\n")
			write("AGENTS.md", "instructions\n")
			git("add", ".")
			git("commit", "-m", "base")
			candidate := filepath.Join(t.TempDir(), "candidate")
			git("worktree", "add", "-b", "test-checkpoint", candidate, "HEAD")
			root = candidate
			indexPath := strings.TrimSpace(git("rev-parse", "--git-path", "index"))
			pointer := "<!-- swarm:active:start -->\nactive\n<!-- swarm:active:end -->\n\n"
			switch kind {
			case "staged-new-ignored", "intent-new-ignored":
				write("new", "new\n")
				if kind == "intent-new-ignored" {
					git("add", "-N", "new")
				} else {
					git("add", "new")
				}
				write(".gitignore", "new\n")
			case "unstaged":
				write("file", "changed\n")
			case "staged":
				write("file", "changed\n")
				git("add", "file")
			case "partial":
				write("file", "staged\n")
				git("add", "file")
				write("file", "unstaged\n")
			case "cancelled":
				write("file", "staged\n")
				git("add", "file")
				write("file", "initial\n")
			case "new":
				write("new", "new\n")
			case "deleted":
				if e := os.Remove(filepath.Join(root, "file")); e != nil {
					t.Fatal(e)
				}
			case "renamed":
				git("mv", "file", "renamed")
			case "executable":
				if e := os.Chmod(filepath.Join(root, "file"), 0755); e != nil {
					t.Fatal(e)
				}
			case "ignored":
				write(".gitignore", "ignored\n")
				write("ignored", "ignored\n")
			case "tracked-ignored":
				write(".gitignore", "file\n")
				write("file", "changed\n")
			case "literal":
				write("[a]*\nname", "literal\n")
			case "pointer":
				write("AGENTS.md", pointer+"instructions\n")
			case "pointer-only":
				write("CLAUDE.md", pointer)
			case "tracked-pointer-only":
				write("AGENTS.md", pointer)
			case "missing-index":
				write(".gitignore", "file\n")
				if e := os.Remove(indexPath); e != nil {
					t.Fatal(e)
				}
			case "split-index":
				git("update-index", "--split-index")
				write("file", "changed\n")
			case "deleted-instructions":
				if e := os.Remove(filepath.Join(root, "AGENTS.md")); e != nil {
					t.Fatal(e)
				}
			case "tracked-empty":
				write("AGENTS.md", "")
			case "user-pointer":
				write("CLAUDE.md", pointer+"user instructions\n")
			case "filter":
				write(".gitattributes", "file filter=upper\n")
				git("config", "filter.upper.clean", "tr a-z A-Z")
				write("file", "changed\n")
			}
			// Establish an old, equal-size/equal-mtime edit without a refreshing
			// Git command afterwards. A copied index incorrectly makes it clean.
			racy := strings.HasPrefix(kind, "racy")
			if racy {
				git("config", "core.checkStat", "minimal")
				git("config", "core.trustctime", "false")
				if strings.Contains(kind, "untracked") {
					git("config", "core.untrackedCache", "true")
				}
				stamp := time.Unix(1700000000, 0)
				file := filepath.Join(root, "file")
				if e := os.Chtimes(file, stamp, stamp); e != nil {
					t.Fatal(e)
				}
				git("add", "file")
				if strings.Contains(kind, "split") {
					git("update-index", "--split-index")
					if strings.TrimSpace(git("rev-parse", "--shared-index-path")) == "" {
						t.Fatal("split index not active")
					}
				}
				write("file", "changed\n")
				paths, e := filepath.Glob(filepath.Join(filepath.Dir(indexPath), "sharedindex.*"))
				if e != nil {
					t.Fatal(e)
				}
				paths = append(paths, indexPath, file)
				for _, path := range paths {
					if e := os.Chtimes(path, stamp, stamp); e != nil {
						t.Fatal(e)
					}
					info, e := os.Stat(path)
					if e != nil || !info.ModTime().Equal(stamp) {
						t.Fatalf("timestamp %s: %v", path, e)
					}
				}
				info, e := os.Stat(file)
				if e != nil || info.Size() != int64(len("initial\n")) {
					t.Fatal("size precondition failed")
				}
			}
			beforeConfig := git("config", "--local", "--list")
			shared := map[string]string{}
			paths, e := filepath.Glob(filepath.Join(filepath.Dir(indexPath), "sharedindex.*"))
			if e != nil {
				t.Fatal(e)
			}
			for _, path := range paths {
				data, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				shared[path] = string(data)
			}
			beforeIndex, e := os.ReadFile(indexPath)
			if e != nil && !os.IsNotExist(e) {
				t.Fatal(e)
			}
			beforeStatus := ""
			if !racy {
				beforeStatus = git("--no-optional-locks", "status", "--porcelain=v1", "-z")
			}
			beforeHead := git("rev-parse", "HEAD")
			beforeFiles := map[string]string{}
			beforeModes := map[string]os.FileMode{}
			_ = filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if d.IsDir() && d.Name() == ".git" {
					return filepath.SkipDir
				}
				if d.Type().IsRegular() {
					data, e := os.ReadFile(path)
					if e != nil {
						return e
					}
					beforeFiles[path] = string(data)
					info, e := d.Info()
					if e != nil {
						return e
					}
					beforeModes[path] = info.Mode()
				}
				return nil
			})
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			tree, e := CheckpointTree(context.Background(), root)
			if e != nil {
				t.Fatal(e)
			}
			entries, e := os.ReadDir(scratch)
			if e != nil || len(entries) != 0 {
				t.Fatal("temporary index leaked")
			}
			afterIndex, e := os.ReadFile(indexPath)
			if e != nil && !os.IsNotExist(e) {
				t.Fatal(e)
			}
			if string(beforeIndex) != string(afterIndex) || (!racy && beforeStatus != git("--no-optional-locks", "status", "--porcelain=v1", "-z")) || beforeHead != git("rev-parse", "HEAD") {
				t.Fatal("fingerprint changed index, status or HEAD")
			}
			if beforeConfig != git("config", "--local", "--list") {
				t.Fatal("changed config")
			}
			afterShared, e := filepath.Glob(filepath.Join(filepath.Dir(indexPath), "sharedindex.*"))
			if e != nil || len(afterShared) != len(shared) {
				t.Fatal("changed shared indexes")
			}
			for _, path := range afterShared {
				data, e := os.ReadFile(path)
				if e != nil || string(data) != shared[path] {
					t.Fatal("changed shared index")
				}
			}
			for path, want := range beforeFiles {
				got, e := os.ReadFile(path)
				info, statErr := os.Stat(path)
				if e != nil || string(got) != want || statErr != nil || info.Mode() != beforeModes[path] {
					t.Fatalf("changed %s", path)
				}
			}
			if racy && git("show", tree+":file") != "changed\n" {
				t.Fatal("fingerprint reused stale cached blob")
			}
			head, e := Checkpoint(context.Background(), root, "test-checkpoint", CheckpointOptions{WorkUnit: &contract.WorkUnit{IssueNumber: 1, Title: "Test"}, Purpose: "submit"})
			if e != nil {
				t.Fatal(e)
			}
			want, e := gitOutputContext(context.Background(), root, "rev-parse", head+"^{tree}")
			if e != nil {
				t.Fatal(e)
			}
			if tree+"\n" != want {
				t.Fatalf("tree %s != checkpoint %s: %s", tree, want, git("diff", tree, strings.TrimSpace(want)))
			}
		})
	}
}

func TestValidationTreeRejectsInstructionSymlink(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"init"}, {"-c", "user.name=Test", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", "base"}} {
		if out, e := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); e != nil {
			t.Fatalf("%v %s", e, out)
		}
	}
	if e := os.Symlink("missing", filepath.Join(root, "AGENTS.md")); e != nil {
		t.Fatal(e)
	}
	if _, e := CheckpointTree(context.Background(), root); e == nil {
		t.Fatal("accepted instruction symlink")
	}
}

func TestValidationTreeRecoveryOutputAndFailurePreserveIndex(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, e := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("git: %v %s", e, out)
		}
		return string(out)
	}
	git("init")
	git("-c", "user.name=Test", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", "base")
	if e := os.MkdirAll(filepath.Join(root, ".swarm/active-output"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, ".swarm/active-output/recovery"), []byte("local"), 0644); e != nil {
		t.Fatal(e)
	}
	want := strings.TrimSpace(git("rev-parse", "HEAD^{tree}"))
	tree, e := CheckpointTree(context.Background(), root)
	if e != nil || tree != want {
		t.Fatalf("untracked recovery included: %s %v", tree, e)
	}
	git("add", ".swarm/active-output/recovery")
	before, e := os.ReadFile(filepath.Join(root, ".git/index"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e := CheckpointTree(context.Background(), root); e == nil {
		t.Fatal("accepted staged recovery")
	}
	after, e := os.ReadFile(filepath.Join(root, ".git/index"))
	if e != nil || string(before) != string(after) {
		t.Fatal("failure changed index")
	}
}
