package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/safefs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/supernovae/swarmos/internal/gitcredential"
)

const (
	ActiveMarkerStart = "<!-- swarm:active:start -->"
	ActiveMarkerEnd   = "<!-- swarm:active:end -->"
)

var (
	nonSlugRegex     = regexp.MustCompile(`[^a-z0-9]+`)
	activeBlockRegex = regexp.MustCompile(`(?s)<!-- swarm:active:start -->.*?<!-- swarm:active:end -->(?:\r?\n){0,2}`)
)

func Slugify(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	s = strings.TrimPrefix(s, "[swarm]")
	s = strings.TrimSpace(s)
	s = nonSlugRegex.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		return "workunit"
	}
	return s
}

func WorktreePath(repoRoot string, issueNum int) string {
	return filepath.Join(repoRoot, ".swarm", "worktrees", fmt.Sprintf("issue-%d", issueNum))
}

func BranchName(issueNum int, titleOrSlug string) string {
	return fmt.Sprintf("swarm/issue-%d-%s", issueNum, Slugify(titleOrSlug))
}

func ForkBranchName(issueNum int, claimID string) string {
	return fmt.Sprintf("swarm/issue-%d-%s", issueNum, strings.TrimSpace(claimID))
}

// DefaultForkRemote names a contributor's personal fork remote.
const DefaultForkRemote = "fork"

// ClaimCheckpoint returns where a claim's work is checkpointed: the recorded
// branch on origin (or the issue branch), or in fork mode a claim-scoped branch
// on the contributor's fork remote.
func ClaimCheckpoint(forkMode bool, forkRemote string, issueNum int, title, claimID, recorded string) (remote, branch string) {
	remote, branch = "origin", recorded
	if forkMode {
		remote = forkRemote
		if branch == "" {
			branch = ForkBranchName(issueNum, claimID)
		}
	}
	if branch == "" {
		branch = BranchName(issueNum, title)
	}
	return remote, branch
}

func FetchCommit(repoRoot, remoteName, sha string) error {
	if strings.TrimSpace(remoteName) == "" {
		remoteName = "origin"
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return fmt.Errorf("empty commit SHA")
	}
	if _, err := resolveCommit(repoRoot, sha); err == nil {
		return nil
	}
	if _, err := gitOutput(repoRoot, "remote", "get-url", remoteName); err != nil {
		return fmt.Errorf("remote %q is not configured: %w", remoteName, err)
	}
	if _, err := gitOutput(repoRoot, "fetch", "--no-write-fetch-head", remoteName, sha); err != nil {
		return fmt.Errorf("fetch commit %s from %s: %w", sha, remoteName, err)
	}
	return nil
}

// ResolveRemoteGitHubSlug resolves a git remote's owner/repo slug and rejects
// missing remotes or a fork remote that points to the same URL as origin.
func ResolveRemoteGitHubSlug(repoRoot, remoteName string) (string, error) {
	remoteName = strings.TrimSpace(remoteName)
	if remoteName == "" {
		return "", fmt.Errorf("remote name is required")
	}
	rawURL, err := gitOutput(repoRoot, "remote", "get-url", remoteName)
	if err != nil {
		return "", fmt.Errorf("git remote %q is not configured: %w", remoteName, err)
	}
	rawURL = strings.TrimSpace(rawURL)
	if remoteName != "origin" {
		if originURL, origErr := gitOutput(repoRoot, "remote", "get-url", "origin"); origErr == nil {
			if strings.TrimSpace(originURL) == rawURL {
				return "", fmt.Errorf("fork remote %q must not point to the same URL as upstream origin", remoteName)
			}
		}
	}
	if explicit, cfgErr := gitOutput(repoRoot, "config", "--get", "remote."+remoteName+".swarmSlug"); cfgErr == nil {
		if slug := strings.TrimSpace(explicit); strings.Count(slug, "/") == 1 {
			return slug, nil
		}
	}
	return parseGitRemoteSlug(rawURL)
}

func parseGitRemoteSlug(rawURL string) (string, error) {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return "", fmt.Errorf("empty git remote URL")
	}
	if strings.Contains(u, "://") {
		parsed, err := url.Parse(u)
		if err != nil {
			return "", fmt.Errorf("parse remote URL %q: %w", rawURL, err)
		}
		path := strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/")
		parts := strings.Split(path, "/")
		if len(parts) >= 2 && parts[len(parts)-2] != "" && parts[len(parts)-1] != "" {
			return parts[len(parts)-2] + "/" + parts[len(parts)-1], nil
		}
		return "", fmt.Errorf("could not extract owner/repo from remote URL %q", rawURL)
	}
	if idx := strings.Index(u, ":"); idx > 0 && !strings.Contains(u[:idx], "/") && !strings.Contains(u[:idx], `\`) {
		path := strings.Trim(strings.TrimSuffix(u[idx+1:], ".git"), "/")
		parts := strings.Split(path, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return parts[0] + "/" + parts[1], nil
		}
	}
	cleaned := filepath.ToSlash(filepath.Clean(strings.TrimSuffix(u, ".git")))
	parts := strings.Split(strings.Trim(cleaned, "/"), "/")
	if len(parts) >= 2 && parts[len(parts)-2] != "" && parts[len(parts)-1] != "" {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1], nil
	}
	return "", fmt.Errorf("could not extract GitHub owner/repo from remote URL %q", rawURL)
}

// PushForkCheckpoint pushes a local checkpoint commit to the verified fork
// remote using explicit --force-with-lease expectations (never to origin).
func PushForkCheckpoint(ctx context.Context, repoRoot, forkRemote, branch, headSHA string) error {
	forkRemote = strings.TrimSpace(forkRemote)
	if forkRemote == "" || forkRemote == "origin" {
		return fmt.Errorf("fork checkpoint push requires a non-origin fork remote (got %q)", forkRemote)
	}
	if !strings.HasPrefix(branch, "swarm/issue-") || strings.Contains(branch, "..") || strings.TrimSpace(headSHA) == "" {
		return fmt.Errorf("invalid fork checkpoint branch %q or head SHA %q", branch, headSHA)
	}
	branchRef := "refs/heads/" + branch
	lsOut, err := gitOutputContext(ctx, repoRoot, "ls-remote", "--refs", forkRemote, branchRef)
	if err != nil {
		return fmt.Errorf("query fork remote %q for %s: %w", forkRemote, branch, err)
	}
	observedSHA := ""
	if trimmed := strings.TrimSpace(lsOut); trimmed != "" {
		fields := strings.Fields(trimmed)
		if len(fields) != 2 || fields[1] != branchRef {
			return fmt.Errorf("invalid ls-remote output from fork remote %q for %s", forkRemote, branchRef)
		}
		observedSHA = fields[0]
	}
	leaseArg := branchRef + ":" + observedSHA
	if _, err := gitOutputContext(ctx, repoRoot, "push", forkRemote, "--force-with-lease="+leaseArg, headSHA+":"+branchRef); err != nil {
		return fmt.Errorf("push checkpoint to fork remote %q: %w", forkRemote, err)
	}
	return nil
}

// ResolveBase resolves the immutable commit used to create a worktree. When
// origin exists, its advertised default branch is authoritative; a local
// checkout branch must never become the base accidentally.
func ResolveBase(repoRoot string) (string, error) {
	if _, err := gitOutput(repoRoot, "rev-parse", "--show-toplevel"); err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	if _, err := gitOutput(repoRoot, "remote", "get-url", "origin"); err != nil {
		if !errors.Is(err, errGitFailed) {
			return "", err
		}
		return resolveCommit(repoRoot, "HEAD")
	}
	if _, err := gitOutput(repoRoot, "fetch", "--no-write-fetch-head", "--prune", "origin"); err != nil {
		return "", fmt.Errorf("fetch origin before resolving base: %w", err)
	}
	if _, err := gitOutput(repoRoot, "remote", "set-head", "origin", "--auto"); err != nil {
		return "", fmt.Errorf("discover advertised origin default branch: %w", err)
	}
	out, err := gitOutput(repoRoot, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("origin has no advertised default branch: %w", err)
	}
	return resolveCommit(repoRoot, strings.TrimSpace(out))
}

// ProvisionAt creates or reopens exactly the requested worktree branch.
// It never resets an existing branch and refuses an unrelated directory.
func ProvisionAt(repoRoot string, issueNum int, title, baseSHA, branch, headSHA string) (string, string, error) {
	return ProvisionFromRemoteAt(repoRoot, "origin", issueNum, title, baseSHA, branch, headSHA)
}

// ProvisionFromRemoteAt creates or reopens the requested worktree branch,
// fetching baseSHA from origin if needed and restoring any pinned checkpoint
// headSHA from checkpointRemote ("origin" in same-repo mode, or the verified
// fork remote in --fork mode).
func ProvisionFromRemoteAt(repoRoot, checkpointRemote string, issueNum int, title, baseSHA, branch, headSHA string) (string, string, error) {
	if issueNum <= 0 || baseSHA == "" {
		return "", "", errors.New("issue number and approved base SHA are required")
	}
	if strings.TrimSpace(checkpointRemote) == "" {
		checkpointRemote = "origin"
	}
	if branch == "" {
		branch = BranchName(issueNum, title)
	}
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", "", err
	}
	resolvedBase, err := resolveCommit(absRoot, baseSHA)
	if err != nil {
		if fetchErr := FetchCommit(absRoot, "origin", baseSHA); fetchErr == nil {
			resolvedBase, err = resolveCommit(absRoot, baseSHA)
		}
		if err != nil {
			return "", "", fmt.Errorf("resolve approved base %q: %w", baseSHA, err)
		}
	}
	baseSHA = resolvedBase
	if headSHA != "" {
		if _, err := gitOutput(absRoot, "remote", "get-url", checkpointRemote); err == nil {
			remote, err := gitOutput(absRoot, "ls-remote", "--refs", checkpointRemote, "refs/heads/"+branch)
			if err != nil {
				return "", "", err
			}
			fields := strings.Fields(remote)
			if len(fields) != 2 || fields[0] != headSHA {
				return "", "", fmt.Errorf("remote branch %q on %s does not match approved checkpoint %s", branch, checkpointRemote, headSHA)
			}
			if _, err := gitOutput(absRoot, "fetch", "--no-write-fetch-head", checkpointRemote, headSHA); err != nil {
				return "", "", fmt.Errorf("fetch pinned checkpoint from %s: %w", checkpointRemote, err)
			}
		}
	}
	checkpoint := headSHA
	if checkpoint == "" {
		checkpoint = baseSHA
	}
	if _, err := gitOutput(absRoot, "merge-base", "--is-ancestor", baseSHA, checkpoint); err != nil {
		return "", "", fmt.Errorf("checkpoint %s does not descend from approved base %s; reconcile the branch without resetting it: %w", checkpoint, baseSHA, err)
	}
	wtPath := WorktreePath(absRoot, issueNum)
	safe, err := safefs.Open(absRoot)
	if err != nil {
		return "", "", err
	}
	defer safe.Close()
	parents, err := safe.Sub(filepath.Join(".swarm", "worktrees"), true, 0755)
	if err != nil {
		return "", "", err
	}
	defer parents.Close()
	if info, err := os.Lstat(wtPath); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf("worktree path %q must be a real directory", wtPath)
		}
		if err := verifyWorktree(wtPath, branch, checkpoint); err != nil {
			return "", "", err
		}
		if headSHA == "" {
			if err := verifyUncheckpointedWorktree(wtPath, checkpoint); err != nil {
				return "", "", err
			}
		}
		if err := configureWorktreeExclude(wtPath); err != nil {
			return "", "", err
		}
		return wtPath, branch, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	_, _ = gitOutput(absRoot, "worktree", "prune")

	if headSHA != "" {
		if existing, err := resolveCommit(absRoot, branch); err == nil && existing != headSHA {
			return "", "", fmt.Errorf("existing branch %q is %s, expected checkpoint %s", branch, existing, headSHA)
		}
		if _, err := gitOutput(absRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
			if _, err := gitOutput(absRoot, "branch", branch, headSHA); err != nil {
				return "", "", fmt.Errorf("create checkpoint branch: %w", err)
			}
		}
	} else if _, err := gitOutput(absRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		if _, err := gitOutput(absRoot, "branch", branch, baseSHA); err != nil {
			return "", "", fmt.Errorf("create worktree branch: %w", err)
		}
	}
	if existing, err := resolveCommit(absRoot, branch); err == nil && existing != checkpoint {
		return "", "", fmt.Errorf("existing branch %q is at %s, expected approved checkpoint %s; preserve it under another branch before retrying", branch, existing, checkpoint)
	}
	if _, err := gitOutput(absRoot, "worktree", "add", "--", wtPath, branch); err != nil {
		return "", "", fmt.Errorf("add worktree: %w", err)
	}
	if err := verifyWorktree(wtPath, branch, headSHA); err != nil {
		return "", "", err
	}
	if err := configureWorktreeExclude(wtPath); err != nil {
		return "", "", err
	}
	return wtPath, branch, nil
}

// CheckpointOptions describes the task and reason for a worktree checkpoint.
type CheckpointOptions struct {
	WorkUnit *contract.WorkUnit
	Purpose  string // implement, repair, or submit
	Detail   string // empty (concise), concise, or detailed
}

// Checkpoint commits the worktree's current changes without rewriting history.
// Publication belongs to the fenced coordination store, never this helper.
func Checkpoint(ctx context.Context, wtPath, branch string, opts CheckpointOptions) (string, error) {
	if branch == "" {
		return "", errors.New("checkpoint branch is required")
	}
	if opts.WorkUnit == nil || opts.WorkUnit.IssueNumber <= 0 || strings.TrimSpace(opts.WorkUnit.Title) == "" {
		return "", errors.New("checkpoint requires a work unit with issue number and title")
	}
	if err := contract.ValidateCommitMessageDetail(opts.Detail); err != nil {
		return "", err
	}
	var action string
	switch opts.Purpose {
	case "implement":
		action = ""
	case "repair":
		action = "Repair validation"
	case "submit":
		action = "Finalize"
	default:
		return "", fmt.Errorf("invalid checkpoint purpose %q", opts.Purpose)
	}
	if err := verifyWorktree(wtPath, branch, ""); err != nil {
		return "", err
	}
	if err := CleanEphemeralHarnessMarkers(wtPath); err != nil {
		return "", err
	}
	status, err := gitOutputContext(ctx, wtPath, "status", "--porcelain=v1")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) != "" {
		if _, err := gitOutputContext(ctx, wtPath, "add", "-A"); err != nil {
			return "", err
		}
		// Staged changes can cancel unstaged changes, leaving no commit to make.
		diff, err := gitOutputContext(ctx, wtPath, "diff", "--cached", "--name-status", "-z", "--no-renames", "HEAD")
		if err != nil {
			return "", err
		}
		if diff != "" {
			suffix := fmt.Sprintf(" (#%d)", opts.WorkUnit.IssueNumber)
			prefix := ""
			if action != "" {
				prefix = action + ": "
			}
			title := checkpointText(opts.WorkUnit.Title)
			title = truncateCheckpointText(title, 72-len(prefix)-len(suffix))
			subject := prefix + title + suffix
			args := []string{"commit", "-m", subject}
			if opts.Detail == "detailed" {
				args = append(args, "-m", checkpointBody(opts.WorkUnit.Objective, diff))
			}
			if _, err := gitOutputContext(ctx, wtPath, args...); err != nil {
				return "", fmt.Errorf("commit checkpoint: %w", err)
			}
		}
	}
	head, err := resolveCommit(wtPath, "HEAD")
	if err != nil {
		return "", err
	}
	return head, nil
}

// checkpointText keeps arbitrary task text on a single safe line.
func checkpointText(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError
	}), " ")
}

func truncateCheckpointText(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	// Keep rune boundaries and reserve space for the ellipsis.
	var end int
	for i, r := range s {
		if i+utf8.RuneLen(r) > maxBytes-3 {
			break
		}
		end = i + utf8.RuneLen(r)
	}
	if maxBytes < 3 {
		return s[:0]
	}
	return strings.TrimSpace(s[:end]) + "..."
}

func truncateCheckpointPath(path string) string {
	const maxBytes = 160
	if len(path) <= maxBytes {
		return path
	}
	end := 0
	for i := range path {
		if i > maxBytes-3 {
			break
		}
		end = i
	}
	return path[:end] + "..."
}

func checkpointBody(objective, diff string) string {
	var body strings.Builder
	if objective = truncateCheckpointText(checkpointText(objective), 500); objective != "" {
		body.WriteString("Objective: ")
		body.WriteString(objective)
		body.WriteString("\n\n")
	}
	// --name-status -z --no-renames emits status\0path\0 pairs, in Git's
	// stable pathname ordering. Only these staged paths inform the message.
	parts := strings.Split(diff, "\x00")
	count := (len(parts) - 1) / 2
	body.WriteString("Staged changes (")
	body.WriteString(strconv.Itoa(count))
	body.WriteString(" file")
	if count != 1 {
		body.WriteByte('s')
	}
	body.WriteString("):")
	const maxFiles = 20
	for i := 0; i < count && i < maxFiles; i++ {
		var kind string
		switch parts[2*i] {
		case "A":
			kind = "added"
		case "D":
			kind = "deleted"
		default:
			kind = "modified"
		}
		body.WriteString("\n- ")
		body.WriteString(kind)
		body.WriteByte(' ')
		body.WriteString(strconv.Quote(truncateCheckpointPath(parts[2*i+1])))
	}
	if count > maxFiles {
		body.WriteString("\n- ... and ")
		body.WriteString(strconv.Itoa(count - maxFiles))
		body.WriteString(" more files")
	}
	return body.String()
}

var errGitFailed = errors.New("git command failed")

func gitOutput(repoRoot string, args ...string) (string, error) {
	return gitOutputContext(context.Background(), repoRoot, args...)
}

func gitOutputContext(ctx context.Context, repoRoot string, args ...string) (string, error) {
	cmd, redact, err := gitcredential.Command(ctx, repoRoot, args...)
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: git %s: %s", errGitFailed, strings.Join(args, " "), redact(strings.TrimSpace(normalizeGitOutput(string(out)))))
	}
	return redact(string(out)), nil
}

func resolveCommit(repoRoot, revision string) (string, error) {
	out, err := gitOutput(repoRoot, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func verifyWorktree(wtPath, branch, expectedHead string) error {
	gitEntry, err := os.Lstat(filepath.Join(wtPath, ".git"))
	if err != nil || !gitEntry.Mode().IsRegular() {
		return fmt.Errorf("existing path %q is not a registered linked worktree", wtPath)
	}
	top, err := gitOutput(wtPath, "rev-parse", "--show-toplevel")
	if err != nil || !samePath(strings.TrimSpace(top), wtPath) {
		return fmt.Errorf("existing worktree %q is not the requested git worktree", wtPath)
	}
	currentBranch, err := gitOutput(wtPath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(currentBranch) != branch {
		return fmt.Errorf("worktree %q is on branch %q, expected %q", wtPath, strings.TrimSpace(currentBranch), branch)
	}
	if expectedHead != "" {
		head, err := resolveCommit(wtPath, "HEAD")
		if err != nil {
			return err
		}
		if head != expectedHead {
			return fmt.Errorf("worktree %q is at %s, expected checkpoint %s", wtPath, head, expectedHead)
		}
	}
	return nil
}

// verifyUncheckpointedWorktree permits only coordinator-generated instruction
// pointers before the first checkpoint. All ordinary candidate changes require
// an explicit checkpoint; a fresh claim cannot adopt a prior worker's files.
func verifyUncheckpointedWorktree(wtPath, base string) error {
	status, err := gitOutput(wtPath, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	reject := func() error {
		return fmt.Errorf("worktree %q contains uncommitted changes without a recorded checkpoint; to keep them as this task's implementation, run swarm issue stage <issue> --phase implement while your claim is active, otherwise move them aside before retrying", wtPath)
	}
	for _, entry := range strings.Split(status, "\x00") {
		if entry == "" {
			continue
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return reject()
		}
		name := entry[3:]
		if name != "AGENTS.md" && name != "CLAUDE.md" {
			return reject()
		}
		if strings.ContainsAny(entry[:2], "RCDU") {
			return reject()
		}
		current, err := safefs.ReadFile(wtPath, name, 16<<20)
		if err != nil {
			return reject()
		}
		listing, err := gitOutput(wtPath, "ls-tree", "-z", base, "--", name)
		if err != nil {
			return err
		}
		expected := ""
		expectedExecutable := false
		if listing != "" {
			fields := strings.Fields(strings.SplitN(listing, "\t", 2)[0])
			if len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") {
				return reject()
			}
			expectedExecutable = fields[0] == "100755"
			expected, err = gitOutput(wtPath, "show", base+":"+name)
			if err != nil {
				return err
			}
		}
		// Git may check out Markdown with CRLF on Windows while storing LF blobs.
		// Compare instruction content with consistent line endings; retain the
		// original bytes when removing or refreshing the generated pointer.
		cleaned := activeBlockRegex.ReplaceAllString(string(current), "")
		if strings.ReplaceAll(cleaned, "\r\n", "\n") != strings.ReplaceAll(expected, "\r\n", "\n") {
			return reject()
		}
		// Git ignores executable bits on Windows; on other platforms preserve them.
		if runtime.GOOS != "windows" {
			info, err := os.Lstat(filepath.Join(wtPath, name))
			if err != nil || !info.Mode().IsRegular() || (info.Mode().Perm()&0111 != 0) != expectedExecutable {
				return reject()
			}
		}
	}
	return nil
}

func samePath(a, b string) bool {
	aAbs, errA := filepath.Abs(a)
	bAbs, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(aAbs); err == nil {
		aAbs = resolved
	}
	if resolved, err := filepath.EvalSymlinks(bAbs); err == nil {
		bAbs = resolved
	}
	return filepath.Clean(aAbs) == filepath.Clean(bAbs)
}

func showHarnessFile(worktreePath, baseSHA, path string) (string, error) {
	out, err := gitOutput(worktreePath, "show", baseSHA+":"+path)
	if err != nil {
		return "", fmt.Errorf("load approved harness file %q: %w", path, err)
	}
	return out, nil
}

// CompileHarnessBundleAt builds the active prompt only from base-pinned
// harness instructions so a candidate cannot alter its own execution policy.
func CompileHarnessBundleAt(worktreePath string, wu *contract.WorkUnit, wf *contract.WorkflowSpec, artifacts []contract.Artifact, role, baseSHA string) error {
	if role == "" {
		role = "planner"
	}
	rolePrompt, err := harness.LoadRolePromptAt(worktreePath, role, baseSHA)
	if err != nil {
		return err
	}
	projectDoc, err := showHarnessFile(worktreePath, baseSHA, ".swarm/project.md")
	if err != nil {
		return err
	}
	archDoc, err := showHarnessFile(worktreePath, baseSHA, ".swarm/architecture.md")
	if err != nil {
		return err
	}
	convDoc, err := showHarnessFile(worktreePath, baseSHA, ".swarm/conventions.md")
	if err != nil {
		return err
	}
	validationDoc, err := showHarnessFile(worktreePath, baseSHA, ".swarm/validation/required-checks.yaml")
	if err != nil {
		return err
	}
	var extraDocs []string
	for _, optPath := range []string{".swarm/goals.md", ".swarm/SKILLS.md"} {
		if content, optErr := showHarnessFile(worktreePath, baseSHA, optPath); optErr == nil && strings.TrimSpace(content) != "" {
			extraDocs = append(extraDocs, strings.TrimSpace(content))
		}
	}
	for _, dirPath := range []string{".swarm/rules", ".swarm/skills"} {
		if treeOut, treeErr := gitOutput(worktreePath, "ls-tree", "-r", "--name-only", baseSHA, dirPath); treeErr == nil {
			var files []string
			for _, line := range strings.Split(strings.TrimSpace(treeOut), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasSuffix(strings.ToLower(line), ".md") {
					files = append(files, line)
				}
			}
			sort.Strings(files)
			for _, f := range files {
				if content, fErr := showHarnessFile(worktreePath, baseSHA, f); fErr == nil && strings.TrimSpace(content) != "" {
					extraDocs = append(extraDocs, strings.TrimSpace(content))
				}
			}
		}
	}
	return compileHarnessBundle(worktreePath, wu, wf, artifacts, role, rolePrompt, projectDoc, archDoc, convDoc, validationDoc, extraDocs...)
}

func compileHarnessBundle(worktreePath string, wu *contract.WorkUnit, wf *contract.WorkflowSpec, artifacts []contract.Artifact, role, rolePrompt, projectDoc, archDoc, convDoc, validationDoc string, extraDocs ...string) error {
	safe, err := safefs.Open(worktreePath)
	if err != nil {
		return err
	}
	defer safe.Close()
	active, err := safe.Sub(filepath.Join(".swarm", "active"), true, 0755)
	if err != nil {
		return err
	}
	defer active.Close()
	if rawJSON, err := json.MarshalIndent(wu, "", "  "); err == nil {
		if err := active.WriteFile("workunit.json", rawJSON, 0o644); err != nil {
			return err
		}
	}
	workunitMD := renderWorkUnitMarkdown(wu, wf, artifacts)
	if err := active.WriteFile("WORKUNIT.md", []byte(workunitMD), 0o644); err != nil {
		return err
	}

	var sys strings.Builder
	sys.WriteString(strings.TrimSpace(rolePrompt) + "\n\n")
	if projectDoc != "" {
		sys.WriteString(strings.TrimSpace(projectDoc) + "\n\n")
	}
	if archDoc != "" {
		sys.WriteString(strings.TrimSpace(archDoc) + "\n\n")
	}
	if convDoc != "" {
		sys.WriteString(strings.TrimSpace(convDoc) + "\n\n")
	}
	for _, doc := range extraDocs {
		if strings.TrimSpace(doc) != "" {
			sys.WriteString(strings.TrimSpace(doc) + "\n\n")
		}
	}
	sys.WriteString("## Approved validation policy\n\n```yaml\n")
	sys.WriteString(strings.TrimSpace(validationDoc))
	sys.WriteString("\n```\n\nRun these check commands directly. Claim-bound validation and publication belong to the coordinator; agent subprocesses must not request coordinator credentials.\n\n")
	sys.WriteString("---\n\n")
	sys.WriteString(workunitMD)

	if err := active.WriteFile("SYSTEM_PROMPT.md", []byte(sys.String()), 0o644); err != nil {
		return err
	}

	pointerBlock := fmt.Sprintf(`%s
## Active SwarmOS Work Unit: %s (#%d)

- **Active Role**: `+"`%s`"+`
- **Work Unit Contract**: `+"`.swarm/active/WORKUNIT.md`"+`
- **System & Role Instructions**: `+"`.swarm/active/SYSTEM_PROMPT.md`"+`
- **Plan Artifact Path**: `+"`.swarm/active/PLAN.md`"+`
- **Test Plan Artifact Path**: `+"`.swarm/active/TEST_PLAN.md`"+`
- **Review Artifact Path**: `+"`.swarm/active/REVIEW.md`"+`
- **Slash Commands**: `+"`/swarm-plan`"+`, `+"`/swarm-review`"+`, `+"`/swarm-implement`"+`, `+"`/swarm-validate`"+`, `+"`/swarm-project-plan`"+`
- **Interactive Staging CLI**: `+"`swarm issue stage %d --phase plan`"+` -> `+"`swarm issue review-plan %d`"+` -> `+"`swarm issue stage %d --phase implement`"+` -> `+"`swarm issue submit %d`"+`

Always read `+"`.swarm/active/WORKUNIT.md`"+` and `+"`.swarm/active/SYSTEM_PROMPT.md`"+` before making changes.
%s`, ActiveMarkerStart, wu.ID, wu.IssueNumber, role, wu.IssueNumber, wu.IssueNumber, wu.IssueNumber, wu.IssueNumber, ActiveMarkerEnd)

	if err := injectActivePointer(worktreePath, pointerBlock); err != nil {
		return err
	}
	return configureWorktreeExclude(worktreePath)
}

// injectActivePointer writes the ephemeral active-task block into the agent
// instruction files; CleanEphemeralHarnessMarkers removes it again.
func injectActivePointer(worktreePath, block string) error {
	for _, docName := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := injectManagedBlock(filepath.Join(worktreePath, docName), block); err != nil {
			return err
		}
	}
	return nil
}

func LoadActiveWorkUnit(worktreePath string) (*contract.WorkUnit, error) {
	data, err := safefs.ReadFile(worktreePath, filepath.Join(".swarm", "active", "workunit.json"), 1<<20)
	if err != nil {
		return nil, err
	}
	var wu contract.WorkUnit
	if err := json.Unmarshal(data, &wu); err != nil {
		return nil, err
	}
	return &wu, nil
}

// CheckCommitIdentity confirms git can author checkpoint commits in repoRoot,
// using the same environment checkpoints use, before any agent work starts.
func CheckCommitIdentity(ctx context.Context, repoRoot string) error {
	if _, err := gitOutputContext(ctx, repoRoot, "var", "GIT_COMMITTER_IDENT"); err != nil {
		return fmt.Errorf("git has no commit identity for checkpoints; set it with git config user.name and git config user.email (globally or in this repository)")
	}
	return nil
}

// StripActivePointer removes SwarmOS's temporary task block from instruction
// file content, leaving the repository's own text.
func StripActivePointer(content string) string {
	return activeBlockRegex.ReplaceAllString(content, "")
}

// CleanEphemeralHarnessMarkers removes the managed <!-- swarm:active:start --> block from AGENTS.md and CLAUDE.md
// so worktree commits remain clean on `swarm issue submit`.
func CleanEphemeralHarnessMarkers(worktreePath string) error {
	safe, err := safefs.Open(worktreePath)
	if err != nil {
		return err
	}
	defer safe.Close()
	for _, docName := range []string{"AGENTS.md", "CLAUDE.md"} {
		full := filepath.Join(worktreePath, docName)
		info, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refuse to rewrite non-regular instruction file %s", docName)
		}
		data, err := safe.ReadFile(docName, 16<<20)
		if err != nil {
			return err
		}
		if !activeBlockRegex.Match(data) {
			continue
		}
		cleaned := activeBlockRegex.ReplaceAllString(string(data), "")
		if cleaned == "" {
			if _, err := gitOutput(worktreePath, "ls-files", "--error-unmatch", docName); err != nil {
				if err := safe.Remove(docName); err != nil {
					return err
				}
				continue
			}
		}
		if err := safe.WriteFile(docName, []byte(cleaned), 0o644); err != nil {
			return err
		}
	}

	return nil
}

func renderWorkUnitMarkdown(wu *contract.WorkUnit, wf *contract.WorkflowSpec, artifacts []contract.Artifact) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Work Unit %s: %s\n\n", wu.ID, wu.Title))
	sb.WriteString(fmt.Sprintf("- **Issue Number**: #%d\n", wu.IssueNumber))
	sb.WriteString(fmt.Sprintf("- **Workflow**: `%s`\n", wu.Workflow))
	sb.WriteString(fmt.Sprintf("- **Plan Review Gate**: `%s`\n", wu.Gate))
	sb.WriteString(fmt.Sprintf("- **Allow Harness Changes**: `%v`\n\n", wu.AllowHarnessChanges))

	sb.WriteString("## Objective\n\n")
	sb.WriteString(strings.TrimSpace(wu.Objective) + "\n\n")

	sb.WriteString("## Constraints\n\n")
	for _, c := range wu.Constraints {
		sb.WriteString("- " + c + "\n")
	}
	sb.WriteString("\n")

	sb.WriteString("## Acceptance Criteria\n\n")
	for _, a := range wu.Acceptance {
		sb.WriteString("- [ ] " + a + "\n")
	}
	sb.WriteString("\n")

	if wf != nil && len(wf.Steps) > 0 {
		sb.WriteString("## Workflow Steps\n\n")
		for idx, st := range wf.Steps {
			artNote := ""
			if st.Artifact != "" {
				artNote = fmt.Sprintf(" -> produces `%s` artifact", st.Artifact)
			}
			sb.WriteString(fmt.Sprintf("%d. **%s** (role: `%s`)%s\n", idx+1, st.Name, st.Role, artNote))
		}
		sb.WriteString("\n")
	}

	if len(artifacts) > 0 {
		sb.WriteString("## Prior Artifacts\n\n")
		for _, a := range artifacts {
			sb.WriteString(fmt.Sprintf("### %s (by `%s`, verdict: `%s`)\n\n%s\n\n", a.Type, a.Producer, a.Verdict, strings.TrimSpace(a.Body)))
		}
	}

	return sb.String()
}

func injectManagedBlock(filePath, block string) error {
	safe, err := safefs.Open(filepath.Dir(filePath))
	if err != nil {
		return err
	}
	defer safe.Close()
	name := filepath.Base(filePath)
	if info, err := os.Lstat(filePath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular instruction file %s", filePath)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	existing, err := safe.ReadFile(name, 16<<20)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return safe.WriteFile(name, []byte(block+"\n\n"), 0o644)
	}
	content := string(existing)
	if activeBlockRegex.MatchString(content) {
		updated := activeBlockRegex.ReplaceAllStringFunc(content, func(string) string { return block + "\n\n" })
		return safe.WriteFile(name, []byte(updated), 0o644)
	}
	updated := block + "\n\n" + content
	return safe.WriteFile(name, []byte(updated), 0o644)
}

func configureWorktreeExclude(wtPath string) error {
	out, err := gitOutput(wtPath, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return nil
	}
	excludePath := strings.TrimSpace(string(out))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(wtPath, excludePath)
	}
	// Git owns the metadata directory; reject links below it before updating
	// its exclude file. Task workers in isolated mode never receive this root.
	if filepath.Base(excludePath) != "exclude" || filepath.Base(filepath.Dir(excludePath)) != "info" {
		return fmt.Errorf("unexpected Git exclude path %q", excludePath)
	}
	metadata, err := safefs.Open(filepath.Dir(filepath.Dir(excludePath)))
	if err != nil {
		return err
	}
	defer metadata.Close()
	existing, err := metadata.ReadFile(filepath.Join("info", "exclude"), 16<<20)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	content := string(existing)
	entries := []string{".swarm/active/", ".swarm/worktrees/", ".swarm/local-tracker/"}
	var sb strings.Builder
	sb.WriteString(content)
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		sb.WriteString("\n")
	}
	changed := false
	for _, e := range entries {
		if !strings.Contains(content, e) {
			sb.WriteString(e + "\n")
			changed = true
		}
	}
	if changed {
		return metadata.WriteFile(filepath.Join("info", "exclude"), []byte(sb.String()), 0o644)
	}
	return nil
}
