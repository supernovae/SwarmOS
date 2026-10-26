package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/safefs"
)

// CheckpointTree stages checkpoint content in a private index without changing
// the candidate files or the caller's index.
func CheckpointTree(ctx context.Context, root string) (string, error) {
	if err := RejectRecoveryOutput(ctx, root, "HEAD"); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "swarm-validation-index-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	index := filepath.Join(dir, "index")
	runInput := func(input string, args ...string) (string, error) {
		cmd, redact, err := gitcredential.Command(ctx, root, args...)
		if err != nil {
			return "", err
		}
		// Apply Git options after Command has inspected the subcommand and
		// installed its credential, filter, and execution protections.
		cmd.Args = append([]string{cmd.Args[0], "-c", "core.splitIndex=false", "-c", "core.untrackedCache=false"}, cmd.Args[1:]...)
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
		cmd.Stdin = strings.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("temporary index git: %w: %s", err, redact(stderr.String()))
		}
		return string(out), nil
	}
	run := func(args ...string) (string, error) { return runInput("", args...) }
	original, err := gitOutputContext(ctx, root, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(original)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var f *os.File
		f, err = os.OpenFile(index, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			_, err = f.Write(data)
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
		}
	} else if os.IsNotExist(err) {
		_, err = run("read-tree", "--empty")
	}
	if err != nil {
		return "", err
	}
	// Remember tracking before staging: an injected, untracked pointer-only file
	// is removed by checkpoint cleanup, whereas a tracked empty file is retained.
	tracked := map[string]bool{}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		out, e := run("ls-files", "--", name)
		if e != nil {
			return "", e
		}
		tracked[name] = out != ""
	}
	// Reject unmerged entries before rebuilding. Keep the original staged names,
	// including intent-to-add paths that write-tree would otherwise omit.
	if _, err := run("write-tree"); err != nil {
		return "", err
	}
	entries, err := run("ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	// A copied index has a new timestamp but old stat entries, making racy
	// edits appear clean. Rebuild with cache entries only, so staging rehashes
	// every file. No stat data or split-index references survive this step.
	if err := os.Remove(index); err != nil {
		return "", err
	}
	if _, err := run("read-tree", "--empty"); err != nil {
		return "", err
	}
	if _, err := runInput(entries, "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	if err := stageCheckpointFilesWith(run); err != nil {
		return "", err
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		info, e := os.Lstat(filepath.Join(root, name))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return "", e
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("refuse non-regular instruction file %s", name)
		}
		data, e := safefs.ReadFile(root, name, 16<<20)
		if e != nil {
			return "", e
		}
		cleaned := StripActivePointer(string(data))
		if cleaned == string(data) {
			continue
		}
		if cleaned == "" && !tracked[name] {
			if _, e := run("update-index", "--force-remove", "--", name); e != nil {
				return "", e
			}
			continue
		}
		blob, e := runInput(cleaned, "hash-object", "-w", "--stdin", "--path="+name)
		if e != nil {
			return "", e
		}
		entry, e := run("ls-files", "--stage", "--", name)
		if e != nil {
			return "", e
		}
		fields := strings.Fields(entry)
		if len(fields) < 3 {
			continue
		}
		if _, e := run("update-index", "--add", "--cacheinfo", fields[0], strings.TrimSpace(blob), name); e != nil {
			return "", e
		}
	}
	tree, err := run("write-tree")
	return strings.TrimSpace(tree), err
}
