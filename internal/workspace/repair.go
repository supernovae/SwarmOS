package workspace

import (
	"context"
	"strings"
)

// RepairPaths fixes the repair scope from the committed PR diff. Disabling
// rename detection includes both endpoints without parsing quoted filenames.
func RepairPaths(ctx context.Context, root, base string) ([]string, error) {
	if err := prCommit(ctx, root, base); err != nil {
		return nil, err
	}
	out, err := gitOutputContext(ctx, root, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", base, "HEAD", "--")
	if err != nil {
		return nil, err
	}
	return nulPaths(out), nil
}

func TrackedRepairFiles(ctx context.Context, root string) ([]string, error) {
	out, err := gitOutputContext(ctx, root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	return nulPaths(out), nil
}
func nulPaths(out string) []string {
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}
