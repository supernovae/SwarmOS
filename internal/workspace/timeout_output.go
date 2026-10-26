package workspace

import (
	"context"
	"fmt"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

// RejectRecoveryOutput keeps coordinator recovery files out of published trees,
// including files manually committed or staged by an operator.
func RejectRecoveryOutput(ctx context.Context, root, head string) error {
	tree, err := gitOutputContext(ctx, root, "ls-tree", "-r", "--name-only", head, "--", ".swarm/active-output")
	if err != nil {
		return err
	}
	staged, err := gitOutputContext(ctx, root, "ls-files", "--cached", "--", ".swarm/active-output")
	if err != nil {
		return err
	}
	if strings.TrimSpace(tree) != "" || strings.TrimSpace(staged) != "" {
		return fmt.Errorf("recovery output must not be tracked or staged: .swarm/active-output")
	}
	return nil
}

// Enumerate literal file paths so candidate ignore rules cannot re-include
// recovery files, and exclusion pathspecs cannot turn ignored dirs into errors.
func stageCheckpointFiles(ctx context.Context, root string) error {
	return stageCheckpointFilesWith(func(args ...string) (string, error) { return gitOutputContext(ctx, root, args...) })
}

func stageCheckpointFilesWith(run func(...string) (string, error)) error {
	paths, err := run("ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	args := []string{"add", "-A", "--"}
	flush := func() error {
		if len(args) == 3 {
			return nil
		}
		_, err := run(args...)
		args = []string{"add", "-A", "--"}
		return err
	}
	for _, name := range strings.Split(paths, "\x00") {
		if name == "" || name == ".swarm/active-output" || strings.HasPrefix(name, ".swarm/active-output/") {
			continue
		}
		args = append(args, ":(literal)"+name)
		if len(args) >= 103 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// onlyActivePointer reports whether an unstaged change to an agent instruction
// file is exactly SwarmOS's temporary task block, which is injected for agents
// and removed before every checkpoint.
func OnlyActivePointer(worktreePath, path string) (bool, error) {
	if path != "AGENTS.md" && path != "CLAUDE.md" {
		return false, nil
	}
	current, err := safefs.ReadFile(worktreePath, path, 16<<20)
	if err != nil {
		return false, nil
	}
	committed := ""
	if listing, err := gitOutputContext(context.Background(), worktreePath, "ls-tree", "--name-only", "HEAD", "--", path); err != nil {
		return false, err
	} else if len(strings.TrimSpace(listing)) != 0 {
		out, err := gitOutputContext(context.Background(), worktreePath, "show", "HEAD:"+path)
		if err != nil {
			return false, err
		}
		committed = string(out)
	}
	stripped := StripActivePointer(string(current))
	if stripped == string(current) {
		return false, nil
	}
	return strings.ReplaceAll(stripped, "\r\n", "\n") == strings.ReplaceAll(committed, "\r\n", "\n"), nil
}

// HasCheckpointableChanges observes the same file set as Checkpoint without writes.
func HasCheckpointableChanges(ctx context.Context, root string) (bool, error) {
	out, err := gitOutputContext(ctx, root, "--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		name := e[3:]
		if e[0] == 'R' || e[0] == 'C' || e[1] == 'R' || e[1] == 'C' {
			return true, nil
		}
		if name == ".swarm/active-output" || strings.HasPrefix(name, ".swarm/active-output/") {
			continue
		}
		only, err := OnlyActivePointer(root, name)
		if err != nil {
			return false, err
		}
		if !only {
			return true, nil
		}
	}
	return false, nil
}
