package workspace

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/safefs"
	"os"
	"path/filepath"
	"strings"
)

// DroppedCheckpoint is coordinator-owned identity for a contained checkpoint.
// Its remote head remains the publication expectation until the next checkpoint.
type DroppedCheckpoint struct {
	Branch  string `json:"branch"`
	HeadSHA string `json:"head_sha"`
}

func provisionDroppedCheckpoint(root string, issue int, title, base string, retired *DroppedCheckpoint) (string, string, error) {
	refuse := func(err error) (string, string, error) {
		return "", "", fmt.Errorf("cannot recover dropped checkpoint: %w", err)
	}
	if issue <= 0 || !strings.HasPrefix(retired.Branch, fmt.Sprintf("swarm/issue-%d-", issue)) || len(retired.HeadSHA) != 40 {
		return refuse(errors.New("invalid retired identity"))
	}
	if _, err := gitOutput(root, "check-ref-format", "refs/heads/"+retired.Branch); err != nil {
		return refuse(err)
	}
	resolved, err := resolveCommit(root, base)
	if err != nil {
		return refuse(err)
	}
	base = resolved
	contained := func(head string) error {
		ok, err := BaseIsAncestor(context.Background(), root, head, base)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("commit %s is not contained in approved base %s", head, base)
		}
		return nil
	}
	if err := contained(retired.HeadSHA); err != nil {
		return refuse(err)
	}
	// A missing remote branch also fails closed: publication must still expect
	// exactly the recorded retired head.
	if _, err := gitOutput(root, "remote", "get-url", "origin"); err == nil {
		out, err := gitOutput(root, "ls-remote", "--refs", "origin", "refs/heads/"+retired.Branch)
		if err != nil {
			return refuse(err)
		}
		fields := strings.Fields(out)
		if len(fields) != 2 || fields[0] != retired.HeadSHA {
			return refuse(fmt.Errorf("remote checkpoint is %q, expected %s", strings.TrimSpace(out), retired.HeadSHA))
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return refuse(err)
	}
	path := WorktreePath(absRoot, issue)
	ref := "refs/heads/" + retired.Branch
	local, localErr := resolveCommit(root, ref)
	if localErr == nil {
		if err := contained(local); err != nil {
			return refuse(err)
		}
	}
	info, statErr := os.Lstat(path)
	if statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return refuse(errors.New("worktree must be a real directory"))
		}
		if localErr != nil {
			return refuse(localErr)
		}
		if err := verifyWorktree(path, retired.Branch, local); err != nil {
			return refuse(err)
		}
		common, err := gitOutput(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return refuse(err)
		}
		expected, err := gitOutput(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil || !samePath(strings.TrimSpace(common), strings.TrimSpace(expected)) {
			return refuse(errors.New("worktree belongs to another repository"))
		}
		if err := verifyRecoveryIndex(path, local); err != nil {
			return refuse(err)
		}
		if err := verifyUncheckpointedWorktree(path, local); err != nil {
			return refuse(err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return refuse(statErr)
	} else {
		// Never move a branch checked out elsewhere, including stale registrations.
		registered, err := gitOutput(root, "worktree", "list", "--porcelain")
		if err != nil {
			return refuse(err)
		}
		if strings.Contains(registered, "branch "+ref+"\n") {
			return refuse(errors.New("task branch is registered to another worktree"))
		}
	}
	// Validate/create root-bound destination directories only after all Git and
	// content checks, before changing any local task ref.
	safe, err := safefs.Open(absRoot)
	if err != nil {
		return refuse(err)
	}
	defer safe.Close()
	parents, err := safe.Sub(filepath.Join(".swarm", "worktrees"), true, 0755)
	if err != nil {
		return refuse(err)
	}
	defer parents.Close()
	// All refusal checks precede marker cleanup and ref/worktree changes.
	if statErr == nil {
		if err := verifyWorktree(path, retired.Branch, local); err != nil {
			return refuse(err)
		}
		// Restore only the verified marker-only index changes, then remove
		// pointers using HEAD's tracked-file identities.
		if _, err := gitOutput(path, "restore", "--staged", "--", "."); err != nil {
			return refuse(err)
		}
		if err := CleanEphemeralHarnessMarkers(path); err != nil {
			return refuse(err)
		}
		if _, err := gitOutput(path, "merge", "--ff-only", base); err != nil {
			return refuse(err)
		}
	} else {
		if localErr != nil {
			local = strings.Repeat("0", 40)
		}
		if _, err := gitOutput(root, "update-ref", ref, base, local); err != nil {
			return refuse(err)
		}
	}
	return ProvisionFromRemoteAt(root, "origin", issue, title, base, retired.Branch, "")
}

// Check the index independently: matching working bytes cannot authorize
// discarding different staged content.
func verifyRecoveryIndex(path, head string) error {
	names, err := gitOutput(path, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return err
	}
	pointerPaths := instructionPaths(path)
	for _, name := range strings.Split(names, "\x00") {
		if name == "" {
			continue
		}
		if !pointerPaths[name] {
			return fmt.Errorf("staged changes in %s", name)
		}
		staged, err := gitOutput(path, "show", ":"+name)
		if err != nil {
			return err
		}
		expected, err := gitOutput(path, "show", head+":"+name)
		if err != nil {
			listing, listErr := gitOutput(path, "ls-tree", head, "--", name)
			if listErr != nil || listing != "" {
				return fmt.Errorf("cannot verify staged instruction %s", name)
			}
			expected = ""
		}
		if strings.ReplaceAll(StripActivePointer(staged), "\r\n", "\n") != strings.ReplaceAll(expected, "\r\n", "\n") {
			return fmt.Errorf("staged instruction content changed in %s", name)
		}
		raw, err := gitOutput(path, "diff", "--cached", "--raw", "--", name)
		if err != nil {
			return err
		}
		fields := strings.Fields(raw)
		if len(fields) < 2 || (fields[0] != ":000000" && strings.TrimPrefix(fields[0], ":") != fields[1]) {
			return fmt.Errorf("staged instruction mode changed in %s", name)
		}
	}
	return nil
}
