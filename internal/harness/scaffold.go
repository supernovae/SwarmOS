package harness

import (
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"gopkg.in/yaml.v3"
)

//go:embed all:templates
var embeddedTemplates embed.FS

type StackProfile struct {
	Language    string
	BuildCmd    string
	TestCmd     string
	LintCmd     string
	Conventions []string
}

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
	case fileExists(filepath.Join(repoRoot, "pyproject.toml")):
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
			BuildCmd: "true",
			TestCmd:  "true",
			LintCmd:  "true",
			Conventions: []string{
				"Keep changes minimal, atomic, and well-tested.",
			},
		}
	}
}

type InitOptions struct {
	RepoRoot string
	Force    bool
}

type InitResult struct {
	CreatedFiles []string
	SkippedFiles []string
	Stack        StackProfile
}

func InitRepository(opts InitOptions) (*InitResult, error) {
	root := opts.RepoRoot
	if root == "" {
		root = "."
	}
	stack := DetectStack(root)
	res := &InitResult{Stack: stack}

	writeFile := func(relPath string, data []byte) error {
		full := filepath.Join(root, relPath)
		if !opts.Force && fileExists(full) {
			res.SkippedFiles = append(res.SkippedFiles, relPath)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return err
		}
		res.CreatedFiles = append(res.CreatedFiles, relPath)
		return nil
	}

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
	projectName := filepath.Base(mustAbs(root))
	projectMD := fmt.Sprintf("# Project: %s\n\n- **Primary Stack**: %s\n- **Build Command**: `%s`\n- **Test Command**: `%s`\n",
		projectName, stack.Language, stack.BuildCmd, stack.TestCmd)
	if err := writeFile(".swarm/project.md", []byte(projectMD)); err != nil {
		return nil, err
	}

	archMD := "# Architecture\n\n- Contract-driven work units live in GitHub Issues and are compiled into `.swarm/active/WORKUNIT.md` per isolated worktree.\n- Protected paths (`.github/workflows/**`, `.swarm/validation/**`, `.swarm/contracts/**`) are enforced by `swarm validate`.\n"
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
		ProtectedPaths: []string{
			".github/workflows/**",
			".swarm/validation/**",
			".swarm/contracts/**",
		},
	}
	valBytes, err := yaml.Marshal(&valSpec)
	if err != nil {
		return nil, err
	}
	if err := writeFile(".swarm/validation/required-checks.yaml", valBytes); err != nil {
		return nil, err
	}

	// 7. GitHub Issue Template & Action
	if err := copyEmbedded("templates/github/ISSUE_TEMPLATE/swarm-workunit.yml", ".github/ISSUE_TEMPLATE/swarm-workunit.yml"); err != nil {
		return nil, err
	}
	if err := copyEmbedded("templates/github/workflows/swarm-validate.yml", ".github/workflows/swarm-validate.yml"); err != nil {
		return nil, err
	}

	// 8. Universal Slash Commands (.claude/commands/ and .opencode/command/)
	if err := EnsureSlashCommands(root, opts.Force, res); err != nil {
		return nil, err
	}

	// 9. Update .gitignore
	if err := ensureGitignoreEntries(root, []string{".swarm/worktrees/", ".swarm/active/", ".swarm/local-tracker/"}); err != nil {
		return nil, err
	}

	return res, nil
}

func EnsureSlashCommands(root string, force bool, res *InitResult) error {
	cmds := []string{"swarm-plan.md", "swarm-review.md", "swarm-implement.md", "swarm-validate.md"}
	for _, cmd := range cmds {
		data, err := embeddedTemplates.ReadFile("templates/commands/" + cmd)
		if err != nil {
			return err
		}
		for _, dir := range []string{".claude/commands", ".opencode/command"} {
			rel := filepath.Join(dir, cmd)
			full := filepath.Join(root, rel)
			if !force && fileExists(full) {
				if res != nil {
					res.SkippedFiles = append(res.SkippedFiles, rel)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, data, 0o644); err != nil {
				return err
			}
			if res != nil {
				res.CreatedFiles = append(res.CreatedFiles, rel)
			}
		}
	}
	return nil
}

func LoadConfig(repoRoot string) (*contract.SwarmConfig, error) {
	path := filepath.Join(repoRoot, ".swarm", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		// Return sensible defaults if not initialized yet
		var cfg contract.SwarmConfig
		raw, _ := embeddedTemplates.ReadFile("templates/swarm/config.yaml")
		_ = yaml.Unmarshal(raw, &cfg)
		return &cfg, err
	}
	var cfg contract.SwarmConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse .swarm/config.yaml: %w", err)
	}
	return &cfg, nil
}

func LoadWorkflow(repoRoot, name string) (*contract.WorkflowSpec, error) {
	if name == "" {
		name = "feature"
	}
	path := filepath.Join(repoRoot, ".swarm", "workflows", name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		// Fallback to embedded workflow template
		data, err = embeddedTemplates.ReadFile("templates/swarm/workflows/" + name + ".yaml")
		if err != nil {
			return nil, fmt.Errorf("workflow %q not found: %w", name, err)
		}
	}
	var wf contract.WorkflowSpec
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow %q: %w", name, err)
	}
	return &wf, nil
}

func LoadValidationSpec(repoRoot string) (*contract.ValidationSpec, error) {
	path := filepath.Join(repoRoot, ".swarm", "validation", "required-checks.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read .swarm/validation/required-checks.yaml: %w", err)
	}
	var spec contract.ValidationSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse required-checks.yaml: %w", err)
	}
	return &spec, nil
}

func LoadRolePrompt(repoRoot, role string) string {
	path := filepath.Join(repoRoot, ".swarm", "prompts", role+".md")
	if data, err := os.ReadFile(path); err == nil {
		return string(data)
	}
	if data, err := embeddedTemplates.ReadFile("templates/swarm/prompts/" + role + ".md"); err == nil {
		return string(data)
	}
	return fmt.Sprintf("# Role: %s\n", role)
}

type DoctorCheck struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Status   string `json:"status"` // "OK", "WARN", "FAIL"
	Detail   string `json:"detail"`
}

func RunDoctor(repoRoot string, checkGitHubAuth bool) []DoctorCheck {
	var checks []DoctorCheck

	// 1. Git binary
	if gitPath, err := exec.LookPath("git"); err == nil {
		out, _ := exec.Command(gitPath, "--version").CombinedOutput()
		checks = append(checks, DoctorCheck{
			Category: "system",
			Name:     "git",
			Status:   "OK",
			Detail:   strings.TrimSpace(string(out)),
		})
	} else {
		checks = append(checks, DoctorCheck{
			Category: "system",
			Name:     "git",
			Status:   "FAIL",
			Detail:   "git binary not found in PATH",
		})
	}

	// 2. GitHub CLI / Auth
	if ghPath, err := exec.LookPath("gh"); err == nil {
		if checkGitHubAuth {
			if out, err := exec.Command(ghPath, "auth", "status").CombinedOutput(); err == nil {
				firstLine := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
				checks = append(checks, DoctorCheck{
					Category: "tracker",
					Name:     "gh-auth",
					Status:   "OK",
					Detail:   firstLine,
				})
			} else {
				checks = append(checks, DoctorCheck{
					Category: "tracker",
					Name:     "gh-auth",
					Status:   "WARN",
					Detail:   "gh installed but not authenticated (use --backend local or gh auth login)",
				})
			}
		} else {
			checks = append(checks, DoctorCheck{
				Category: "tracker",
				Name:     "gh-cli",
				Status:   "OK",
				Detail:   ghPath,
			})
		}
	} else {
		checks = append(checks, DoctorCheck{
			Category: "tracker",
			Name:     "gh-cli",
			Status:   "WARN",
			Detail:   "gh CLI not found (local tracker still available)",
		})
	}

	// 3. .swarm/ Harness configuration
	if cfg, err := LoadConfig(repoRoot); err == nil && cfg.Version >= 1 {
		checks = append(checks, DoctorCheck{
			Category: "harness",
			Name:     ".swarm/config.yaml",
			Status:   "OK",
			Detail:   fmt.Sprintf("v%d (default_agent=%s, default_gate=%s)", cfg.Version, cfg.DefaultAgent, cfg.DefaultGate),
		})
	} else {
		checks = append(checks, DoctorCheck{
			Category: "harness",
			Name:     ".swarm/config.yaml",
			Status:   "FAIL",
			Detail:   "missing or invalid .swarm/config.yaml (run `swarm init`)",
		})
	}

	if spec, err := LoadValidationSpec(repoRoot); err == nil {
		checks = append(checks, DoctorCheck{
			Category: "harness",
			Name:     ".swarm/validation/required-checks.yaml",
			Status:   "OK",
			Detail:   fmt.Sprintf("%d checks, %d protected paths", len(spec.RequiredChecks), len(spec.ProtectedPaths)),
		})
	} else {
		checks = append(checks, DoctorCheck{
			Category: "harness",
			Name:     ".swarm/validation/required-checks.yaml",
			Status:   "FAIL",
			Detail:   err.Error(),
		})
	}

	// 4. Probe all 4 target agent harnesses: claude, codex, opencode, omp
	for _, agentName := range []string{"claude", "codex", "opencode", "omp"} {
		if binPath, err := exec.LookPath(agentName); err == nil {
			checks = append(checks, DoctorCheck{
				Category: "agent",
				Name:     agentName,
				Status:   "OK",
				Detail:   binPath,
			})
		} else {
			checks = append(checks, DoctorCheck{
				Category: "agent",
				Name:     agentName,
				Status:   "WARN",
				Detail:   "not found in PATH",
			})
		}
	}

	return checks
}

func ensureGitignoreEntries(repoRoot string, entries []string) error {
	gitignorePath := filepath.Join(repoRoot, ".gitignore")
	existing, _ := os.ReadFile(gitignorePath)
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
	return os.WriteFile(gitignorePath, []byte(sb.String()), 0o644)
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
