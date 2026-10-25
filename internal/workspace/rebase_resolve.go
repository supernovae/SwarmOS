package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
)

const (
	maxConflictFileBytes  = 4 << 20
	maxConflictTotalBytes = 16 << 20
)

// ErrConflictIneligible marks a conflict an agent may not resolve; the rebase
// stays paused for manual resolution.
var ErrConflictIneligible = errors.New("conflict needs manual resolution")

// ConflictStage is one side of a conflicted path from the Git index.
type ConflictStage struct {
	Mode string
	Data []byte
}

// ConflictInput is a conflicted regular file with its index stages. Base is
// nil for add/add conflicts, where both sides created the file.
type ConflictInput struct {
	Path     string
	Base     *ConflictStage
	Ours     *ConflictStage
	Theirs   *ConflictStage
	Worktree []byte
	Mode     os.FileMode
}

// ConflictInputs reads every unmerged path of a paused rebase. It rejects
// conflicts that are not ordinary text edits of regular files, so callers can
// leave them paused rather than hand them to an agent.
func ConflictInputs(ctx context.Context, root string) ([]ConflictInput, error) {
	out, err := gitOutputContext(ctx, root, "ls-files", "-u", "-z")
	if err != nil {
		return nil, err
	}
	type entry struct{ mode, object string }
	stages := map[string]map[string]entry{}
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("unexpected unmerged index entry")
		}
		if stages[name] == nil {
			stages[name] = map[string]entry{}
		}
		stages[name][fields[2]] = entry{mode: fields[0], object: fields[1]}
	}
	names := make([]string, 0, len(stages))
	for name := range stages {
		names = append(names, name)
	}
	slices.Sort(names)
	var inputs []ConflictInput
	total := 0
	for _, name := range names {
		if name == ".swarm/active" || strings.HasPrefix(name, ".swarm/active/") {
			return nil, fmt.Errorf("%w: %s is a SwarmOS control path", ErrConflictIneligible, name)
		}
		s := stages[name]
		if _, ok := s["2"]; !ok {
			return nil, fmt.Errorf("%w: %s was deleted on the new base", ErrConflictIneligible, name)
		}
		if _, ok := s["3"]; !ok {
			return nil, fmt.Errorf("%w: %s was deleted by the PR commit", ErrConflictIneligible, name)
		}
		input := ConflictInput{Path: name}
		for stage, e := range s {
			if e.mode != "100644" && e.mode != "100755" {
				return nil, fmt.Errorf("%w: %s is not a regular file in every version", ErrConflictIneligible, name)
			}
			data, err := readBlob(ctx, root, e.object)
			if err != nil {
				return nil, err
			}
			total += len(data)
			side := &ConflictStage{Mode: e.mode, Data: data}
			switch stage {
			case "1":
				input.Base = side
			case "2":
				input.Ours = side
			case "3":
				input.Theirs = side
			}
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: %s is not a regular file in the worktree", ErrConflictIneligible, name)
		}
		if info.Size() > maxConflictFileBytes {
			return nil, fmt.Errorf("%w: %s is too large", ErrConflictIneligible, name)
		}
		input.Mode = info.Mode().Perm()
		if input.Worktree, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			return nil, err
		}
		// Without markers Git produced no textual merge (for example a binary
		// file); accepting the worktree bytes would silently drop one side.
		if !HasConflictMarkers(input.Worktree) {
			return nil, fmt.Errorf("%w: %s has no text conflict markers (binary or unsupported conflict)", ErrConflictIneligible, name)
		}
		total += len(input.Worktree)
		if total > maxConflictTotalBytes {
			return nil, fmt.Errorf("%w: conflicted files are too large", ErrConflictIneligible)
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

// readBlob returns exact blob bytes; file content is never redacted or trimmed.
func readBlob(ctx context.Context, root, object string) ([]byte, error) {
	if len(object) < 40 || strings.Trim(object, "0123456789abcdef") != "" {
		return nil, fmt.Errorf("invalid index object")
	}
	cmd, _, err := gitcredential.Command(ctx, root, "cat-file", "blob", object)
	if err != nil {
		return nil, err
	}
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read index object: %w", err)
	}
	if len(data) > maxConflictFileBytes {
		return nil, fmt.Errorf("%w: conflict side is too large", ErrConflictIneligible)
	}
	return data, nil
}

// StoppedCommit names the PR commit a paused rebase is replaying.
func StoppedCommit(ctx context.Context, root string) (string, error) {
	out, err := gitOutputContext(ctx, root, "rev-parse", "--verify", "REBASE_HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("read stopped rebase commit: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// StageResolved stages exactly the named paths, matched literally.
func StageResolved(ctx context.Context, root string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	args := []string{"add", "--"}
	for _, p := range paths {
		args = append(args, ":(literal)"+p)
	}
	_, err := gitOutputContext(ctx, root, args...)
	return err
}

// HasConflictMarkers reports a line that starts a conflict hunk, its diff3
// base section, or its end. A lone "=======" is ordinary content (for
// example a Markdown heading underline), so it is not treated as a marker.
func HasConflictMarkers(data []byte) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		for _, marker := range [][]byte{[]byte("<<<<<<<"), []byte(">>>>>>>"), []byte("|||||||")} {
			if bytes.HasPrefix(line, marker) && (len(line) == len(marker) || line[len(marker)] == ' ') {
				return true
			}
		}
	}
	return false
}
