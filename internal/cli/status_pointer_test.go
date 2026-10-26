package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/workspace"
)

// TestStatusPointerOnlyWorktreeIsClean covers issue and PR status after a
// harness leaves SwarmOS's pointer block in the task worktrees.
func TestStatusPointerOnlyWorktreeIsClean(t *testing.T) {
	block := workspace.ActiveMarkerStart + "\nRead .swarm/active/WORKUNIT.md.\n" + workspace.ActiveMarkerEnd + "\n\n"
	for _, layout := range []struct {
		name    string
		claude  string   // committed CLAUDE.md: "" for none, "->AGENTS.md" for a link
		targets []string // files that receive the pointer
	}{
		{"generated claude", "", []string{"AGENTS.md", "CLAUDE.md"}},
		{"separate files", "# Claude notes\n", []string{"AGENTS.md", "CLAUDE.md"}},
		{"claude links to agents", "->AGENTS.md", []string{"AGENTS.md"}},
	} {
		t.Run(layout.name, func(t *testing.T) {
			f := newCLIFixture(t)
			if layout.claude != "" {
				path := filepath.Join(f.root, "CLAUDE.md")
				if target, ok := strings.CutPrefix(layout.claude, "->"); ok {
					if err := os.Symlink(target, path); err != nil {
						t.Skipf("symlink creation unavailable: %v", err)
					}
				} else if err := os.WriteFile(path, []byte(layout.claude), 0o644); err != nil {
					t.Fatal(err)
				}
				prGit(t, f.root, "add", "CLAUDE.md")
				prGit(t, f.root, "commit", "-m", "instructions")
			}
			f.create(t, "agent") // PR and issue IDs deliberately differ.
			n := f.create(t, "agent")
			issue := strconv.Itoa(n)
			f.must(t, "issue", "approve", issue)
			f.must(t, "issue", "work", issue, "--agent", "mock", "--progress=false")
			f.must(t, "issue", "submit", issue)
			p := strconv.Itoa(f.state(t, n).PRNumber)
			t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR update\\n' > pr-update.txt")
			worked := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--instructions", "Fix the implementation within the approved acceptance criteria", "--progress=false", "--json"))
			trees := []string{workspace.WorktreePath(f.root, n), worked.Worktree}

			assertDirty := func(want bool) {
				t.Helper()
				raw := f.must(t, "issue", "status", issue, "--json")
				var status struct {
					Issues []struct {
						Checkpoint *workspace.CheckpointState `json:"checkpoint"`
					} `json:"issues"`
				}
				if err := json.Unmarshal([]byte(raw), &status); err != nil || len(status.Issues) != 1 || status.Issues[0].Checkpoint == nil || status.Issues[0].Checkpoint.Dirty != want {
					t.Fatalf("issue status JSON dirty, want %v: %v\n%s", want, err, raw)
				}
				if text := f.must(t, "issue", "status", issue); !strings.Contains(text, "dirty="+strconv.FormatBool(want)+" ") {
					t.Fatalf("issue status text, want dirty=%v:\n%s", want, text)
				}
				if got := readPROutcome(t, f.must(t, "pr", "status", p, "--json")); got.Dirty != want {
					t.Fatalf("pr status JSON dirty=%v, want %v", got.Dirty, want)
				}
				if text := f.must(t, "pr", "status", p); !strings.Contains(text, "Local changes: "+strconv.FormatBool(want)+"\n") {
					t.Fatalf("pr status text, want local changes %v:\n%s", want, text)
				}
			}

			for _, wt := range trees {
				for _, target := range layout.targets {
					path := filepath.Join(wt, target)
					existing, err := os.ReadFile(path)
					if err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, append([]byte(block), existing...), 0o644); err != nil {
						t.Fatal(err)
					}
					if only, err := workspace.OnlyActivePointer(wt, target); err != nil || !only {
						t.Fatalf("%s is not pointer-only: %v %v", target, only, err)
					}
				}
			}
			assertDirty(false)

			// A real edit to the instruction file the pointer lives in stays dirty.
			for _, wt := range trees {
				path := filepath.Join(wt, "AGENTS.md")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(data, "Team edit.\n"...), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			assertDirty(true)
		})
	}
}

func TestStatusPointerOtherFileIsDirty(t *testing.T) {
	f, n, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR update\\n' > pr-update.txt")
	worked := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--instructions", "Fix the implementation within the approved acceptance criteria", "--progress=false", "--json"))
	for _, wt := range []string{workspace.WorktreePath(f.root, n), worked.Worktree} {
		if err := os.WriteFile(filepath.Join(wt, "notes.txt"), []byte("unsaved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if text := f.must(t, "issue", "status", strconv.Itoa(n)); !strings.Contains(text, "dirty=true ") {
		t.Fatalf("issue status hides an untracked file:\n%s", text)
	}
	if got := readPROutcome(t, f.must(t, "pr", "status", p, "--json")); !got.Dirty {
		t.Fatal("pr status hides an untracked file")
	}
}
