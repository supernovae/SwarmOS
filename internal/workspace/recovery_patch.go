package workspace

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
)

// FilterRecoveryPatch removes the sections for drop from a saved recovery
// patch and returns the remaining patch with the paths it changes. It accepts
// only the plain text sections SwarmOS writes: no renames, copies, binary
// data, or paths outside the worktree, so the returned paths are exactly the
// files git apply will touch.
func FilterRecoveryPatch(patch []byte, drop []string) ([]byte, []string, error) {
	dropped := map[string]bool{}
	for _, name := range drop {
		dropped[name] = true
	}
	var kept bytes.Buffer
	var paths []string
	seen := map[string]bool{}
	for _, section := range recoverySections(patch) {
		name, err := recoverySectionPath(section)
		if err != nil {
			return nil, nil, err
		}
		if seen[name] {
			return nil, nil, fmt.Errorf("recovery patch changes %s twice", name)
		}
		seen[name] = true
		if dropped[name] {
			continue
		}
		kept.Write(section)
		paths = append(paths, name)
	}
	return kept.Bytes(), paths, nil
}

func recoverySections(patch []byte) [][]byte {
	var sections [][]byte
	lines := bytes.SplitAfter(patch, []byte("\n"))
	start := 0
	offset := 0
	for i, line := range lines {
		if i > 0 && bytes.HasPrefix(line, []byte("diff --git ")) {
			sections = append(sections, patch[start:offset])
			start = offset
		}
		offset += len(line)
	}
	if start < len(patch) {
		sections = append(sections, patch[start:])
	}
	return sections
}

// recoverySectionPath validates one section's header and returns its path.
func recoverySectionPath(section []byte) (string, error) {
	lines := strings.Split(string(section), "\n")
	header, ok := strings.CutPrefix(lines[0], "diff --git ")
	if !ok {
		return "", fmt.Errorf("invalid recovery patch section")
	}
	oldName, rest, err := recoveryPatchName(header)
	if err != nil || !strings.HasPrefix(rest, " ") {
		return "", fmt.Errorf("invalid recovery patch header %q", lines[0])
	}
	newName, rest, err := recoveryPatchName(rest[1:])
	if err != nil || rest != "" || !strings.HasPrefix(oldName, "a/") || newName != "b/"+oldName[2:] {
		return "", fmt.Errorf("invalid recovery patch header %q", lines[0])
	}
	name := oldName[2:]
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == "." || strings.HasPrefix(name, "../") || name == ".." {
		return "", fmt.Errorf("recovery patch path %q is outside the worktree", name)
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return "", fmt.Errorf("recovery patch path %q is not allowed", name)
		}
	}
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "@@") {
			break
		}
		switch {
		case line == "":
		case strings.HasPrefix(line, "new file mode "), strings.HasPrefix(line, "deleted file mode "), strings.HasPrefix(line, "old mode "), strings.HasPrefix(line, "new mode "):
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
			target, rest, err := recoveryPatchName(line[4:])
			prefix := "a/"
			if line[0] == '+' {
				prefix = "b/"
			}
			if err != nil || rest != "" || (target != "/dev/null" && target != prefix+name) {
				return "", fmt.Errorf("invalid recovery patch line %q", line)
			}
		default:
			return "", fmt.Errorf("unsupported recovery patch line %q", line)
		}
	}
	return name, nil
}

// recoveryPatchName reads one possibly quoted name, matching the sandbox's
// patch writer, and returns it with the unread remainder.
func recoveryPatchName(text string) (string, string, error) {
	if strings.HasPrefix(text, `"`) {
		quoted, err := strconv.QuotedPrefix(text)
		if err != nil {
			return "", "", err
		}
		name, err := strconv.Unquote(quoted)
		return name, text[len(quoted):], err
	}
	name, rest, _ := strings.Cut(text, " ")
	if rest != "" {
		rest = " " + rest
	}
	return name, rest, nil
}

// ApplyRecoveryPatch applies a filtered recovery patch to the worktree only
// when it is still clean, not rebasing, and at the head the rejected run
// started from. git apply is all-or-nothing, so a failed apply leaves the
// worktree unchanged.
func ApplyRecoveryPatch(ctx context.Context, wtPath, head string, patch []byte) error {
	current, dirty, rebasing, err := StatusPRHead(ctx, wtPath)
	if err != nil {
		return err
	}
	switch {
	case rebasing:
		return fmt.Errorf("worktree is rebasing; refusing to apply the recovery patch")
	case current != head:
		return fmt.Errorf("worktree head moved from %s to %s since the rejected run started; refusing to apply the recovery patch", head, current)
	case dirty:
		return fmt.Errorf("worktree has uncommitted changes; refusing to apply the recovery patch")
	case len(patch) == 0:
		return fmt.Errorf("recovery patch has no changes left after dropping the rejected paths")
	}
	for _, args := range [][]string{{"apply", "--check", "-"}, {"apply", "-"}} {
		cmd, redact, err := gitcredential.Command(ctx, wtPath, args...)
		if err != nil {
			return err
		}
		cmd.Stdin = bytes.NewReader(patch)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("recovery patch does not apply: %w: %s", err, redact(strings.TrimSpace(string(out))))
		}
	}
	return nil
}
