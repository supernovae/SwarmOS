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
	Input string         `json:"input"`
	Rows  []ReadinessRow `json:"rows"`
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
}

type ReadinessProject struct {
	Directory string
	Ecosystem string
	Source    string
	inputs    map[string][]byte
}

func readinessEcosystem(name string) string {
	switch path.Base(name) {
	case "package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "pnpm-workspace.yaml":
		return "node"
	case "go.mod", "go.work":
		return "go"
	case "Cargo.toml", "rust-toolchain", "rust-toolchain.toml":
		return "rust"
	case "global.json":
		return "dotnet"
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
		for _, part := range strings.Split(dir, "/") {
			switch part {
			case ".git", ".swarm", "node_modules", "vendor", "testdata":
				return nil
			}
		}
		eco := readinessEcosystem(name)
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
					case ".git", ".swarm", "node_modules", "vendor", "testdata":
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
	data []byte
}

func (w *readinessOutput) Write(p []byte) (int, error) {
	n := len(p)
	if room := 4096 - len(w.data); room > 0 {
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
	var firstFailure error
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
		row := ReadinessRow{Role: role, Agent: agent, Boundary: boundary, Project: project.Directory, Ecosystem: project.Ecosystem, Check: "toolchain", Source: project.Source, Status: "unverified", Reason: deferred}
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
		var unsupported string
		if row.Status == "passed" && project.Ecosystem == "node" {
			source, reason, pass := readinessNodeVersion(project, row.Observed)
			if !pass {
				row.Source, row.Reason, row.Status = source, reason, "failed"
			} else if strings.HasPrefix(reason, "unsupported") {
				unsupported = reason
			}
		}
		if row.Status == "failed" && firstFailure == nil {
			firstFailure = fmt.Errorf("%s readiness for %s (%s): %s", role, project.Directory, project.Source, row.Reason)
		}
		unverified := row
		unverified.Check = "requirements"
		unverified.Status, unverified.Observed = "unverified", ""
		if run != nil {
			unverified.Reason = "project version constraints and dependency declarations require further inspection"
			if row.Status == "passed" {
				unverified = checkReadinessDependencies(ctx, project, unverified, run)
				if unsupported != "" && unverified.Status == "passed" {
					unverified.Status, unverified.Reason = "unverified", unsupported+"; offline package check passed"
				}
			}
		}
		if unverified.Status == "failed" && firstFailure == nil {
			firstFailure = fmt.Errorf("%s readiness for %s (%s): %s", role, project.Directory, unverified.Source, unverified.Reason)
		}
		rows = append(rows, unverified, row)
	}
	return rows, firstFailure
}

var readinessCommands = map[string]string{
	"node":   "command -v node && node --version",
	"go":     "command -v go && GOTOOLCHAIN=local go version",
	"rust":   "command -v cargo && cargo --version && rustc --version",
	"dotnet": "command -v dotnet && dotnet --version",
	"c":      "command -v cc && cc --version && printf 'int main(void) { return 0; }\\n' | cc -x c - -o ./readiness",
	"cpp":    "command -v c++ && c++ --version && printf 'int main() { return 0; }\\n' | c++ -x c++ - -o ./readiness",
}
