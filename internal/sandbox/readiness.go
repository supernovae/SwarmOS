package sandbox

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/safefs"
)

const maxReadinessInput = 4 << 20

type ReadinessReport struct {
	Input   string         `json:"input"`
	Outcome string         `json:"outcome"`
	Rows    []ReadinessRow `json:"rows"`
}

type ReadinessRow struct {
	Role      string `json:"role"`
	Agent     string `json:"agent"`
	Boundary  string `json:"boundary"`
	Project   string `json:"project"`
	Ecosystem string `json:"ecosystem"`
	Check     string `json:"check"`
	Source    string `json:"source"`
	Status    string `json:"status"`
	Observed  string `json:"observed,omitempty"`
	Reason    string `json:"reason"`
	Required  bool   `json:"required"`
}

// RefreshOutcome summarizes the observations and any boundary failure.
func (r *ReadinessReport) RefreshOutcome(err error) {
	r.Outcome = "ready"
	if err != nil {
		r.Outcome = "blocked"
	}
	for _, row := range r.Rows {
		if row.Status == "failed" || (row.Required && row.Status == "unverified") {
			r.Outcome = "blocked"
		} else if row.Status == "unverified" && r.Outcome == "ready" {
			r.Outcome = "unverified"
		}
	}
}

type ReadinessProject struct {
	Directory     string
	Ecosystem     string
	Source        string
	inputs        map[string][]byte
	workspaceRoot string
}

func readinessEcosystem(name string) string {
	switch path.Base(name) {
	case "package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "pnpm-workspace.yaml":
		return "node"
	case "go.mod", "go.work":
		return "go"
	case "Cargo.toml", "rust-toolchain", "rust-toolchain.toml":
		return "rust"
	case "pyproject.toml", "uv.lock", ".python-version":
		return "python"
	case "pom.xml":
		return "maven"
	case "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "gradle.lockfile":
		return "gradle"
	case "global.json":
		return "dotnet"
	}
	if ok, _ := path.Match("requirements*.txt", path.Base(name)); ok {
		return "python"
	}
	switch path.Ext(name) {
	case ".csproj", ".fsproj", ".vbproj":
		return "dotnet"
	case ".c":
		return "c"
	case ".cc", ".cpp", ".cxx", ".C":
		return "cpp"
	}
	return ""
}

func DiscoverReadiness(ctx context.Context, root, base string) ([]ReadinessProject, error) {
	var projects []ReadinessProject
	seen := map[[2]string]bool{}
	inputs := map[string][]byte{}
	total := 0
	add := func(name string, read func() ([]byte, error)) error {
		if !filepath.IsLocal(name) || strings.Contains(name, "\\") {
			return fmt.Errorf("invalid readiness input path %q", name)
		}
		dir := path.Dir(name)
		wrapper := path.Base(name) == "gradle-wrapper.properties" && path.Base(dir) == "wrapper" && path.Base(path.Dir(dir)) == "gradle"
		if wrapper {
			dir = path.Dir(path.Dir(dir))
		}
		for _, part := range strings.Split(dir, "/") {
			switch part {
			case ".git", ".swarm", "node_modules", "vendor", "testdata", ".venv", ".gradle":
				return nil
			}
		}
		eco := readinessEcosystem(name)
		if wrapper {
			eco = "gradle"
		}
		if readinessProbeInput(name) {
			data, err := read()
			if err != nil {
				return fmt.Errorf("read readiness input %s: %w", name, err)
			}
			total += len(data)
			if total > 24<<20 || len(inputs) >= 1500 {
				return fmt.Errorf("project inputs exceed readiness limits")
			}
			inputs[name] = data
		}
		if eco == "" {
			return nil
		}
		key := [2]string{dir, eco}
		if seen[key] {
			return nil
		}
		if len(projects) >= 512 {
			return fmt.Errorf("too many project inputs for readiness inspection")
		}
		seen[key] = true
		projects = append(projects, ReadinessProject{Directory: dir, Ecosystem: eco, Source: name, inputs: inputs})
		return nil
	}
	if base == "" {
		files, err := safefs.Open(root)
		if err != nil {
			return nil, err
		}
		defer files.Close()
		err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name != root {
					switch entry.Name() {
					case ".git", ".swarm", "node_modules", "vendor", "testdata", ".venv", ".gradle":
						return filepath.SkipDir
					}
				}
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, name)
			if err != nil {
				return err
			}
			return add(filepath.ToSlash(rel), func() ([]byte, error) {
				return files.ReadFile(rel, maxReadinessInput)
			})
		})
		if err != nil {
			return nil, err
		}
	} else {
		entries, err := recursiveBaseTree(ctx, root, base)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") {
				continue
			}
			if err := add(entry.Path, func() ([]byte, error) {
				size, err := gitcredential.Output(ctx, root, "cat-file", "-s", entry.Object)
				if err != nil {
					return nil, err
				}
				n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64)
				if err != nil || n < 0 || n > maxReadinessInput {
					return nil, fmt.Errorf("manifest exceeds readiness input limit")
				}
				return gitcredential.Output(ctx, root, "cat-file", "blob", entry.Object)
			}); err != nil {
				return nil, err
			}
		}
	}
	inputNames := make([]string, 0, len(inputs))
	for name := range inputs {
		inputNames = append(inputNames, name)
	}
	sort.Strings(inputNames)
	for _, name := range inputNames {
		dir := path.Dir(name)
		languages := readinessNativeLanguages(path.Base(name), inputs[name])
		for _, eco := range languages {
			key := [2]string{dir, eco}
			if !seen[key] {
				if len(projects) >= 512 {
					return nil, fmt.Errorf("too many projects for readiness inspection")
				}
				seen[key] = true
				projects = append(projects, ReadinessProject{Directory: dir, Ecosystem: eco, Source: name, inputs: inputs})
			}
		}
	}
	projects = deduplicateReadinessProjects(projects, inputs)
	sort.Slice(projects, func(i, j int) bool {
		a, b := projects[i], projects[j]
		if a.Directory != b.Directory {
			return a.Directory < b.Directory
		}
		return a.Ecosystem < b.Ecosystem
	})
	return projects, nil
}

type ReadinessExecutor func(context.Context, string, string, io.Writer, io.Writer) error

type readinessOutput struct {
	data      []byte
	limit     int
	truncated bool
}

func (w *readinessOutput) Write(p []byte) (int, error) {
	n := len(p)
	limit := w.limit
	if limit == 0 {
		limit = 4096
	}
	room := limit - len(w.data)
	w.truncated = w.truncated || n > room
	if room > 0 {
		w.data = append(w.data, p[:min(n, room)]...)
	}
	return n, nil
}

// CheckReadiness executes only coordinator-authored commands against an empty
// disposable workspace. Project declarations are never treated as shell input.
func CheckReadiness(ctx context.Context, projects []ReadinessProject, role, agent, boundary, deferred string, run ReadinessExecutor) ([]ReadinessRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var rows []ReadinessRow
	var dir string
	if run != nil && len(projects) > 0 {
		var err error
		dir, err = os.MkdirTemp("", "swarm-readiness-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
	}
	observed := map[string]ReadinessRow{}
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return rows, fmt.Errorf("%s project readiness: %w", role, err)
		}
		row := ReadinessRow{Role: role, Agent: agent, Boundary: boundary, Project: project.Directory, Ecosystem: project.Ecosystem, Check: "toolchain", Source: project.Source, Status: "unverified", Reason: deferred, Required: run != nil}
		if run != nil {
			if prior, ok := observed[project.Ecosystem]; ok {
				row.Status, row.Observed, row.Reason = prior.Status, prior.Observed, prior.Reason
			} else {
				var out, diagnostics readinessOutput
				probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := run(probeCtx, dir, readinessCommands[project.Ecosystem], &out, &diagnostics)
				cancel()
				row.Observed = strings.TrimSpace(string(out.data))
				row.Status, row.Reason = "passed", "toolchain command succeeded"
				if err != nil {
					row.Status = "failed"
					row.Reason = fmt.Sprintf("select a %s environment with a working %s toolchain", role, project.Ecosystem)
					if detail := strings.TrimSpace(string(diagnostics.data)); detail != "" {
						row.Reason += fmt.Sprintf(" (%q)", detail)
					} else {
						row.Reason += fmt.Sprintf(" (%v)", err)
					}
				}
				observed[project.Ecosystem] = row
			}
		}
		if row.Status == "passed" && project.Ecosystem == "node" {
			source, reason, pass := readinessNodeVersion(project, row.Observed)
			if !pass {
				row.Source, row.Reason, row.Status = source, fmt.Sprintf("%s (observed %q)", reason, row.Observed), "failed"
			} else if strings.HasPrefix(reason, "unsupported") {
				row.Source, row.Status = source, "unverified"
				row.Reason = reason + "; declare a supported numeric Node engine range"
			}
		}
		requirements := row
		requirements.Check = "requirements"
		requirements.Status, requirements.Observed = "unverified", ""
		if run == nil {
			rows = append(rows, requirements, row)
			continue
		}
		requirements.Reason = "dependency checks require a verified toolchain"
		requirements.Required = row.Status == "passed"
		if row.Status == "passed" && project.Ecosystem == "node" && project.workspaceRoot == "" {
			manager := checkReadinessNodeManager(ctx, project, row, dir, run)
			if manager.Status != "passed" {
				requirements.Reason = "locked package checks require the selected package manager"
				requirements.Required = false
				rows = append(rows, manager, requirements, row)
				continue
			}
			rows = append(rows, manager)
		}
		if row.Status == "passed" {
			if project.Ecosystem == "node" && project.workspaceRoot != "" {
				requirements.Required = false
				requirements.Status = "passed"
				requirements.Reason = "locked packages are checked at workspace root " + project.workspaceRoot
			} else {
				requirements = checkReadinessDependencies(ctx, project, requirements, run)
			}
		}
		rows = append(rows, requirements, row)
	}
	for _, row := range rows {
		if row.Status == "failed" {
			return rows, readinessCheckError(row)
		}
	}
	for _, row := range rows {
		if row.Required && row.Status == "unverified" {
			return rows, readinessCheckError(row)
		}
	}
	return rows, nil
}

func readinessCheckError(row ReadinessRow) error {
	return fmt.Errorf("%s readiness for %s (%s): %s", row.Role, row.Project, row.Source, row.Reason)

}

var readinessCommands = map[string]string{
	"node":   "command -v node && node --version && node -e 'process.exit(0)'",
	"python": "python3 -I --version",
	"maven":  "java -version 2>&1 && mvn --version",
	"gradle": "java -version 2>&1 && gradle --version",
	"go":     "command -v go && GOTOOLCHAIN=local go version",
	"rust":   "command -v cargo && cargo --version && rustc --version",
	"dotnet": "command -v dotnet && dotnet --version",
	"c":      "command -v cc && cc --version && printf 'int main(void) { return 0; }\\n' | cc -x c - -o ./readiness",
	"cpp":    "command -v c++ && c++ --version && printf 'int main() { return 0; }\\n' | c++ -x c++ - -o ./readiness",
}
