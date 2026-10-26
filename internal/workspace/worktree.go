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

// DefaultUpstreamRemote names the project remote when none is selected.
const DefaultUpstreamRemote = "origin"

var remoteNameRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateRemoteName rejects names that Git could read as an option, a URL,
// or a path before one is used as a command argument or ref component.
func ValidateRemoteName(name string) error {
	if !remoteNameRegex.MatchString(name) || strings.Contains(name, "..") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("invalid git remote name %q", name)
	}
	return nil
}

// UpstreamRemoteOrDefault returns name, or origin when it is empty.
func UpstreamRemoteOrDefault(name string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return DefaultUpstreamRemote
}

// ClaimCheckpoint returns where a claim's work is checkpointed: the recorded
// branch on origin (or the issue branch), or in fork mode a claim-scoped branch
// on the contributor's fork remote.
func ClaimCheckpoint(forkMode bool, forkRemote string, issueNum int, title, claimID, recorded string) (remote, branch string) {
	return ClaimCheckpointFrom(DefaultUpstreamRemote, forkMode, forkRemote, issueNum, title, claimID, recorded)
}

// ClaimCheckpointFrom is ClaimCheckpoint with same-repository checkpoints on
// the named upstream remote.
func ClaimCheckpointFrom(upstreamRemote string, forkMode bool, forkRemote string, issueNum int, title, claimID, recorded string) (remote, branch string) {
	remote, branch = UpstreamRemoteOrDefault(upstreamRemote), recorded
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
	return ResolveForkRemoteSlug(repoRoot, DefaultUpstreamRemote, remoteName)
}

// ResolveForkRemoteSlug resolves a git remote's owner/repo slug. Any remote
// other than upstreamRemote must not point to the same URL as the upstream.
func ResolveForkRemoteSlug(repoRoot, upstreamRemote, remoteName string) (string, error) {
	remoteName = strings.TrimSpace(remoteName)
	upstreamRemote = UpstreamRemoteOrDefault(upstreamRemote)
	if remoteName == "" {
		return "", fmt.Errorf("remote name is required")
	}
	if err := ValidateRemoteName(remoteName); err != nil {
		return "", err
	}
	rawURL, err := gitOutput(repoRoot, "remote", "get-url", remoteName)
	if err != nil {
		return "", fmt.Errorf("git remote %q is not configured: %w", remoteName, err)
	}
	rawURL = strings.TrimSpace(rawURL)
	if remoteName != upstreamRemote {
		if upstreamURL, upErr := gitOutput(repoRoot, "remote", "get-url", upstreamRemote); upErr == nil {
			if strings.TrimSpace(upstreamURL) == rawURL {
				return "", fmt.Errorf("fork remote %q must not point to the same URL as upstream %s", remoteName, upstreamRemote)
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
	u := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if u == "" {
		return "", fmt.Errorf("empty git remote URL")
	}
	if strings.Contains(u, "://") {
		parsed, err := url.Parse(u)
		if err != nil {
			return "", fmt.Errorf("parse remote URL %q: %w", rawURL, err)
		}
		path := strings.Trim(strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), ".git"), "/")
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
	return PushForkCheckpointFrom(ctx, repoRoot, DefaultUpstreamRemote, forkRemote, branch, headSHA)
}

// PushForkCheckpointFrom is PushForkCheckpoint that never pushes to the named
// upstream remote.
func PushForkCheckpointFrom(ctx context.Context, repoRoot, upstreamRemote, forkRemote, branch, headSHA string) error {
	forkRemote = strings.TrimSpace(forkRemote)
	upstreamRemote = UpstreamRemoteOrDefault(upstreamRemote)
	if forkRemote == "" || strings.EqualFold(forkRemote, upstreamRemote) {
		if upstreamRemote == DefaultUpstreamRemote {
			return fmt.Errorf("fork checkpoint push requires a non-origin fork remote (got %q)", forkRemote)
		}
		return fmt.Errorf("fork checkpoint push requires a fork remote other than upstream %s (got %q)", upstreamRemote, forkRemote)
	}
	if err := ValidateRemoteName(forkRemote); err != nil {
		return err
	}
	if !strings.HasPrefix(branch, "swarm/issue-") || strings.Contains(branch, "..") || strings.TrimSpace(headSHA) == "" {
		return fmt.Errorf("invalid fork checkpoint branch %q or head SHA %q", branch, headSHA)
	}
	if err := RejectRecoveryOutput(ctx, repoRoot, headSHA); err != nil {
		return err
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
	return ResolveBaseFrom(repoRoot, DefaultUpstreamRemote)
}

// ResolveBaseFrom is ResolveBase with the named upstream remote's default
// branch as the authority. Only a missing origin falls back to local HEAD; a
// missing named upstream remote is an error.
func ResolveBaseFrom(repoRoot, upstreamRemote string) (string, error) {
	upstreamRemote = UpstreamRemoteOrDefault(upstreamRemote)
	if err := ValidateRemoteName(upstreamRemote); err != nil {
		return "", err
	}
	if _, err := gitOutput(repoRoot, "rev-parse", "--show-toplevel"); err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	if _, err := gitOutput(repoRoot, "remote", "get-url", upstreamRemote); err != nil {
		if !errors.Is(err, errGitFailed) {
			return "", err
		}
		if upstreamRemote != DefaultUpstreamRemote {
			return "", MissingUpstreamRemoteError(upstreamRemote)
		}
		return resolveCommit(repoRoot, "HEAD")
	}
	if _, err := gitOutput(repoRoot, "fetch", "--no-write-fetch-head", "--prune", upstreamRemote); err != nil {
		return "", fmt.Errorf("fetch %s before resolving base: %w", upstreamRemote, err)
	}
	if _, err := gitOutput(repoRoot, "remote", "set-head", upstreamRemote, "--auto"); err != nil {
		return "", fmt.Errorf("discover advertised %s default branch: %w", upstreamRemote, err)
	}
	out, err := gitOutput(repoRoot, "symbolic-ref", "--quiet", "refs/remotes/"+upstreamRemote+"/HEAD")
	if err != nil {
		return "", fmt.Errorf("%s has no advertised default branch: %w", upstreamRemote, err)
	}
	return resolveCommit(repoRoot, strings.TrimSpace(out))
}

// MissingUpstreamRemoteError names a selected upstream remote that is not
// configured and how to fix it.
func MissingUpstreamRemoteError(upstreamRemote string) error {
	return fmt.Errorf("upstream remote %q is not configured; add it with git remote add %s <project-url>, or pass --upstream-remote <project-remote>", upstreamRemote, upstreamRemote)
}

// ProvisionAt creates or reopens exactly the requested worktree branch.
// It never resets an existing branch and refuses an unrelated directory.
func ProvisionAt(repoRoot string, issueNum int, title, baseSHA, branch, headSHA string) (string, string, error) {
	return ProvisionFromRemoteAt(repoRoot, DefaultUpstreamRemote, issueNum, title, baseSHA, branch, headSHA)
}

// ProvisionFromRemoteAt creates or reopens the requested worktree branch,
// fetching baseSHA from origin if needed and restoring any pinned checkpoint
// headSHA from checkpointRemote ("origin" in same-repo mode, or the verified
// fork remote in --fork mode).
// Callers that know a previous approved base use ProvisionWithContext.
func ProvisionFromRemoteAt(repoRoot, checkpointRemote string, issueNum int, title, baseSHA, branch, headSHA string, recovery ...*DroppedCheckpoint) (string, string, error) {
	var dropped *DroppedCheckpoint
	if len(recovery) > 0 {
		dropped = recovery[0]
	}
	return provisionFromRemote(repoRoot, DefaultUpstreamRemote, checkpointRemote, issueNum, title, baseSHA, branch, headSHA, dropped)
}

// provisionFromRemote fetches the approved base from upstreamRemote; only a
// checkpoint on that same remote is a same-repository checkpoint.
func provisionFromRemote(repoRoot, upstreamRemote, checkpointRemote string, issueNum int, title, baseSHA, branch, headSHA string, dropped *DroppedCheckpoint) (string, string, error) {
	upstreamRemote = UpstreamRemoteOrDefault(upstreamRemote)
	if err := ValidateRemoteName(upstreamRemote); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(checkpointRemote) == "" {
		checkpointRemote = upstreamRemote
	}
	if err := ValidateRemoteName(checkpointRemote); err != nil {
		return "", "", err
	}
	if dropped != nil {
		if checkpointRemote != upstreamRemote || headSHA != "" {
			return "", "", fmt.Errorf("invalid checkpoint recovery context")
		}
		return provisionDroppedCheckpoint(repoRoot, upstreamRemote, issueNum, title, baseSHA, dropped)
	}
	if issueNum <= 0 || baseSHA == "" {
		return "", "", errors.New("issue number and approved base SHA are required")
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
		if fetchErr := FetchCommit(absRoot, upstreamRemote, baseSHA); fetchErr == nil {
			resolvedBase, err = resolveCommit(absRoot, baseSHA)
		}
		if err != nil {
			return "", "", fmt.Errorf("resolve approved base %q: %w", baseSHA, err)
		}
	}
	baseSHA = resolvedBase
	if headSHA != "" {
		observation := InspectCheckpoint(context.Background(), absRoot, checkpointRemote, issueNum, branch, headSHA)
		remoteConfigured := false
		if _, err := gitOutput(absRoot, "remote", "get-url", checkpointRemote); err == nil {
			remoteConfigured = true
		}
		if (remoteConfigured && observation.RemoteBranch.SHA != headSHA) ||
			(!observation.LocalBranch.Missing && observation.LocalBranch.SHA != headSHA) ||
			(!observation.Worktree.Missing && (observation.Worktree.SHA != headSHA || observation.AttachedBranch != branch)) {
			return "", "", &CheckpointDivergenceError{Issue: issueNum, State: observation}
		}
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
			if headSHA == "" {
				return "", "", fmt.Errorf("%w; %s", err, RecoveryHint(issueNum, wtPath, branch))
			}
			return "", "", err
		}
		if headSHA == "" {
			if err := verifyUncheckpointedWorktree(wtPath, checkpoint); err != nil {
				return "", "", fmt.Errorf("%w; %s", err, RecoveryHint(issueNum, wtPath, branch))
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
		return "", "", fmt.Errorf("existing branch %q is at %s, expected approved checkpoint %s; preserve it under another branch with git branch -m %s %s before retrying", branch, existing, checkpoint, shellQuote(branch), shellQuote(fmt.Sprintf("swarm-backup/issue-%d", issueNum)))
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
	if err := RejectRecoveryOutput(ctx, wtPath, "HEAD"); err != nil {
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
		if err := stageCheckpointFiles(ctx, wtPath); err != nil {
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
	pointerPaths := instructionPaths(wtPath)
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
		if !pointerPaths[name] {
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
	// Complete evidence is a report attachment, never system-prompt guidance.
	var evidence strings.Builder
	for _, a := range artifacts {
		fmt.Fprintf(&evidence, "## %s (%s)\n\n%s\n\n", a.Type, a.Verdict, a.Body)
	}
	if lessons, readErr := active.ReadFile("LESSONS.md", 1<<20); readErr == nil {
		evidence.WriteString("## Active lessons\n\n" + string(lessons))
	}
	if err := active.WriteFile("REVIEW_EVIDENCE.md", []byte(evidence.String()), 0644); err != nil {
		return err
	}
	workunitMD := renderWorkUnitMarkdown(wu, wf, artifacts, role)
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
	if role == "planner" {
		for i := len(artifacts) - 1; i >= 0; i-- {
			a := artifacts[i]
			if a.Type == contract.ArtifactReview {
				if a.Verdict == "CHANGES_REQUESTED" {
					sys.WriteString("Address the previous CHANGES_REQUESTED findings by revising PLAN.md or TEST_PLAN.md before requesting review.\n\n")
				}
				break
			}
		}
	}
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
// instruction files; CleanEphemeralHarnessMarkers removes it again. Names
// linked to one file get a single block.
func injectActivePointer(worktreePath, block string) error {
	safe, err := safefs.Open(worktreePath)
	if err != nil {
		return err
	}
	defer safe.Close()
	targets, err := instructionTargets(safe)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if err := injectInto(safe, target.path, block); err != nil {
			return err
		}
	}
	return nil
}

// instructionTarget is a regular file that AGENTS.md or CLAUDE.md resolves to.
type instructionTarget struct {
	path   string
	exists bool
	// linked targets are kept even when only a pointer remains, so the
	// repository's instruction link never dangles.
	linked bool
}

// instructionTargets resolves both instruction names inside safe, refusing any
// unsafe link before a caller changes either file, and merges shared targets.
func instructionTargets(safe *safefs.Root) ([]instructionTarget, error) {
	resolved, err := safe.ResolveInstructions()
	if err != nil {
		return nil, err
	}
	var targets []instructionTarget
	index := map[string]int{}
	for _, in := range resolved {
		path := filepath.ToSlash(in.Target)
		if i, ok := index[path]; ok {
			targets[i].linked = targets[i].linked || in.Link
			continue
		}
		index[path] = len(targets)
		targets = append(targets, instructionTarget{path: path, exists: in.Exists, linked: in.Link})
	}
	return targets, nil
}

// instructionPaths lists the paths that may carry a generated pointer: the
// instruction names and the files they safely resolve to. A refused link adds
// no target, which leaves the stricter name-only set.
func instructionPaths(root string) map[string]bool {
	paths := map[string]bool{"AGENTS.md": true, "CLAUDE.md": true}
	safe, err := safefs.Open(root)
	if err != nil {
		return paths
	}
	defer safe.Close()
	if targets, err := instructionTargets(safe); err == nil {
		for _, target := range targets {
			paths[target.path] = true
		}
	}
	return paths
}

// readInstruction reads a resolved instruction target and its permissions,
// refusing any link substituted after resolution.
func readInstruction(safe *safefs.Root, name string) ([]byte, os.FileMode, error) {
	f, err := safe.OpenRegular(name)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		return nil, 0, err
	}
	data, err := safe.ReadFile(name, 16<<20)
	return data, info.Mode().Perm(), err
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
// so worktree commits remain clean on `swarm issue submit`. Links stay as they
// are; the block is removed from the file they resolve to.
func CleanEphemeralHarnessMarkers(worktreePath string) error {
	safe, err := safefs.Open(worktreePath)
	if err != nil {
		return err
	}
	defer safe.Close()
	targets, err := instructionTargets(safe)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if !target.exists {
			continue
		}
		data, perm, err := readInstruction(safe, target.path)
		if err != nil {
			return err
		}
		if !activeBlockRegex.Match(data) {
			continue
		}
		cleaned := activeBlockRegex.ReplaceAllString(string(data), "")
		if cleaned == "" && !target.linked {
			if _, err := gitOutput(worktreePath, "ls-files", "--error-unmatch", "--", target.path); err != nil {
				if err := safe.Remove(target.path); err != nil {
					return err
				}
				continue
			}
		}
		if err := safe.WriteFile(target.path, []byte(cleaned), perm); err != nil {
			return err
		}
	}

	return nil
}

func renderWorkUnitMarkdown(wu *contract.WorkUnit, wf *contract.WorkflowSpec, artifacts []contract.Artifact, roles ...string) string {
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
		role := ""
		if len(roles) > 0 {
			role = roles[0]
		}
		sb.WriteString(renderArtifactContext(artifacts, role))
	}

	return sb.String()
}

func injectManagedBlock(filePath, block string) error {
	safe, err := safefs.Open(filepath.Dir(filePath))
	if err != nil {
		return err
	}
	defer safe.Close()
	in, err := safe.ResolveInstruction(filepath.Base(filePath))
	if err != nil {
		return err
	}
	return injectInto(safe, in.Target, block)
}

// injectInto adds or replaces the block in a resolved instruction target,
// creating the file when it is missing.
func injectInto(safe *safefs.Root, name, block string) error {
	existing, perm, err := readInstruction(safe, name)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return safe.WriteFile(name, []byte(block+"\n\n"), 0o644)
	}
	content := string(existing)
	if activeBlockRegex.MatchString(content) {
		updated := activeBlockRegex.ReplaceAllStringFunc(content, func(string) string { return block + "\n\n" })
		return safe.WriteFile(name, []byte(updated), perm)
	}
	updated := block + "\n\n" + content
	return safe.WriteFile(name, []byte(updated), perm)
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
	entries := []string{".swarm/active-output/", ".swarm/active/", ".swarm/worktrees/", ".swarm/local-tracker/"}
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

// RebindBudgetCheckpoint reuses a clean managed worktree after the same fork
// contributor acquires a new claim. Both claim branches retain their history.
func RebindBudgetCheckpoint(ctx context.Context, root string, issue int, oldBranch, newBranch, head string) error {
	path := WorktreePath(root, issue)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("budget resume worktree must be a real directory")
	}
	onNew := verifyWorktree(path, newBranch, head) == nil
	if !onNew {
		if err := verifyWorktree(path, oldBranch, head); err != nil {
			return err
		}
	}
	if err := CleanEphemeralHarnessMarkers(path); err != nil {
		return err
	}
	dirty, err := gitOutputContext(ctx, path, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if strings.TrimSpace(dirty) != "" {
		return fmt.Errorf("budget resume requires a clean checkpoint worktree")
	}
	if onNew {
		return nil
	}
	if _, err := gitOutputContext(ctx, path, "switch", "--no-track", "-c", newBranch, head); err != nil {
		return fmt.Errorf("switch to new claim checkpoint: %w", err)
	}
	return verifyWorktree(path, newBranch, head)
}
