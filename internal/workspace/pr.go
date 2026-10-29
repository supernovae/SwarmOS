package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/safefs"
)

var prCommitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// CompilePRHarnessBundleAt reads the approved issue's base-pinned harness,
// then replaces only the managed active pointer for the PR lifecycle.
func CompilePRHarnessBundleAt(worktreePath string, wu *contract.WorkUnit, wf *contract.WorkflowSpec, arts []contract.Artifact, role, baseSHA string, prNumber int, revision ...*RevisionPlanContext) error {
	if prNumber <= 0 || wu == nil {
		return fmt.Errorf("PR number and linked WorkUnit are required")
	}
	if err := CompileHarnessBundleAt(worktreePath, wu, wf, arts, role, baseSHA); err != nil {
		return err
	}
	var c *RevisionPlanContext
	if len(revision) > 0 {
		c = revision[0]
	}
	if err := WriteRevisionContext(worktreePath, c); err != nil {
		return err
	}
	if role == "" {
		role = "planner"
	}
	pointer := fmt.Sprintf(`%s
## Active SwarmOS Pull Request #%d (Work Unit %s, Issue #%d)

- **Active Role**: `+"`%s`"+`
- **Work Unit Contract**: `+"`.swarm/active/WORKUNIT.md`"+`
- **System & Role Instructions**: `+"`.swarm/active/SYSTEM_PROMPT.md`"+`
- **Plan Artifact Path**: `+"`.swarm/active/PLAN.md`"+`
- **Test Plan Artifact Path**: `+"`.swarm/active/TEST_PLAN.md`"+`
- **Review Artifact Path**: `+"`.swarm/active/REVIEW.md`"+`
- **PR lifecycle**: `+"`swarm pr status %d`"+` -> `+"`swarm pr work %d`"+` -> `+"`swarm pr validate %d`"+` -> `+"`swarm pr submit %d`"+`
- **Interactive artifacts**: /swarm-plan -> `+"`swarm pr stage %d --phase plan`"+` -> `+"`swarm issue review-plan %[12]d`"+` (configured gate) -> /swarm-implement -> `+"`swarm pr validate %[6]d`"+` -> `+"`swarm pr stage %[11]d --phase implement`"+` -> `+"`swarm issue renew %[12]d`"+` -> `+"`swarm pr submit %[6]d`"+`
- **Human plan gate**: an independent maintainer approves the linked plan with `+"`swarm issue review-plan %[12]d --approve`"+`

Always read `+"`.swarm/active/WORKUNIT.md`"+` and `+"`.swarm/active/SYSTEM_PROMPT.md`"+` before making changes.
%[13]s`, ActiveMarkerStart, prNumber, wu.ID, wu.IssueNumber, role, prNumber, prNumber, prNumber, prNumber, prNumber, prNumber, wu.IssueNumber, ActiveMarkerEnd)
	return injectActivePointer(worktreePath, pointer)
}

type PRWorkspaceOptions struct {
	Number int
	Remote string
	// BaseRemote, when set, supplies the approved base instead of Remote.
	BaseRemote          string
	HeadBranch          string
	HeadSHA             string
	LocalHeadSHA        string
	BaseSHA             string
	AllowHarnessMarkers bool
	RequireBaseAncestor bool
}

func PRWorktreePath(root string, number int) string {
	return filepath.Join(root, ".swarm", "worktrees", fmt.Sprintf("pr-%d", number))
}

func PRBranchName(number int) string { return fmt.Sprintf("swarm/pr-%d", number) }

func prCommit(ctx context.Context, path, sha string) error {
	if !prCommitSHA.MatchString(sha) {
		return fmt.Errorf("expected full commit SHA, got %q", sha)
	}
	actual, err := gitOutputContext(ctx, path, "rev-parse", "--verify", sha+"^{commit}")
	if err != nil {
		return fmt.Errorf("commit %s is unavailable: %w", sha, err)
	}
	if !strings.EqualFold(strings.TrimSpace(actual), sha) {
		return fmt.Errorf("commit %s resolved to %s", sha, strings.TrimSpace(actual))
	}
	return nil
}

func prRef(ctx context.Context, root, ref string) error {
	if strings.HasPrefix(ref, "-") || strings.Contains(ref, ":") || ref == "" {
		return fmt.Errorf("invalid branch %q", ref)
	}
	if _, err := gitOutputContext(ctx, root, "check-ref-format", "refs/heads/"+ref); err != nil {
		return fmt.Errorf("invalid branch %q: %w", ref, err)
	}
	return nil
}

// PreparePR creates a separate managed worktree; existing issue worktrees and
// locally committed PR work are never reset or removed.
func PreparePR(ctx context.Context, root string, opts PRWorkspaceOptions) (string, string, error) {
	if opts.Number <= 0 {
		return "", "", fmt.Errorf("invalid PR number %d", opts.Number)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	if err := prRef(ctx, abs, opts.HeadBranch); err != nil {
		return "", "", err
	}
	for _, sha := range []string{opts.HeadSHA, opts.BaseSHA} {
		if !prCommitSHA.MatchString(sha) {
			return "", "", fmt.Errorf("expected full commit SHA, got %q", sha)
		}
	}
	if opts.LocalHeadSHA != "" && !prCommitSHA.MatchString(opts.LocalHeadSHA) {
		return "", "", fmt.Errorf("expected full local PR commit SHA, got %q", opts.LocalHeadSHA)
	}
	if opts.Remote != "" {
		if strings.HasPrefix(opts.Remote, "-") || strings.ContainsAny(opts.Remote, "\x00\n") {
			return "", "", fmt.Errorf("invalid remote %q", opts.Remote)
		}
		if _, err := gitOutputContext(ctx, abs, "remote", "get-url", opts.Remote); err != nil {
			return "", "", fmt.Errorf("remote %q unavailable: %w", opts.Remote, err)
		}
		ref := "refs/heads/" + opts.HeadBranch
		observed, err := gitOutputContext(ctx, abs, "ls-remote", "--refs", opts.Remote, ref)
		if err != nil {
			return "", "", err
		}
		if strings.TrimSpace(observed) != opts.HeadSHA+"\t"+ref {
			return "", "", fmt.Errorf("remote %s branch %s does not match expected PR head %s", opts.Remote, ref, opts.HeadSHA)
		}
		if err := prCommit(ctx, abs, opts.HeadSHA); err != nil {
			if _, err := gitOutputContext(ctx, abs, "fetch", "--no-write-fetch-head", opts.Remote, opts.HeadSHA); err != nil {
				return "", "", fmt.Errorf("fetch expected PR head: %w", err)
			}
		}
		if err := prCommit(ctx, abs, opts.BaseSHA); err != nil {
			baseRemote := opts.Remote
			if opts.BaseRemote != "" {
				if err := ValidateRemoteName(opts.BaseRemote); err != nil {
					return "", "", err
				}
				baseRemote = opts.BaseRemote
			}
			_, err := gitOutputContext(ctx, abs, "fetch", "--no-write-fetch-head", baseRemote, opts.BaseSHA)
			if err != nil && baseRemote != opts.Remote {
				_, err = gitOutputContext(ctx, abs, "fetch", "--no-write-fetch-head", opts.Remote, opts.BaseSHA)
			}
			if err != nil {
				return "", "", fmt.Errorf("fetch approved PR base: %w", err)
			}
		}
	}
	for _, sha := range []string{opts.HeadSHA, opts.BaseSHA} {
		if err := prCommit(ctx, abs, sha); err != nil {
			return "", "", err
		}
	}
	if opts.RequireBaseAncestor {
		candidate := opts.LocalHeadSHA
		if candidate == "" {
			candidate = opts.HeadSHA
		}
		if _, err := gitOutputContext(ctx, abs, "merge-base", "--is-ancestor", opts.BaseSHA, candidate); err != nil {
			return "", "", fmt.Errorf("PR candidate does not include the approved base; run swarm pr rebase %d: %w", opts.Number, err)
		}
	}
	branch := PRBranchName(opts.Number)
	path := PRWorktreePath(abs, opts.Number)
	safe, err := safefs.Open(abs)
	if err != nil {
		return "", "", err
	}
	defer safe.Close()
	parents, err := safe.Sub(filepath.Join(".swarm", "worktrees"), true, 0755)
	if err != nil {
		return "", "", err
	}
	defer parents.Close()
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf("PR worktree path %q must be a real directory", path)
		}
		local, dirty, rebasing, err := PRHead(ctx, path)
		if err != nil {
			return "", "", err
		}
		if rebasing {
			if err := verifyPRRebase(ctx, path, opts.Number); err != nil {
				return "", "", err
			}
			ref, err := gitOutputContext(ctx, abs, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
			expected := opts.HeadSHA
			if opts.LocalHeadSHA != "" {
				expected = opts.LocalHeadSHA
			}
			if err != nil || strings.TrimSpace(ref) != expected {
				return "", "", fmt.Errorf("managed PR branch changed during rebase; preserve conflicts and inspect branch")
			}
			return path, branch, nil
		}
		if err := verifyWorktree(path, branch, ""); err != nil {
			return "", "", err
		}
		if dirty {
			if !opts.AllowHarnessMarkers {
				return "", "", fmt.Errorf("PR worktree is dirty; stage or preserve changes before retrying")
			}
			if err := verifyUncheckpointedWorktree(path, local); err != nil {
				return "", "", err
			}
		}
		if opts.LocalHeadSHA != "" {
			if local != opts.LocalHeadSHA {
				return "", "", fmt.Errorf("managed PR head %s differs from recorded local head %s", local, opts.LocalHeadSHA)
			}
		} else if _, err := gitOutputContext(ctx, path, "merge-base", "--is-ancestor", opts.HeadSHA, local); err != nil {
			// A worktree left at an earlier published head catches up to the
			// PR's current head; real divergence is still refused.
			if _, behind := gitOutputContext(ctx, path, "merge-base", "--is-ancestor", local, opts.HeadSHA); behind != nil {
				return "", "", fmt.Errorf("PR worktree head %s diverged from expected PR head %s", local, opts.HeadSHA)
			}
			if err := CleanEphemeralHarnessMarkers(path); err != nil {
				return "", "", err
			}
			if _, err := gitOutputContext(ctx, path, "merge", "--ff-only", "--quiet", opts.HeadSHA); err != nil {
				return "", "", fmt.Errorf("fast-forward PR worktree to published head %s: %w", opts.HeadSHA, err)
			}
		}
		return path, branch, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	// Drop registrations for PR worktrees removed by hand so worktree add succeeds.
	_, _ = gitOutputContext(ctx, abs, "worktree", "prune")
	if out, err := gitOutputContext(ctx, abs, "show-ref", "--verify", "--hash", "refs/heads/"+branch); err == nil {
		local := strings.TrimSpace(out)
		if opts.LocalHeadSHA != "" {
			if local != opts.LocalHeadSHA {
				return "", "", fmt.Errorf("existing PR branch %s differs from recorded local head %s", branch, opts.LocalHeadSHA)
			}
		} else if _, err := gitOutputContext(ctx, abs, "merge-base", "--is-ancestor", opts.HeadSHA, local); err != nil {
			if _, behind := gitOutputContext(ctx, abs, "merge-base", "--is-ancestor", local, opts.HeadSHA); behind != nil {
				return "", "", fmt.Errorf("existing PR branch %s diverged from expected PR head %s", branch, opts.HeadSHA)
			}
			// Fast-forward only: the branch is strictly behind the published head.
			if _, err := gitOutputContext(ctx, abs, "update-ref", "refs/heads/"+branch, opts.HeadSHA, local); err != nil {
				return "", "", fmt.Errorf("fast-forward PR branch %s to published head %s: %w", branch, opts.HeadSHA, err)
			}
		}
	} else {
		if opts.LocalHeadSHA != "" && opts.LocalHeadSHA != opts.HeadSHA {
			return "", "", fmt.Errorf("recorded local PR head %s is missing; refusing to recreate it", opts.LocalHeadSHA)
		}
		if _, err := gitOutputContext(ctx, abs, "branch", branch, opts.HeadSHA); err != nil {
			return "", "", fmt.Errorf("create PR branch: %w", err)
		}
	}
	if _, err := gitOutputContext(ctx, abs, "worktree", "add", "--", path, branch); err != nil {
		return "", "", fmt.Errorf("add PR worktree: %w", err)
	}
	if err := verifyWorktree(path, branch, ""); err != nil {
		return "", "", err
	}
	return path, branch, nil
}

// VerifyPRWorktree checks a recorded PR checkout without rejecting uncommitted
// implementation files. Manual staging uses it before checkpointing those files.
func VerifyPRWorktree(ctx context.Context, root string, number int, expectedHead string) (string, error) {
	if number <= 0 || !prCommitSHA.MatchString(expectedHead) {
		return "", fmt.Errorf("PR number and exact recorded head are required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	safe, err := safefs.Open(abs)
	if err != nil {
		return "", err
	}
	defer safe.Close()
	parents, err := safe.Sub(filepath.Join(".swarm", "worktrees"), false, 0755)
	if err != nil {
		return "", err
	}
	defer parents.Close()
	path := PRWorktreePath(abs, number)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("PR worktree must be a real directory")
	}
	if err := verifyWorktree(path, PRBranchName(number), expectedHead); err != nil {
		return "", err
	}
	common, err := gitOutputContext(ctx, abs, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	actual, err := gitOutputContext(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !samePath(strings.TrimSpace(common), strings.TrimSpace(actual)) {
		return "", fmt.Errorf("PR worktree belongs to another repository")
	}
	rebasing, err := prRebasing(ctx, path)
	if err != nil {
		return "", err
	}
	if rebasing {
		return "", fmt.Errorf("finish or abort the PR rebase before staging")
	}
	return path, nil
}

func prRebasing(ctx context.Context, path string) (bool, error) {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		gitPath, err := gitOutputContext(ctx, path, "rev-parse", "--git-path", name)
		if err != nil {
			return false, err
		}
		if _, err := os.Lstat(strings.TrimSpace(gitPath)); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func PRHead(ctx context.Context, path string) (string, bool, bool, error) {
	return prHead(ctx, path, prHeadRaw)
}

// InspectPRHead reads worktree state without refreshing the Git index.
func InspectPRHead(ctx context.Context, path string) (string, bool, bool, error) {
	return prHead(ctx, path, prHeadReadOnly)
}

// StatusPRHead is InspectPRHead for display: SwarmOS's own instruction
// pointer block does not make the worktree dirty.
func StatusPRHead(ctx context.Context, path string) (string, bool, bool, error) {
	return prHead(ctx, path, prHeadStatus)
}

type prHeadMode int

const (
	prHeadRaw prHeadMode = iota
	prHeadReadOnly
	prHeadStatus
)

func prHead(ctx context.Context, path string, mode prHeadMode) (string, bool, bool, error) {
	head, err := gitOutputContext(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", false, false, err
	}
	var dirty bool
	if mode == prHeadStatus {
		dirty, err = hasStatusChanges(ctx, path)
	} else {
		args := []string{"status", "--porcelain", "-z", "--untracked-files=all"}
		if mode == prHeadReadOnly {
			args = append([]string{"--no-optional-locks"}, args...)
		}
		var status string
		status, err = gitOutputContext(ctx, path, args...)
		dirty = status != ""
	}
	if err != nil {
		return "", false, false, err
	}
	rebasing, err := prRebasing(ctx, path)
	return strings.TrimSpace(head), dirty, rebasing, err
}

func prRebaseValue(ctx context.Context, path, key string) (string, error) {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		name, err := gitOutputContext(ctx, path, "rev-parse", "--git-path", dir+"/"+key)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(strings.TrimSpace(name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	return "", fmt.Errorf("missing rebase metadata %s", key)
}

func verifyPRRebase(ctx context.Context, path string, number int) error {
	branch, err := prRebaseValue(ctx, path, "head-name")
	if err != nil || branch != "refs/heads/"+PRBranchName(number) {
		return fmt.Errorf("not a managed PR rebase for #%d", number)
	}
	return nil
}

// RebasePR never cleans conflict files. Continue uses a noninteractive editor
// while allowing Git to retain its own in-progress conflict state on failure.
func RebasePR(ctx context.Context, path, baseSHA string, resume, abort bool) (string, error) {
	if resume && abort {
		return "", errors.New("rebase continue and abort are mutually exclusive")
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		return "", fmt.Errorf("inspect PR worktree: %w", statErr)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("PR worktree must be a real managed directory: %s", path)
	}
	numberText := strings.TrimPrefix(filepath.Base(path), "pr-")
	number, parseErr := strconv.Atoi(numberText)
	if parseErr != nil || number <= 0 || filepath.Base(filepath.Dir(path)) != "worktrees" || filepath.Base(filepath.Dir(filepath.Dir(path))) != ".swarm" {
		return "", fmt.Errorf("not a managed PR worktree: %s", path)
	}
	gitFile, statErr := os.Lstat(filepath.Join(path, ".git"))
	if statErr != nil || !gitFile.Mode().IsRegular() {
		return "", fmt.Errorf("not a registered PR worktree: %s", path)
	}
	top, err := gitOutputContext(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil || !samePath(strings.TrimSpace(top), path) {
		return "", fmt.Errorf("not a registered PR worktree: %s", path)
	}
	head, dirty, rebasing, err := PRHead(ctx, path)
	if err != nil {
		return "", err
	}
	if rebasing && !resume && !abort {
		return "", fmt.Errorf("rebase already in progress; run swarm pr rebase %d --continue or --abort", number)
	}
	branch, err := gitOutputContext(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(branch) != PRBranchName(number) {
		if !resume && !abort {
			return "", fmt.Errorf("not on managed branch %s", PRBranchName(number))
		}
		if err := verifyPRRebase(ctx, path, number); err != nil {
			return "", err
		}
	}

	if resume || abort {
		if !rebasing {
			return "", fmt.Errorf("no managed rebase in progress")
		}
		if resume {
			if err := prCommit(ctx, path, baseSHA); err != nil {
				return "", err
			}
			onto, err := prRebaseValue(ctx, path, "onto")
			if err != nil || onto != baseSHA {
				return "", fmt.Errorf("in-progress rebase target differs from approved base; abort and restart the rebase")
			}
		}
		operation := "--continue"
		if abort {
			operation = "--abort"
		}
		cmd, redact, err := gitcredential.Command(ctx, path, "rebase", operation)
		if err != nil {
			return "", err
		}
		cmd.Env = append(cmd.Env, "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true", "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git rebase %s: %w: %s", operation, err, redact(strings.TrimSpace(normalizeGitOutput(string(out)))))
		}
		newHead, _, _, err := PRHead(ctx, path)
		return newHead, err
	}
	if rebasing {
		return "", fmt.Errorf("rebase already in progress; run swarm pr rebase %d --continue or --abort", number)
	}
	if dirty {
		return "", fmt.Errorf("PR worktree is dirty; preserve it before retrying")
	}
	if err := prCommit(ctx, path, baseSHA); err != nil {
		return "", err
	}
	if _, err := gitOutputContext(ctx, path, "rebase", "--no-autostash", baseSHA); err != nil {
		return "", fmt.Errorf("rebase PR head %s onto %s (conflicts preserved): %w", head, baseSHA, err)
	}
	newHead, _, _, err := PRHead(ctx, path)
	return newHead, err
}

// PublishPR uses the caller's immutable snapshot as the sole lease; it cannot
// create a missing remote branch or replace a head modified by another writer.
func PublishPR(ctx context.Context, root, remote, branch, expectedHead, head string) error {
	if remote == "" || strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, "\x00\n") {
		return fmt.Errorf("invalid remote %q", remote)
	}
	if err := prRef(ctx, root, branch); err != nil {
		return err
	}
	if err := prCommit(ctx, root, expectedHead); err != nil {
		return err
	}
	if err := prCommit(ctx, root, head); err != nil {
		return err
	}
	if err := RejectRecoveryOutput(ctx, root, head); err != nil {
		return err
	}
	if _, err := gitOutputContext(ctx, root, "remote", "get-url", remote); err != nil {
		return fmt.Errorf("remote %q unavailable: %w", remote, err)
	}
	ref := "refs/heads/" + branch
	observed, err := gitOutputContext(ctx, root, "ls-remote", "--refs", remote, ref)
	if err != nil {
		return err
	}
	if strings.TrimSpace(observed) != expectedHead+"\t"+ref {
		return fmt.Errorf("remote %s branch %s no longer points to expected head %s", remote, branch, expectedHead)
	}
	_, err = gitOutputContext(ctx, root, "push", remote, "--force-with-lease="+ref+":"+expectedHead, head+":"+ref)
	if err != nil {
		return fmt.Errorf("publish PR head with exact remote lease: %w", err)
	}
	return nil
}
