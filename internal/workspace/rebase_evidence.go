package workspace

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
)

// RebaseConflicts returns literal unmerged paths without Git's quoting rules.
func RebaseConflicts(ctx context.Context, root string) ([]string, error) {
	out, err := gitOutputContext(ctx, root, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// RebasePatchIdentity compares aggregate changes using Git's stable patch ID.
// Empty changes compare equal to each other; failures never imply equality.
func RebasePatchIdentity(ctx context.Context, root, oldBase, oldHead, newBase, newHead string) (string, error) {
	patchID := func(base, head string) (string, error) {
		for _, sha := range []string{base, head} {
			if err := prCommit(ctx, root, sha); err != nil {
				return "", err
			}
		}
		diffCmd, _, err := gitcredential.Command(ctx, root, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--binary", base, head, "--")
		if err != nil {
			return "", err
		}
		raw, err := diffCmd.Output()
		if err != nil {
			return "", fmt.Errorf("read patch: %w", err)
		}
		diff := string(raw)
		if diff == "" {
			return "empty", nil
		}
		cmd, redact, err := gitcredential.Command(ctx, root, "patch-id", "--stable")
		if err != nil {
			return "", err
		}
		cmd.Stdin = strings.NewReader(diff)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("patch identity: %w: %s", err, redact(normalizeGitOutput(string(out))))
		}
		fields := strings.Fields(string(out))
		if len(fields) != 2 {
			return "", fmt.Errorf("patch identity unavailable")
		}
		return fields[0], nil
	}
	a, err := patchID(oldBase, oldHead)
	if err != nil {
		return "unavailable", err
	}
	b, err := patchID(newBase, newHead)
	if err != nil {
		return "unavailable", err
	}
	if a == b {
		return "unchanged", nil
	}
	return "changed", nil
}

// RebaseDroppedPaths returns the sorted paths the PR changed against oldBase
// that it no longer changes against newBase. Disabling rename detection counts
// both endpoints; failures never imply that nothing was dropped.
func RebaseDroppedPaths(ctx context.Context, root, oldBase, oldHead, newBase, newHead string) ([]string, error) {
	changed := func(base, head string) ([]string, error) {
		for _, sha := range []string{base, head} {
			if err := prCommit(ctx, root, sha); err != nil {
				return nil, err
			}
		}
		out, err := gitOutputContext(ctx, root, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", base, head, "--")
		if err != nil {
			return nil, err
		}
		return nulPaths(out), nil
	}
	before, err := changed(oldBase, oldHead)
	if err != nil {
		return nil, err
	}
	after, err := changed(newBase, newHead)
	if err != nil {
		return nil, err
	}
	kept := make(map[string]bool, len(after))
	for _, p := range after {
		kept[p] = true
	}
	dropped := []string{}
	for _, p := range before {
		if !kept[p] {
			dropped = append(dropped, p)
		}
	}
	slices.Sort(dropped)
	return slices.Compact(dropped), nil
}

func normalizeGitOutput(out string) string {
	return strings.ReplaceAll(strings.ReplaceAll(out, "\r\n", "\n"), "\r", "\n")
}
