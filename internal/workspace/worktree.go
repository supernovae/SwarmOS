package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
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
	activeBlockRegex = regexp.MustCompile(`(?s)\n*<!-- swarm:active:start -->.*?<!-- swarm:active:end -->\n*`)
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

// Provision creates or reuses an isolated git worktree at .swarm/worktrees/issue-<N> on branch swarm/issue-<N>-<slug>.
func Provision(repoRoot string, issueNum int, slug string) (string, string, error) {
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", "", err
	}

	if err := ensureInitialCommit(absRoot); err != nil {
		return "", "", err
	}

	branch := BranchName(issueNum, slug)
	wtPath := WorktreePath(absRoot, issueNum)

	if info, err := os.Stat(wtPath); err == nil && info.IsDir() {
		_ = syncSwarmConfigToWorktree(absRoot, wtPath)
		_ = configureWorktreeExclude(wtPath)
		return wtPath, branch, nil
	}

	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return "", "", err
	}

	_ = exec.Command("git", "-C", absRoot, "worktree", "prune").Run()

	cmd := exec.Command("git", "-C", absRoot, "worktree", "add", "-B", branch, wtPath, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("git worktree add failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	if err := syncSwarmConfigToWorktree(absRoot, wtPath); err != nil {
		return "", "", err
	}
	if err := configureWorktreeExclude(wtPath); err != nil {
		return "", "", err
	}

	return wtPath, branch, nil
}

// CompileHarnessBundle populates .swarm/active/{WORKUNIT.md,SYSTEM_PROMPT.md,workunit.json} and injects universal context blocks into AGENTS.md and CLAUDE.md.
func CompileHarnessBundle(worktreePath string, wu *contract.WorkUnit, wf *contract.WorkflowSpec, artifacts []contract.Artifact, role string) error {
	if role == "" {
		role = "planner"
	}
	activeDir := filepath.Join(worktreePath, ".swarm", "active")
	if err := os.MkdirAll(activeDir, 0o755); err != nil {
		return err
	}

	if rawJSON, err := json.MarshalIndent(wu, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(activeDir, "workunit.json"), rawJSON, 0o644)
	}

	workunitMD := renderWorkUnitMarkdown(wu, wf, artifacts)
	if err := os.WriteFile(filepath.Join(activeDir, "WORKUNIT.md"), []byte(workunitMD), 0o644); err != nil {
		return err
	}

	rolePrompt := harness.LoadRolePrompt(worktreePath, role)
	projectDoc := readOptionalFile(filepath.Join(worktreePath, ".swarm", "project.md"))
	archDoc := readOptionalFile(filepath.Join(worktreePath, ".swarm", "architecture.md"))
	convDoc := readOptionalFile(filepath.Join(worktreePath, ".swarm", "conventions.md"))

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

	if err := harness.EnsureSlashCommands(worktreePath, false, nil); err != nil {
		return err
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
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		cleaned := strings.TrimSpace(activeBlockRegex.ReplaceAllString(string(data), "\n"))
		if cleaned == "" {
			if err := exec.Command("git", "-C", worktreePath, "ls-files", "--error-unmatch", docName).Run(); err != nil {
				_ = os.Remove(full)
				continue
			}
		}
		_ = os.WriteFile(full, []byte(cleaned+"\n"), 0o644)
	}

	manifestPath := filepath.Join(worktreePath, ".swarm", "active", "synced-untracked.json")
	if raw, err := os.ReadFile(manifestPath); err == nil {
		var synced map[string]string
		if json.Unmarshal(raw, &synced) == nil {
			for rel, origHash := range synced {
				full := filepath.Join(worktreePath, rel)
				if curData, err := os.ReadFile(full); err == nil {
					sum := sha256.Sum256(curData)
					if hex.EncodeToString(sum[:]) == origHash {
						_ = os.Remove(full)
					}
				}
			}
		}
	}
	for _, cmdDir := range []string{".claude/commands", ".opencode/command"} {
		if err := exec.Command("git", "-C", worktreePath, "ls-files", "--error-unmatch", cmdDir).Run(); err != nil {
			_ = os.RemoveAll(filepath.Join(worktreePath, filepath.Dir(cmdDir)))
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
	existing, err := os.ReadFile(filePath)
	if err != nil {
		return os.WriteFile(filePath, []byte(block+"\n"), 0o644)
	}
	content := string(existing)
	if strings.Contains(content, ActiveMarkerStart) && strings.Contains(content, ActiveMarkerEnd) {
		updated := activeBlockRegex.ReplaceAllString(content, "\n\n"+block+"\n")
		return os.WriteFile(filePath, []byte(strings.TrimSpace(updated)+"\n"), 0o644)
	}
	updated := strings.TrimSpace(content) + "\n\n" + block + "\n"
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

func syncSwarmConfigToWorktree(repoRoot, wtPath string) error {
	srcSwarm := filepath.Join(repoRoot, ".swarm")
	if _, err := os.Stat(srcSwarm); err != nil {
		return nil
	}
	manifestPath := filepath.Join(wtPath, ".swarm", "active", "synced-untracked.json")
	synced := make(map[string]string)
	if raw, err := os.ReadFile(manifestPath); err == nil {
		_ = json.Unmarshal(raw, &synced)
	}

	err := filepath.WalkDir(srcSwarm, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(srcSwarm, path)
		if err != nil {
			return nil
		}
		if strings.HasPrefix(rel, "worktrees") || strings.HasPrefix(rel, "active") || strings.HasPrefix(rel, "local-tracker") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dst := filepath.Join(wtPath, ".swarm", rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if _, statErr := os.Stat(dst); statErr == nil {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		repoRel := filepath.ToSlash(filepath.Join(".swarm", rel))
		sum := sha256.Sum256(data)
		synced[repoRel] = hex.EncodeToString(sum[:])
		return nil
	})
	if len(synced) > 0 {
		_ = os.MkdirAll(filepath.Dir(manifestPath), 0o755)
		if out, mErr := json.MarshalIndent(synced, "", "  "); mErr == nil {
			_ = os.WriteFile(manifestPath, out, 0o644)
		}
	}
	return err
}

func ensureInitialCommit(repoRoot string) error {
	if err := exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "HEAD").Run(); err == nil {
		return nil
	}
	_ = exec.Command("git", "-C", repoRoot, "config", "user.name", "SwarmOS").Run()
	_ = exec.Command("git", "-C", repoRoot, "config", "user.email", "swarm@localhost").Run()
	cmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "chore: initial repository commit")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create initial commit for worktree: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func readOptionalFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}
