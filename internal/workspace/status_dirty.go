package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// hasStatusChanges reports whether a worktree has changes a person would need
// to save, without writing the index. It differs from raw porcelain only for
// AGENTS.md, CLAUDE.md, or the file they resolve to, when the staged and
// working content both equal HEAD once SwarmOS's pointer block is stripped and
// the entry is still the same kind of regular file.
func hasStatusChanges(ctx context.Context, root string) (bool, error) {
	out, err := gitOutputContext(ctx, root, "--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	var paths map[string]bool
	for _, e := range strings.Split(out, "\x00") {
		if e == "" {
			continue
		}
		if len(e) < 4 {
			return true, nil
		}
		x, y, name := e[0], e[1], e[3:]
		if paths == nil {
			paths = instructionPaths(root)
		}
		if !paths[name] {
			return true, nil
		}
		only, err := pointerOnlyEntry(ctx, root, x, y, name)
		if err != nil {
			return false, err
		}
		if !only {
			return true, nil
		}
	}
	return false, nil
}

// pointerOnlyEntry checks one instruction status entry. Renames, copies,
// deletions, type changes, and conflicts never qualify.
func pointerOnlyEntry(ctx context.Context, root string, x, y byte, name string) (bool, error) {
	untracked := x == '?' && y == '?'
	if !untracked && (!strings.ContainsRune(" MA", rune(x)) || !strings.ContainsRune(" M", rune(y))) {
		return false, nil
	}
	if info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	if only, err := OnlyActivePointer(root, name); err != nil || !only {
		return false, err
	}
	if untracked {
		return true, nil
	}
	added := x == 'A'
	if ok, err := sameRegularMode(ctx, root, name, added, "--cached"); err != nil || !ok {
		return false, err
	}
	if ok, err := sameRegularMode(ctx, root, name, added); err != nil || !ok {
		return false, err
	}
	if x == ' ' {
		return true, nil
	}
	// The working file may hide a staged edit, so check the index separately.
	committed, err := committedInstruction(ctx, root, name)
	if err != nil {
		return false, err
	}
	staged, err := gitOutputContext(ctx, root, "show", ":"+name)
	if err != nil {
		return false, err
	}
	return sameInstructionText(StripActivePointer(staged), committed), nil
}

// sameRegularMode compares HEAD with the index (--cached) or the working file
// and reports whether name keeps its regular-file mode. An added entry has no
// HEAD mode and only needs to be a regular file.
func sameRegularMode(ctx context.Context, root, name string, added bool, extra ...string) (bool, error) {
	args := append([]string{"--no-optional-locks", "diff-index", "--raw", "-z"}, extra...)
	out, err := gitOutputContext(ctx, root, append(args, "HEAD", "--", ":(literal)"+name)...)
	if err != nil {
		return false, err
	}
	if out == "" {
		return !added, nil
	}
	fields := strings.Fields(strings.TrimPrefix(strings.SplitN(out, "\x00", 2)[0], ":"))
	if len(fields) < 2 || (fields[1] != "100644" && fields[1] != "100755") {
		return false, nil
	}
	if added {
		return fields[0] == "000000", nil
	}
	return fields[0] == fields[1], nil
}
