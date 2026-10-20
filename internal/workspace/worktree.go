package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

const (
	ActiveMarkerStart = "<!-- swarm:active:start -->"
	ActiveMarkerEnd   = "<!-- swarm:active:end -->"
)

var (
	nonSlugRegex     = regexp.MustCompile(`[^a-z0-9]+`)
	activeBlockRegex = regexp.MustCompile(`(?s)<!-- swarm:active:start -->.*?<!-- swarm:active:end -->\n{0,2}`)
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
	baseSHA, err = resolveCommit(absRoot, baseSHA)
	if err != nil {
		return "", "", fmt.Errorf("resolve approved base %q: %w", baseSHA, err)
	}
	if headSHA != "" {
		if _, err := gitOutput(absRoot, "remote", "get-url", "origin"); err == nil {
			remote, err := gitOutput(absRoot, "ls-remote", "--refs", "origin", "refs/heads/"+branch)
			if err != nil {
				return "", "", err
			}
			fields := strings.Fields(remote)
			if len(fields) != 2 || fields[0] != headSHA {
				return "", "", fmt.Errorf("remote branch %q does not match approved checkpoint %s", branch, headSHA)
			}
			if _, err := gitOutput(absRoot, "fetch", "--no-write-fetch-head", "origin", headSHA); err != nil {
				return "", "", fmt.Errorf("fetch pinned checkpoint: %w", err)
			}
		}
	}
	checkpoint := headSHA
	if checkpoint == "" {
		checkpoint, err = resolveCommit(absRoot, "refs/heads/"+branch)
		if err != nil {
			checkpoint = baseSHA
		}
	}
	if _, err := gitOutput(absRoot, "merge-base", "--is-ancestor", baseSHA, checkpoint); err != nil {
		return "", "", fmt.Errorf("checkpoint %s does not descend from approved base %s; reconcile the branch without resetting it: %w", checkpoint, baseSHA, err)
	}
	wtPath := WorktreePath(absRoot, issueNum)
	if _, err := os.Lstat(wtPath); err == nil {
		if err := verifyWorktree(wtPath, branch, headSHA); err != nil {
			return "", "", err
		}
		if err := configureWorktreeExclude(wtPath); err != nil {
			return "", "", err
		}
		return wtPath, branch, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return "", "", err
	}
	_ = exec.Command("git", "-C", absRoot, "worktree", "prune").Run()

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
	if _, err := gitOutput(absRoot, "worktree", "add", wtPath, branch); err != nil {
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

// Checkpoint commits the worktree's current changes without rewriting history.
// Publication belongs to the fenced coordination store, never this helper.
func Checkpoint(ctx context.Context, wtPath, branch string) (string, error) {
	if branch == "" {
		return "", errors.New("checkpoint branch is required")
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
		if _, err := gitOutputContext(ctx, wtPath, "commit", "-m", "swarm checkpoint"); err != nil {
			return "", fmt.Errorf("commit checkpoint: %w", err)
		}
	}
	head, err := resolveCommit(wtPath, "HEAD")
	if err != nil {
		return "", err
	}
	return head, nil
}

var errGitFailed = errors.New("git command failed")

func gitOutput(repoRoot string, args ...string) (string, error) {
	return gitOutputContext(context.Background(), repoRoot, args...)
}

func gitOutputContext(ctx context.Context, repoRoot string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: git %s: %s", errGitFailed, strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return string(out), nil
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
	if err != nil || gitEntry.IsDir() {
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
	return compileHarnessBundle(worktreePath, wu, wf, artifacts, role, rolePrompt, projectDoc, archDoc, convDoc, validationDoc)
}

func compileHarnessBundle(worktreePath string, wu *contract.WorkUnit, wf *contract.WorkflowSpec, artifacts []contract.Artifact, role, rolePrompt, projectDoc, archDoc, convDoc, validationDoc string) error {
	activeDir := filepath.Join(worktreePath, ".swarm", "active")
	if err := os.MkdirAll(activeDir, 0o755); err != nil {
		return err
	}
	if rawJSON, err := json.MarshalIndent(wu, "", "  "); err == nil {
		if err := os.WriteFile(filepath.Join(activeDir, "workunit.json"), rawJSON, 0o644); err != nil {
			return err
		}
	}
	workunitMD := renderWorkUnitMarkdown(wu, wf, artifacts)
	if err := os.WriteFile(filepath.Join(activeDir, "WORKUNIT.md"), []byte(workunitMD), 0o644); err != nil {
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
	sys.WriteString("## Approved validation policy\n\n```yaml\n")
	sys.WriteString(strings.TrimSpace(validationDoc))
	sys.WriteString("\n```\n\nRun these check commands directly. Claim-bound validation and publication belong to the coordinator; agent subprocesses must not request coordinator credentials.\n\n")
	sys.WriteString("---\n\n")
	sys.WriteString(workunitMD)

	if err := os.WriteFile(filepath.Join(activeDir, "SYSTEM_PROMPT.md"), []byte(sys.String()), 0o644); err != nil {
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
- **Slash Commands**: `+"`/swarm-plan`"+`, `+"`/swarm-review`"+`, `+"`/swarm-implement`"+`, `+"`/swarm-validate`"+`

Always read `+"`.swarm/active/WORKUNIT.md`"+` and `+"`.swarm/active/SYSTEM_PROMPT.md`"+` before making changes.
%s`, ActiveMarkerStart, wu.ID, wu.IssueNumber, role, ActiveMarkerEnd)

	for _, docName := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := injectManagedBlock(filepath.Join(worktreePath, docName), pointerBlock); err != nil {
			return err
		}
	}

	return configureWorktreeExclude(worktreePath)
}

func LoadActiveWorkUnit(worktreePath string) (*contract.WorkUnit, error) {
	data, err := os.ReadFile(filepath.Join(worktreePath, ".swarm", "active", "workunit.json"))
	if err != nil {
		return nil, err
	}
	var wu contract.WorkUnit
	if err := json.Unmarshal(data, &wu); err != nil {
		return nil, err
	}
	return &wu, nil
}

// CleanEphemeralHarnessMarkers removes the managed <!-- swarm:active:start --> block from AGENTS.md and CLAUDE.md
// so worktree commits remain clean on `swarm submit`.
func CleanEphemeralHarnessMarkers(worktreePath string) error {
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
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		if !activeBlockRegex.Match(data) {
			continue
		}
		cleaned := activeBlockRegex.ReplaceAllString(string(data), "")
		if cleaned == "" {
			if err := exec.Command("git", "-C", worktreePath, "ls-files", "--error-unmatch", docName).Run(); err != nil {
				if err := os.Remove(full); err != nil {
					return err
				}
				continue
			}
		}
		if err := os.WriteFile(full, []byte(cleaned), 0o644); err != nil {
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
	if info, err := os.Lstat(filePath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular instruction file %s", filePath)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	existing, err := os.ReadFile(filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return os.WriteFile(filePath, []byte(block+"\n\n"), 0o644)
	}
	content := string(existing)
	if activeBlockRegex.MatchString(content) {
		updated := activeBlockRegex.ReplaceAllStringFunc(content, func(string) string { return block + "\n\n" })
		return os.WriteFile(filePath, []byte(updated), 0o644)
	}
	updated := block + "\n\n" + content
	return os.WriteFile(filePath, []byte(updated), 0o644)
}

func configureWorktreeExclude(wtPath string) error {
	out, err := exec.Command("git", "-C", wtPath, "rev-parse", "--git-path", "info/exclude").CombinedOutput()
	if err != nil {
		return nil
	}
	excludePath := strings.TrimSpace(string(out))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(wtPath, excludePath)
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return nil
	}
	existing, _ := os.ReadFile(excludePath)
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
		return os.WriteFile(excludePath, []byte(sb.String()), 0o644)
	}
	return nil
}
