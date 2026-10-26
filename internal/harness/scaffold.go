package harness

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
	"gopkg.in/yaml.v3"
)

//go:embed all:templates
var embeddedTemplates embed.FS

var immutableSwarmPin = regexp.MustCompile(`^(?:v([0-9]+)\.([0-9]+)\.([0-9]+)(?:-[0-9A-Za-z.-]+)?|[0-9a-f]{40})$`)

// ValidateSwarmVersionPin accepts immutable releases supporting the resource CLI,
// or an explicitly selected full commit SHA of a compatible build.
func ValidateSwarmVersionPin(version string) error {
	const invalidPin = "swarm version must be a resource-CLI-compatible immutable release tag (v0.6.0 or newer) or full commit SHA"
	match := immutableSwarmPin.FindStringSubmatch(version)
	if match == nil {
		return fmt.Errorf("%s", invalidPin)
	}
	if match[1] == "" {
		return nil
	}
	var parts [3]uint64
	for i, part := range match[1:4] {
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return fmt.Errorf("%s", invalidPin)
		}
		parts[i] = n
	}
	if parts[0] == 0 && parts[1] < 6 {
		return fmt.Errorf("%s", invalidPin)
	}
	return nil
}

type StackProfile struct {
	Language    string
	BuildCmd    string
	TestCmd     string
	LintCmd     string
	Conventions []string
}

// GoFmtCheckCommand fails when gofmt lists unformatted files or cannot run.
const GoFmtCheckCommand = `unformatted=$(gofmt -l .) || exit 1; if [ -n "$unformatted" ]; then printf '%s\n' "$unformatted"; exit 1; fi`

func DetectStack(repoRoot string) StackProfile {
	switch {
	case fileExists(filepath.Join(repoRoot, "go.mod")):
		return StackProfile{
			Language: "Go",
			BuildCmd: "go build ./...",
			TestCmd:  "go test ./...",
			LintCmd:  "go vet ./...",
			Conventions: []string{
				"Format all code with gofmt.",
				"Keep packages focused with minimal exported surface area.",
				"Table-driven unit tests for edge cases and error paths.",
			},
		}
	case fileExists(filepath.Join(repoRoot, "Cargo.toml")):
		return StackProfile{
			Language: "Rust",
			BuildCmd: "cargo build",
			TestCmd:  "cargo test",
			LintCmd:  "cargo clippy -- -D warnings",
			Conventions: []string{
				"Format with cargo fmt.",
				"Avoid unwrap() in production code paths.",
			},
		}
	case fileExists(filepath.Join(repoRoot, "pyproject.toml")) ||
		fileExists(filepath.Join(repoRoot, "requirements.txt")) ||
		fileExists(filepath.Join(repoRoot, "requirements-dev.txt")):
		return StackProfile{
			Language: "Python",
			BuildCmd: "python3 -m compileall .",
			TestCmd:  "pytest",
			LintCmd:  "ruff check .",
			Conventions: []string{
				"Include type annotations on public functions.",
				"Write isolated pytest fixtures.",
			},
		}
	case fileExists(filepath.Join(repoRoot, "package.json")):
		return StackProfile{
			Language: "TypeScript/JavaScript",
			BuildCmd: "npm run build --if-present",
			TestCmd:  "npm test",
			LintCmd:  "npm run lint --if-present",
			Conventions: []string{
				"Prefer strict TypeScript types over any.",
				"Keep modules deterministic and side-effect free.",
			},
		}
	default:
		return StackProfile{
			Language: "Generic",
			BuildCmd: "printf '%s\\n' 'Configure project-specific required checks before approving work' >&2; exit 1",
			TestCmd:  "printf '%s\\n' 'Configure project-specific required checks before approving work' >&2; exit 1",
			LintCmd:  "printf '%s\\n' 'Configure project-specific required checks before approving work' >&2; exit 1",
			Conventions: []string{
				"Keep changes minimal, atomic, and well-tested.",
			},
		}
	}
}

const (
	setupNodeSHA        = "949feb2413d6458794dcd2491c4babbbce0c15c1" // actions/setup-node v7.1.0
	setupPythonSHA      = "5fda3b95a4ea91299a34e894583c3862153e4b97" // actions/setup-python v7.0.0
	setupRustSHA        = "ecabd13d1c56bd1345c230e542e9144811ad706f" // actions-rust-lang/setup-rust-toolchain v2.0.0
	pythonPytestVersion = "9.1.1"
	pythonRuffVersion   = "0.17.1"
)

func workflowDefaultBranch(repoRoot string) (string, string) {
	return workflowDefaultBranchFrom(repoRoot, "")
}

func workflowDefaultBranchFrom(repoRoot, remote string) (string, string) {
	if remote == "" {
		remote = "origin"
	}
	runGit := func(args ...string) (string, error) {
		out, err := gitcredential.Output(context.Background(), repoRoot, args...)
		return strings.TrimSpace(string(out)), err
	}

	if ref, err := runGit("symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
		if branch, ok := strings.CutPrefix(ref, remote+"/"); ok && branch != "" {
			return branch, ""
		}
	}

	if _, err := runGit("config", "--get", "remote."+remote+".url"); err == nil {
		if refs, err := runGit("ls-remote", "--symref", remote, "HEAD"); err == nil {
			for _, line := range strings.Split(refs, "\n") {
				fields := strings.Fields(line)
				if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
					if branch, ok := strings.CutPrefix(fields[1], "refs/heads/"); ok && branch != "" {
						return branch, ""
					}
				}
			}
		}
		return "", fmt.Sprintf("CI workflow omitted: could not resolve %s's default branch; fetch %s and rerun swarm repo init", remote, remote)
	}

	if branch, err := runGit("symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && branch != "" {
		return branch, ""
	}
	return "", "CI workflow omitted: no default branch is known; create or check out the intended default branch and rerun swarm repo init"
}

func yamlQuotedString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func projectRuntimeSetup(stack StackProfile) string {
	switch stack.Language {
	case "Go":
		return `      - name: Set up consumer Go toolchain
        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7
        with:
          go-version-file: go.mod
          cache: false
`
	case "TypeScript/JavaScript":
		return `      - name: Set up Node.js
        uses: actions/setup-node@` + setupNodeSHA + ` # v7.1.0
        with:
          node-version: '24.22.0'
          package-manager-cache: false
      - name: Install Node dependencies
        run: |
          if [ -f package-lock.json ]; then
            npm ci
          else
            npm install
          fi
`
	case "Python":
		return `      - name: Set up Python
        uses: actions/setup-python@` + setupPythonSHA + ` # v7.0.0
        with:
          python-version: '3.13'
      - name: Install Python validation dependencies
        run: |
          python -m pip install pytest==` + pythonPytestVersion + ` ruff==` + pythonRuffVersion + `
          if [ -f requirements.txt ]; then python -m pip install -r requirements.txt; fi
          if [ -f requirements-dev.txt ]; then python -m pip install -r requirements-dev.txt; fi
          # Projects declaring dependencies only in pyproject.toml must configure them explicitly.
`
	case "Rust":
		return `      - name: Set up Rust toolchain
        uses: actions-rust-lang/setup-rust-toolchain@` + setupRustSHA + ` # v2.0.0
        with:
          components: clippy
          rustflags: ''
`
	default:
		// No runtime was detected. Unconfigured required checks already fail
		// closed, so a repository whose checks need only a shell can pass.
		return "      # No language runtime was detected; add setup steps here if your required checks need one.\n"
	}
}

type InitOptions struct {
	RepoRoot        string
	Force           bool
	SwarmVersion    string
	TemplateDir     string
	ForkCoordinator bool
	// UpstreamRemote names the project remote used for the repository name
	// and default branch; empty means origin.
	UpstreamRemote string
	// DryRun reports what init would do without writing anything.
	DryRun bool
}

type InitResult struct {
	CreatedFiles []string
	// UpdatedFiles were still as SwarmOS generated them and now match this version.
	UpdatedFiles []string
	// CurrentFiles already matched this version.
	CurrentFiles []string
	// CustomizedFiles were generated by SwarmOS, then edited; they are kept
	// (Force replaces them).
	CustomizedFiles []string
	// PreservedFiles already existed and were never recorded as generated by
	// SwarmOS; they are left as they are (Force replaces them).
	PreservedFiles []string
	// SectionFiles are repository-owned files that received or refreshed a
	// marked SwarmOS section.
	SectionFiles []string
	// RemovedFiles were generated by an earlier SwarmOS, are no longer owned by
	// it, and were still unmodified.
	RemovedFiles []string
	// Diffs maps each existing file a dry run would change or keep to a
	// unified diff from its current content to what this version generates.
	Diffs    map[string]string
	Stack    StackProfile
	Warnings []string
}

// CoordinatorWorkflowPath is the opt-in fork coordinator workflow. Once a
// repository has it, init keeps it current without --fork-coordinator.
const CoordinatorWorkflowPath = ".github/workflows/swarm-fork-coordinator.yml"

func InitRepository(opts InitOptions) (*InitResult, error) {
	if opts.SwarmVersion != "" {
		if err := ValidateSwarmVersionPin(opts.SwarmVersion); err != nil {
			return nil, err
		}
	}
	root := opts.RepoRoot
	if root == "" {
		root = "."
	}
	forkCoordinator := opts.ForkCoordinator || fileExists(filepath.Join(root, CoordinatorWorkflowPath))
	if forkCoordinator && strings.TrimSpace(opts.SwarmVersion) == "" {
		return nil, fmt.Errorf("the fork coordinator workflow must install a specific swarm, but this build is neither a release nor a clean commit (for example, it has uncommitted changes); run repo init from a released or committed swarm, or pass --swarm-version <v0.6.0-or-newer-tag-or-full-commit-SHA>")
	}
	stack := DetectStack(root)
	res := &InitResult{Stack: stack}
	safe, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer safe.Close()
	// Refuse an unsafe AGENTS.md or CLAUDE.md link before writing anything. A
	// safe link shares its target, so the SwarmOS section is written there once.
	instructions, err := safe.ResolveInstructions()
	if err != nil {
		return nil, err
	}

	recorded, err := loadManifest(safe)
	if err != nil {
		return nil, err
	}
	writer := &managedWriter{safe: safe, manifest: recorded, force: opts.Force, dryRun: opts.DryRun, res: res}
	writeFile := func(relPath string, data []byte) error { return writer.write(relPath, data, nil) }

	copyEmbedded := func(embedPath, targetRel string) error {
		data, err := embeddedTemplates.ReadFile(embedPath)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", embedPath, err)
		}
		return writeFile(targetRel, data)
	}

	// 1. .swarm/config.yaml
	if err := copyEmbedded("templates/swarm/config.yaml", ".swarm/config.yaml"); err != nil {
		return nil, err
	}

	// 2. .swarm/{project,architecture,conventions}.md
	projectName := repositoryName(root, opts.UpstreamRemote)
	projectMD := fmt.Sprintf("# Project: %s\n\n- **Primary Stack**: %s\n- **Build Command**: `%s`\n- **Test Command**: `%s`\n",
		projectName, stack.Language, stack.BuildCmd, stack.TestCmd)
	if err := writeFile(".swarm/project.md", []byte(projectMD)); err != nil {
		return nil, err
	}

	archMD := "# Architecture\n\n- Contract-driven work units live in GitHub Issues and are compiled into `.swarm/active/WORKUNIT.md` per isolated worktree.\n- Protected paths (`.github/workflows/**`, `.swarm/validation/**`, `.swarm/contracts/**`) are enforced by `swarm issue validate`.\n"
	if err := writeFile(".swarm/architecture.md", []byte(archMD)); err != nil {
		return nil, err
	}

	var convBuilder strings.Builder
	convBuilder.WriteString("# Engineering Conventions\n\n")
	for _, c := range stack.Conventions {
		convBuilder.WriteString("- " + c + "\n")
	}
	if err := writeFile(".swarm/conventions.md", []byte(convBuilder.String())); err != nil {
		return nil, err
	}
	// 2b. .swarm/goals.md, .swarm/SKILLS.md, AGENTS.md, .swarm/rules/, .swarm/skills/
	if err := copyEmbedded("templates/swarm/goals.md", ".swarm/goals.md"); err != nil {
		return nil, err
	}
	if err := copyEmbedded("templates/swarm/SKILLS.md", ".swarm/SKILLS.md"); err != nil {
		return nil, err
	}
	agentsTemplate, err := embeddedTemplates.ReadFile("templates/swarm/AGENTS.md")
	if err != nil {
		return nil, err
	}
	if err := writer.writeAgentsFile(filepath.ToSlash(instructions[0].Target), agentsTemplate); err != nil {
		return nil, err
	}
	if err := copyEmbedded("templates/swarm/rules/default.md", ".swarm/rules/default.md"); err != nil {
		return nil, err
	}
	if err := copyEmbedded("templates/swarm/rules/lessons.md", ".swarm/rules/lessons.md"); err != nil {
		return nil, err
	}
	if err := copyEmbedded("templates/swarm/skills/triage-and-plan.md", ".swarm/skills/triage-and-plan.md"); err != nil {
		return nil, err
	}

	// 3. Workflows
	for _, wf := range []string{"feature.yaml", "bugfix.yaml", "refactor.yaml"} {
		if err := copyEmbedded("templates/swarm/workflows/"+wf, ".swarm/workflows/"+wf); err != nil {
			return nil, err
		}
	}

	// 4. Role prompts
	for _, role := range []string{"planner.md", "reviewer.md", "developer.md", "tester.md"} {
		if err := copyEmbedded("templates/swarm/prompts/"+role, ".swarm/prompts/"+role); err != nil {
			return nil, err
		}
	}

	// 5. Schemas
	if err := writeFile(".swarm/contracts/workunit.schema.json", contract.WorkUnitSchemaBytes()); err != nil {
		return nil, err
	}
	if err := writeFile(".swarm/contracts/artifact.schema.json", contract.ArtifactSchemaBytes()); err != nil {
		return nil, err
	}

	// 6. Validation spec
	valSpec := contract.ValidationSpec{
		RequiredChecks: []contract.CheckSpec{
			{Name: "build", Command: stack.BuildCmd, Timeout: "5m"},
			{Name: "test", Command: stack.TestCmd, Timeout: "10m"},
			{Name: "lint", Command: stack.LintCmd, Timeout: "5m"},
		},
		ProtectedPaths: contract.DefaultProtectedPaths(),
	}
	if stack.Language == "Go" {
		// Formatting drift otherwise first shows up in CI after submit.
		valSpec.RequiredChecks = append([]contract.CheckSpec{{Name: "fmt", Command: GoFmtCheckCommand, Timeout: "2m"}}, valSpec.RequiredChecks...)
	}
	valBytes, err := yaml.Marshal(&valSpec)
	if err != nil {
		return nil, err
	}
	if err := writeFile(".swarm/validation/required-checks.yaml", valBytes); err != nil {
		return nil, err
	}

	// 7. GitHub Issue Templates & Action
	for _, tmpl := range []string{"swarm-workunit.yml", "swarm-community.yml", "swarm-feature.yml", "swarm-bugfix.yml", "swarm-refactor.yml"} {
		if err := copyEmbedded("templates/github/ISSUE_TEMPLATE/"+tmpl, ".github/ISSUE_TEMPLATE/"+tmpl); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(opts.TemplateDir) != "" {
		if err := copyCustomTemplateDir(opts.TemplateDir, writeFile); err != nil {
			return nil, err
		}
	}
	if opts.SwarmVersion == "" {
		res.Warnings = append(res.Warnings, "CI workflow omitted: this swarm build is neither a release nor a clean commit (for example, it has uncommitted changes); rerun repo init from a released or committed swarm to add it")
	} else {
		template, err := embeddedTemplates.ReadFile("templates/github/workflows/swarm-validate.yml")
		if err != nil {
			return nil, err
		}
		branch, warning := workflowDefaultBranchFrom(root, opts.UpstreamRemote)
		if warning != "" {
			res.Warnings = append(res.Warnings, warning)
		}
		if branch != "" {
			render := func(pin string) []byte {
				data := bytes.ReplaceAll(template, []byte("SWARM_VERSION_PIN"), []byte(pin))
				data = bytes.ReplaceAll(data, []byte("DEFAULT_BRANCH_YAML"), []byte(yamlQuotedString(branch)))
				return bytes.ReplaceAll(data, []byte("      # PROJECT_RUNTIME_SETUP"), []byte(projectRuntimeSetup(stack)))
			}
			if err := writer.write(".github/workflows/swarm-validate.yml", render(opts.SwarmVersion), samePinnedTemplate(render)); err != nil {
				return nil, err
			}
		}
	}
	if forkCoordinator {
		template, err := embeddedTemplates.ReadFile("templates/github/workflows/swarm-fork-coordinator.yml")
		if err != nil {
			return nil, err
		}
		render := func(pin string) []byte { return bytes.ReplaceAll(template, []byte("SWARM_VERSION_PIN"), []byte(pin)) }
		if err := writer.write(CoordinatorWorkflowPath, render(opts.SwarmVersion), samePinnedTemplate(render)); err != nil {
			return nil, err
		}
	}

	// 8. Universal Slash Commands (.claude/commands/ and .opencode/commands/)
	if err := ensureSlashCommands(writer); err != nil {
		return nil, err
	}
	if err := writer.retire(); err != nil {
		return nil, err
	}
	if opts.DryRun {
		return res, nil
	}
	if opts.SwarmVersion != "" {
		recorded.SwarmVersion = opts.SwarmVersion
	}
	if err := recorded.save(safe); err != nil {
		return nil, err
	}

	// 9. Update .gitignore
	if err := ensureGitignoreEntries(root, []string{".swarm/worktrees/", ".swarm/active/", ".swarm/local-tracker/", ".swarm/coordination/"}); err != nil {
		return nil, err
	}

	return res, nil
}

func ensureSlashCommands(w *managedWriter) error {
	for _, cmd := range []string{"swarm-plan.md", "swarm-review.md", "swarm-implement.md", "swarm-validate.md", "swarm-project-plan.md", OnboardCommand} {
		data, err := embeddedTemplates.ReadFile("templates/commands/" + cmd)
		if err != nil {
			return err
		}
		for _, dir := range []string{".claude/commands", ".opencode/commands"} {
			if err := w.write(dir+"/"+cmd, data, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyCustomTemplateDir(templateDir string, writeFile func(relPath string, data []byte) error) error {
	info, err := os.Stat(templateDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("invalid --template-dir %q: directory does not exist", templateDir)
	}
	copyDirFiles := func(srcDir, targetDir string, exts []string) error {
		entries, err := os.ReadDir(srcDir)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			matched := false
			for _, allowed := range exts {
				if ext == allowed {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			data, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
			if err != nil {
				return err
			}
			if err := writeFile(filepath.Join(targetDir, e.Name()), data); err != nil {
				return err
			}
		}
		return nil
	}
	for _, issueDir := range []string{templateDir, filepath.Join(templateDir, "ISSUE_TEMPLATE"), filepath.Join(templateDir, ".github", "ISSUE_TEMPLATE")} {
		if err := copyDirFiles(issueDir, ".github/ISSUE_TEMPLATE", []string{".yml", ".yaml"}); err != nil {
			return err
		}
	}
	for _, rulesDir := range []string{filepath.Join(templateDir, "rules"), filepath.Join(templateDir, ".swarm", "rules")} {
		if err := copyDirFiles(rulesDir, ".swarm/rules", []string{".md"}); err != nil {
			return err
		}
	}
	for _, skillsDir := range []string{filepath.Join(templateDir, "skills"), filepath.Join(templateDir, ".swarm", "skills")} {
		if err := copyDirFiles(skillsDir, ".swarm/skills", []string{".md"}); err != nil {
			return err
		}
	}
	for _, pair := range [][2]string{
		{filepath.Join(templateDir, "goals.md"), ".swarm/goals.md"},
		{filepath.Join(templateDir, ".swarm", "goals.md"), ".swarm/goals.md"},
		{filepath.Join(templateDir, "SKILLS.md"), ".swarm/SKILLS.md"},
		{filepath.Join(templateDir, ".swarm", "SKILLS.md"), ".swarm/SKILLS.md"},
	} {
		if data, err := os.ReadFile(pair[0]); err == nil {
			if err := writeFile(pair[1], data); err != nil {
				return err
			}
		}
	}
	return nil
}

func LoadConfig(repoRoot string) (*contract.SwarmConfig, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".swarm", "config.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read .swarm/config.yaml (run swarm repo init first): %w", err)
	}
	return decodeConfig(data)
}

// LoadConfigAt reads policy from the approved immutable commit, never candidate files.
func LoadConfigAt(repoRoot, baseSHA string) (*contract.SwarmConfig, error) {
	data, err := readAt(repoRoot, baseSHA, ".swarm/config.yaml")
	if err != nil {
		return nil, err
	}
	return decodeConfig(data)
}

func decodeConfig(data []byte) (*contract.SwarmConfig, error) {
	var cfg contract.SwarmConfig
	if err := decodeStrictYAML(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse .swarm/config.yaml: %w", err)
	}
	if cfg.Version != 1 || cfg.DefaultAgent == "" || len(cfg.Agents) == 0 {
		return nil, fmt.Errorf("config requires version: 1, default_agent, and an agents allowlist")
	}
	if _, ok := cfg.Agents[cfg.DefaultAgent]; !ok {
		return nil, fmt.Errorf("default agent %q is not in the agents allowlist", cfg.DefaultAgent)
	}
	if cfg.DefaultGate != contract.GateNone && cfg.DefaultGate != contract.GateAgent && cfg.DefaultGate != contract.GateHuman {
		return nil, fmt.Errorf("default_gate must be none, agent, or human")
	}
	if err := contract.ValidatePlanMode(cfg.PlanMode); err != nil {
		return nil, err
	}
	if cfg.CommitMessage != nil {
		if err := contract.ValidateCommitMessageDetail(cfg.CommitMessage.Detail); err != nil {
			return nil, err
		}
	}
	if _, err := contract.ResolveExecution(&cfg, ""); err != nil {
		return nil, err
	}
	if !cfg.Security.RequireReadyLabel || strings.TrimSpace(cfg.Security.ReadyLabel) == "" {
		return nil, fmt.Errorf("security requires require_ready_label: true and a nonempty ready_label")
	}
	if d, err := time.ParseDuration(cfg.Lease.Duration); err != nil || d <= 0 || cfg.Lease.MaxRenewals < 0 || cfg.Lease.RebaseRepairAttempts() < 0 || cfg.Lease.PlanReplanAttempts() < 0 {
		return nil, fmt.Errorf("lease requires a positive duration and nonnegative max_renewals, rebase_repair_limit and plan_replan_limit")
	}
	if cfg.Fork != nil && cfg.Fork.MaxActiveClaims <= 0 {
		return nil, fmt.Errorf("fork.max_active_claims must be positive")
	}
	for role, name := range cfg.Roles {
		switch role {
		case "planner", "reviewer", "developer", "tester":
		default:
			return nil, fmt.Errorf("unknown role %q", role)
		}
		if _, ok := cfg.Agents[name]; !ok {
			return nil, fmt.Errorf("role %s uses unapproved agent %q", role, name)
		}
	}
	for _, name := range cfg.Security.ReviewerAgents {
		agent, ok := cfg.Agents[name]
		if !ok {
			return nil, fmt.Errorf("security.reviewer_agents uses unapproved agent %q", name)
		}
		if len(agent.Roles) > 0 && !slices.Contains(agent.Roles, "reviewer") {
			return nil, fmt.Errorf("security.reviewer_agents entry %q is not allowed the reviewer role", name)
		}
	}
	for name, agent := range cfg.Agents {
		if agent.Authentication != "" && agent.Authentication != "auto" && agent.Authentication != "none" {
			return nil, fmt.Errorf("agent %q authentication must be auto or none", name)
		}
		if strings.TrimSpace(name) == "" || strings.TrimSpace(agent.Binary) == "" {
			return nil, fmt.Errorf("agents require nonempty names and executable paths")
		}
	}
	return &cfg, nil
}

func readAt(root, sha, path string) ([]byte, error) {
	if len(sha) != 40 && len(sha) != 64 {
		return nil, fmt.Errorf("approved base must be a full commit SHA")
	}
	for _, c := range sha {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return nil, fmt.Errorf("invalid approved base SHA")
		}
	}
	out, err := gitcredential.Output(context.Background(), root, "show", sha+":"+path)
	if err != nil {
		return nil, fmt.Errorf("read approved %s at %s: %w", path, sha, err)
	}
	return out, nil
}

func LoadWorkflow(repoRoot, name string) (*contract.WorkflowSpec, error) {
	if err := validateWorkflowName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, ".swarm", "workflows", name+".yaml"))
	if err != nil {
		return nil, fmt.Errorf("read workflow %q: %w", name, err)
	}
	return decodeWorkflow(data)
}

func LoadWorkflowAt(repoRoot, name, baseSHA string) (*contract.WorkflowSpec, error) {
	if err := validateWorkflowName(name); err != nil {
		return nil, err
	}
	data, err := readAt(repoRoot, baseSHA, ".swarm/workflows/"+name+".yaml")
	if err != nil {
		return nil, err
	}
	return decodeWorkflow(data)
}

func validateWorkflowName(name string) error {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("workflow must be a nonempty name without path separators")
	}
	return nil
}

func decodeWorkflow(data []byte) (*contract.WorkflowSpec, error) {
	var wf contract.WorkflowSpec
	if err := decodeStrictYAML(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	if wf.Name == "" || len(wf.Steps) == 0 {
		return nil, fmt.Errorf("workflow requires a name and prompt recipe steps")
	}
	if wf.DefaultGate != "" && wf.DefaultGate != contract.GateNone && wf.DefaultGate != contract.GateAgent && wf.DefaultGate != contract.GateHuman {
		return nil, fmt.Errorf("invalid workflow default_gate %q", wf.DefaultGate)
	}
	return &wf, nil
}

func LoadValidationSpec(repoRoot string) (*contract.ValidationSpec, error) {
	path := filepath.Join(repoRoot, ".swarm", "validation", "required-checks.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read .swarm/validation/required-checks.yaml: %w", err)
	}
	return DecodeValidationSpec(data)
}

func LoadValidationSpecAt(repoRoot, baseSHA string) (*contract.ValidationSpec, error) {
	data, err := readAt(repoRoot, baseSHA, ".swarm/validation/required-checks.yaml")
	if err != nil {
		return nil, err
	}
	return DecodeValidationSpec(data)
}

func DecodeValidationSpec(data []byte) (*contract.ValidationSpec, error) {
	var spec contract.ValidationSpec
	if err := decodeStrictYAML(data, &spec); err != nil {
		return nil, fmt.Errorf("parse required-checks.yaml: %w", err)
	}
	if len(spec.RequiredChecks) == 0 || len(spec.ProtectedPaths) == 0 {
		return nil, fmt.Errorf("validation policy requires at least one required check and one protected path")
	}
	names := make(map[string]bool, len(spec.RequiredChecks))
	for _, check := range spec.RequiredChecks {
		if strings.TrimSpace(check.Name) == "" || strings.TrimSpace(check.Command) == "" || names[check.Name] {
			return nil, fmt.Errorf("required checks need unique nonempty names and nonempty commands")
		}
		if timeout, err := time.ParseDuration(check.Timeout); err != nil || timeout <= 0 {
			return nil, fmt.Errorf("check %q requires a positive timeout", check.Name)
		}
		names[check.Name] = true
	}
	for _, path := range spec.ProtectedPaths {
		if strings.TrimSpace(path) == "" || filepath.IsAbs(path) || strings.HasPrefix(path, "../") {
			return nil, fmt.Errorf("protected paths must be nonempty repository-relative patterns")
		}
	}
	return &spec, nil
}

func decodeStrictYAML(data []byte, value any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("expected a single YAML document")
	}
	return nil
}

func LoadRolePromptAt(repoRoot, role, baseSHA string) (string, error) {
	switch role {
	case "planner", "reviewer", "developer", "tester":
	default:
		return "", fmt.Errorf("unknown role %q", role)
	}
	data, err := readAt(repoRoot, baseSHA, ".swarm/prompts/"+role+".md")
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "", fmt.Errorf("approved %s prompt is empty", role)
	}
	return string(data), nil
}

type DoctorCheck struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Status   string `json:"status"` // "OK", "WARN", "FAIL", "OPTIONAL"
	Detail   string `json:"detail"`
}

func RunDoctor(repoRoot string, checkGitHubAuth bool) []DoctorCheck {
	return RunDoctorWithRunner(repoRoot, checkGitHubAuth, nil)
}

func RunDoctorWithRunner(repoRoot string, checkGitHubAuth bool, run DoctorRunner) []DoctorCheck {
	var checks []DoctorCheck

	checks = append(checks, run.Run("system", "git", func() DoctorCheck {
		// 1. Git binary
		if gitPath, err := exec.LookPath("git"); err == nil {
			out, _ := exec.Command(gitPath, "--version").CombinedOutput()
			return DoctorCheck{
				Category: "system",
				Name:     "git",
				Status:   "OK",
				Detail:   strings.TrimSpace(string(out)),
			}
		} else {
			return DoctorCheck{
				Category: "system",
				Name:     "git",
				Status:   "FAIL",
				Detail:   "git binary not found in PATH",
			}
		}
	}))
	checks = append(checks, run.Run("tracker", "github-auth", func() DoctorCheck {
		// 2. GitHub Token / CLI Auth
		if _, source, err := gitcredential.GitHubToken(); err != nil {
			return DoctorCheck{
				Category: "tracker",
				Name:     "github-token",
				Status:   "FAIL",
				Detail:   err.Error(),
			}
		} else if source != "" {
			return DoctorCheck{
				Category: "tracker",
				Name:     "github-token",
				Status:   "OK",
				Detail:   fmt.Sprintf("authenticated via %s (gh CLI not required)", source),
			}
		} else if ghPath, err := exec.LookPath("gh"); err == nil {
			if checkGitHubAuth {
				if out, err := exec.Command(ghPath, "auth", "status").CombinedOutput(); err == nil {
					firstLine := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
					return DoctorCheck{
						Category: "tracker",
						Name:     "gh-auth",
						Status:   "OK",
						Detail:   firstLine,
					}
				} else {
					return DoctorCheck{
						Category: "tracker",
						Name:     "gh-auth",
						Status:   "WARN",
						Detail:   "gh installed but not authenticated (run gh auth login or set SWARM_GITHUB_TOKEN_COMMAND)",
					}
				}
			} else {
				return DoctorCheck{
					Category: "tracker",
					Name:     "gh-cli",
					Status:   "OK",
					Detail:   ghPath,
				}
			}
		} else {
			return DoctorCheck{
				Category: "tracker",
				Name:     "github-auth",
				Status:   "WARN",
				Detail:   "no SWARM_GITHUB_TOKEN_COMMAND/SWARM_GITHUB_TOKEN/GH_TOKEN set and gh CLI not found (local tracker still available)",
			}
		}
	}))
	// 3. .swarm/ Harness configuration
	var cfg *contract.SwarmConfig
	var configErr error
	checks = append(checks, run.Run("harness", ".swarm/config.yaml", func() DoctorCheck {
		cfg, configErr = LoadConfig(repoRoot)
		if configErr == nil && cfg.Version >= 1 {
			return DoctorCheck{
				Category: "harness",
				Name:     ".swarm/config.yaml",
				Status:   "OK",
				Detail:   fmt.Sprintf("v%d (default_agent=%s, default_gate=%s)", cfg.Version, cfg.DefaultAgent, cfg.DefaultGate),
			}
		} else {
			return DoctorCheck{
				Category: "harness",
				Name:     ".swarm/config.yaml",
				Status:   "FAIL",
				Detail:   fmt.Sprintf("missing or invalid .swarm/config.yaml: %v (run `swarm repo init`)", configErr),
			}
		}
	}))
	if configErr == nil {
		execution, err := contract.ResolveExecution(cfg, "")
		if err == nil {
			for _, role := range []string{"planner", "reviewer", "developer", "tester"} {
				checks = append(checks, run.Run("execution", role+" timeout", func() DoctorCheck {
					normal, _ := contract.ResolveAgentLimits(execution, role, false, 0)
					interactive, _ := contract.ResolveAgentLimits(execution, role, true, 0)
					detail := fmt.Sprintf("unattended %s; interactive wall %s", normal.Wall, interactive.Wall)
					if interactive.ExcludeIdle {
						detail += fmt.Sprintf("; working time %s (operator input excluded)", interactive.Active)
					}
					return DoctorCheck{Category: "execution", Name: role + " timeout", Status: "OK", Detail: detail + "; runs use approved-base policy"}
				}))
			}
		}
	}

	var spec *contract.ValidationSpec
	checks = append(checks, run.Run("harness", ".swarm/validation/required-checks.yaml", func() DoctorCheck {
		var err error
		if spec, err = LoadValidationSpec(repoRoot); err == nil {
			return DoctorCheck{
				Category: "harness",
				Name:     ".swarm/validation/required-checks.yaml",
				Status:   "OK",
				Detail:   fmt.Sprintf("%d checks, %d protected paths", len(spec.RequiredChecks), len(spec.ProtectedPaths)),
			}
		} else {
			return DoctorCheck{
				Category: "harness",
				Name:     ".swarm/validation/required-checks.yaml",
				Status:   "FAIL",
				Detail:   err.Error(),
			}
		}
	}))
	if spec != nil {
		// Advisory only: CI lint and format commands that no required check
		// covers show up as WARN suggestions; policy is never edited here.
		var drift []DoctorCheck
		summary := run.Run("harness", ciDriftCheckName, func() DoctorCheck {
			drift = doctorRequiredCheckDrift(repoRoot, spec)
			return drift[0]
		})
		checks = append(checks, summary)
		if len(drift) > 1 {
			checks = append(checks, drift[1:]...)
		}
	}
	// Probe configured executables rather than assuming particular installed CLIs.
	if cfg, err := LoadConfig(repoRoot); err == nil {
		names := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			checks = append(checks, run.Run("binary", name, func() DoctorCheck {
				binary := cfg.Agents[name].Binary
				check := DoctorCheck{Category: "binary", Name: name, Status: "OPTIONAL", Detail: "executable not found: " + binary}
				if path, err := exec.LookPath(binary); err == nil {
					check.Status, check.Detail = "OK", path+"; installation only; see execution checks"
				}
				return check
			}))
		}
	}

	if configErr != nil || cfg.Version < 1 {
		cfg = nil
	}
	checks = append(checks, run.Run("sandbox", "reviewer-runtime", func() DoctorCheck { return doctorSandboxCheck(cfg) }))
	return checks
}

// doctorSandboxCheck reports discovery only; agent preflights validate execution.
func doctorSandboxCheck(cfg *contract.SwarmConfig) DoctorCheck {
	check := DoctorCheck{Category: "sandbox", Name: "reviewer-runtime", Status: "WARN"}
	var runtimeName, containerRuntime, image string
	var err error
	if cfg != nil {
		name := cfg.Roles["reviewer"]
		if name == "" {
			name = cfg.DefaultAgent
		}
		ac := cfg.Agents[name]
		runtimeName, containerRuntime, image = ac.Sandbox, ac.ContainerRuntime, ac.ContainerImage
		var execution contract.ExecutionConfig
		execution, err = contract.ResolveExecution(cfg, "")
		if err == nil && execution.Mode == contract.ExecutionIsolated {
			runtimeName = ""
			if execution.Backend == contract.BackendOCI {
				runtimeName = "container"
				containerRuntime, image = execution.ContainerRuntime, execution.ContainerImage
				if ac.ContainerRuntime != "" {
					containerRuntime = ac.ContainerRuntime
				}
				if ac.ContainerImage != "" {
					image = ac.ContainerImage
				}
			}
		}
	}
	guidance := "configure execution.backend: oci with execution.container_runtime and execution.container_image"
	if runtimeName == "container" {
		if containerRuntime == "" {
			containerRuntime = "docker"
		}
		guidance = "install the selected " + containerRuntime + " CLI on PATH and configure its container image"
	} else if runtime.GOOS == "linux" {
		guidance = "install Bubblewrap and put bwrap on PATH, or " + guidance
	} else if runtime.GOOS == "darwin" {
		guidance = "ensure sandbox-exec is available on PATH, or " + guidance
	}
	var path string
	if err == nil {
		path, err = sandbox.ResolveReadOnlyRuntime(runtimeName, containerRuntime, image)
	}
	if err != nil {
		check.Detail = err.Error() + "; " + guidance
	} else {
		check.Status = "OK"
		check.Detail = "reviewer runtime binary available: " + path
	}
	return check
}

func ensureGitignoreEntries(repoRoot string, entries []string) error {
	safe, err := safefs.Open(repoRoot)
	if err != nil {
		return err
	}
	defer safe.Close()
	existing, err := safe.ReadFile(".gitignore", 16<<20)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	content := string(existing)
	var added []string
	for _, e := range entries {
		if !strings.Contains(content, e) {
			added = append(added, e)
		}
	}
	if len(added) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(content)
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("# SwarmOS ephemeral state\n")
	for _, a := range added {
		sb.WriteString(a + "\n")
	}
	return safe.WriteFile(".gitignore", []byte(sb.String()), 0o644)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// repositoryName names the project after its origin repository, so the
// generated description does not depend on the local clone's folder name.
func repositoryName(root, remote string) string {
	if remote == "" {
		remote = "origin"
	}
	out, err := gitcredential.Output(context.Background(), root, "remote", "get-url", remote)
	if err == nil {
		url := strings.TrimSuffix(strings.TrimSpace(string(out)), "/")
		url = strings.TrimSuffix(url, ".git")
		if i := strings.LastIndexAny(url, "/:"); i >= 0 && i < len(url)-1 {
			return url[i+1:]
		}
	}
	return filepath.Base(mustAbs(root))
}
