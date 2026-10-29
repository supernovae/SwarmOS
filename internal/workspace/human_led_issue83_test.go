package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

func TestIssue83IssueAndPRPointerSequence(t *testing.T) {
	root := t.TempDir()
	runWorkspaceGit(t, root, "init", "-b", "main")
	runWorkspaceGit(t, root, "config", "user.name", "Fixture")
	runWorkspaceGit(t, root, "config", "user.email", "fixture@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	outside := "Human instructions 🙂\n"
	for _, file := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := os.WriteFile(filepath.Join(root, file), []byte(outside), 0600); err != nil {
			t.Fatal(err)
		}
	}
	runWorkspaceGit(t, root, "add", ".")
	runWorkspaceGit(t, root, "commit", "-m", "base")
	base := workspaceGitOutput(t, root, "rev-parse", "HEAD")
	wf, err := harness.LoadWorkflow(root, "feature")
	if err != nil {
		t.Fatal(err)
	}
	wu := &contract.WorkUnit{ID: "ISSUE-83", IssueNumber: 83, Title: "Human-led", Objective: "Test pointers"}
	for _, pr := range []bool{false, true} {
		if pr {
			err = CompilePRHarnessBundleAt(root, wu, wf, nil, "developer", base, 321)
		} else {
			err = CompileHarnessBundleAt(root, wu, wf, nil, "developer", base)
		}
		if err != nil {
			t.Fatal(err)
		}
		commands := []string{"/swarm-plan", "swarm issue stage 83 --phase plan", "swarm issue review-plan 83", "/swarm-implement", "swarm issue validate 83", "swarm issue stage 83 --phase implement", "swarm issue renew 83", "swarm issue submit 83"}
		if pr {
			commands = []string{"/swarm-plan", "swarm pr stage 321 --phase plan", "swarm pr work 321 --phase review", "/swarm-implement", "swarm pr validate 321", "swarm pr stage 321 --phase implement", "swarm issue renew 83", "swarm pr submit 321"}
		}
		for _, file := range []string{"AGENTS.md", "CLAUDE.md"} {
			raw, err := os.ReadFile(filepath.Join(root, file))
			if err != nil {
				t.Fatal(err)
			}
			text := string(raw)
			if !strings.Contains(text, outside) {
				t.Fatal("outside instructions changed", text)
			}
			sequence := text[strings.Index(text, "/swarm-plan"):]
			for _, command := range commands {
				idx := strings.Index(sequence, command)
				if idx < 0 {
					t.Fatal("missing ordered command", command, text)
				}
				sequence = sequence[idx+len(command):]
			}
			if strings.Contains(text, "swarm pr renew") || strings.Contains(text, "swarm pr review-plan") {
				t.Fatal(text)
			}
		}
	}
}
