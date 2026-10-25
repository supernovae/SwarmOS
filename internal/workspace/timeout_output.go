package workspace

import (
	"context"
	"fmt"
	"strings"
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
	paths, err := gitOutputContext(ctx, root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	args := []string{"add", "-A", "--"}
	flush := func() error {
		if len(args) == 3 {
			return nil
		}
		_, err := gitOutputContext(ctx, root, args...)
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
