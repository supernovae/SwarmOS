package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/safefs"
	"gopkg.in/yaml.v3"
)

const ciDriftCheckName = "ci lint/format coverage"

// checkTool is a lint or format tool that doctor recognizes in CI run steps
// and required check commands.
type checkTool struct {
	id      string
	match   func(args []string) bool
	name    string // suggested required check name
	command string // suggested read-only check command
}

func anyArgs([]string) bool { return true }

func hasArg(want ...string) func([]string) bool {
	return func(args []string) bool {
		for _, arg := range args {
			for _, w := range want {
				if arg == w {
					return true
				}
			}
		}
		return false
	}
}

// recognizedCheckTools lists the lint and format tools doctor compares between
// CI workflows and required checks. Mutating invocations such as `black .` or
// `prettier --write` are not checks and are deliberately not recognized.
var recognizedCheckTools = map[string][]checkTool{
	"gofmt":         {{id: "gofmt", match: anyArgs, name: "fmt", command: GoFmtCheckCommand}},
	"staticcheck":   {{id: "staticcheck", match: anyArgs, name: "staticcheck", command: "staticcheck ./..."}},
	"golangci-lint": {{id: "golangci-lint", match: anyArgs, name: "golangci-lint", command: "golangci-lint run"}},
	"eslint":        {{id: "eslint", match: anyArgs, name: "eslint", command: "npx eslint ."}},
	"prettier":      {{id: "prettier --check", match: hasArg("--check", "-c"), name: "prettier", command: "npx prettier --check ."}},
	"ruff":          {{id: "ruff", match: anyArgs, name: "ruff", command: "ruff check ."}},
	"black":         {{id: "black --check", match: hasArg("--check"), name: "black", command: "black --check ."}},
	"go": {
		{id: "go vet", match: func(args []string) bool { return len(args) > 0 && args[0] == "vet" }, name: "vet", command: "go vet ./..."},
	},
	"cargo": {
		{id: "cargo fmt --check", match: func(args []string) bool { return len(args) > 0 && args[0] == "fmt" && hasArg("--check")(args) }, name: "cargo-fmt", command: "cargo fmt --all -- --check"},
		{id: "cargo clippy", match: func(args []string) bool { return len(args) > 0 && args[0] == "clippy" }, name: "clippy", command: "cargo clippy --all-targets -- -D warnings"},
	},
}

// toolUse is one recognized tool invocation.
type toolUse struct {
	tool    checkTool
	command string
}

// recognizeTools returns the recognized lint and format tools that script
// invokes in command position. It splits shell syntax statically, without
// expanding variables, resolving make targets or scripts, or running anything.
func recognizeTools(script string) []toolUse {
	var uses []toolUse
	for _, words := range shellCommands(script) {
		original := strings.Join(words, " ")
		words = stripLaunchers(words)
		if len(words) == 0 {
			continue
		}
		name := strings.TrimSuffix(path.Base(filepath.ToSlash(words[0])), ".exe")
		args := words[1:]
		// `go tool staticcheck` and `go run .../cmd/staticcheck@v` run the
		// named tool rather than a go subcommand.
		if name == "go" && len(args) > 1 && args[0] == "tool" {
			name, args = path.Base(args[1]), args[2:]
		} else if name == "go" && len(args) > 1 && args[0] == "run" {
			for i, arg := range args[1:] {
				if base, _, _ := strings.Cut(path.Base(arg), "@"); strings.Contains(arg, "/") && len(recognizedCheckTools[base]) > 0 && base != "go" && base != "cargo" {
					name, args = base, args[i+2:]
					break
				}
			}
		} else if name == "cargo" && len(args) > 0 && strings.HasPrefix(args[0], "+") {
			args = args[1:]
		}
		for _, tool := range recognizedCheckTools[name] {
			if tool.match(args) {
				uses = append(uses, toolUse{tool: tool, command: original})
			}
		}
	}
	return uses
}

var launchers = map[string]bool{"sudo": true, "env": true, "exec": true, "time": true, "command": true, "nice": true, "npx": true, "bunx": true, "pnpx": true}

var subcommandLaunchers = map[string]map[string]bool{
	"npm":     {"exec": true},
	"pnpm":    {"exec": true, "dlx": true},
	"yarn":    {"exec": true, "dlx": true},
	"uv":      {"run": true},
	"poetry":  {"run": true},
	"pipx":    {"run": true},
	"python":  {"-m": true},
	"python3": {"-m": true},
}

// stripLaunchers removes leading variable assignments and common wrappers such
// as `npx`, `uv run`, or `python -m`, together with their options.
func stripLaunchers(words []string) []string {
	for len(words) > 0 {
		first := path.Base(words[0])
		switch {
		case isAssignment(words[0]):
			words = words[1:]
		case launchers[first]:
			words = words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || isAssignment(words[0])) {
				words = words[1:]
			}
		case len(words) > 1 && subcommandLaunchers[first][words[1]]:
			words = words[2:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				words = words[1:]
			}
		default:
			return words
		}
	}
	return words
}

func isAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" {
		return false
	}
	for i, c := range name {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// shellCommands splits a shell script into simple commands of unquoted words.
// It understands quotes, escapes, comments, command separators, subshells, and
// $(...) or `...` substitutions, which become commands of their own.
func shellCommands(script string) [][]string {
	l := &shellLexer{s: script}
	l.commands(0)
	return l.cmds
}

type shellLexer struct {
	s    string
	i    int
	cmds [][]string
}

// commands reads until the end of input, or until the closing delimiter of a
// substitution (')' or '`').
func (l *shellLexer) commands(closer byte) {
	var words []string
	var word strings.Builder
	inWord := false
	flushWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	flushCommand := func() {
		flushWord()
		if len(words) > 0 {
			l.cmds = append(l.cmds, words)
			words = nil
		}
	}
	depth := 0
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case closer != 0 && c == closer && (closer == '`' || depth == 0):
			l.i++
			flushCommand()
			return
		case c == '\\':
			if l.i+1 < len(l.s) && l.s[l.i+1] != '\n' {
				word.WriteByte(l.s[l.i+1])
				inWord = true
			}
			l.i += 2
		case c == '\'':
			end := strings.IndexByte(l.s[l.i+1:], '\'')
			if end < 0 {
				end = len(l.s) - l.i - 1
			}
			word.WriteString(l.s[l.i+1 : l.i+1+end])
			inWord = true
			l.i += end + 2
		case c == '"':
			l.i++
			inWord = true
			l.doubleQuoted(&word)
		case c == '$' && l.i+1 < len(l.s) && l.s[l.i+1] == '(':
			// The substitution's output is unknown; keep the word but drop it.
			l.i += 2
			l.commands(')')
			inWord = true
		case c == '`' && closer != '`':
			l.i++
			l.commands('`')
			inWord = true
		case c == '#' && !inWord:
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
		case c == '(':
			depth++
			flushCommand()
			l.i++
		case c == ')':
			depth--
			flushCommand()
			l.i++
		case c == ';' || c == '&' || c == '|' || c == '\n' || c == '{' && !inWord || c == '}' && !inWord:
			flushCommand()
			l.i++
		case c == ' ' || c == '\t' || c == '\r':
			flushWord()
			l.i++
		default:
			word.WriteByte(c)
			inWord = true
			l.i++
		}
	}
	flushCommand()
}

func (l *shellLexer) doubleQuoted(word *strings.Builder) {
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == '"':
			l.i++
			return
		case c == '\\' && l.i+1 < len(l.s):
			word.WriteByte(l.s[l.i+1])
			l.i += 2
		case c == '$' && l.i+1 < len(l.s) && l.s[l.i+1] == '(':
			l.i += 2
			l.commands(')')
		case c == '`':
			l.i++
			l.commands('`')
		default:
			word.WriteByte(c)
			l.i++
		}
	}
}

// doctorRequiredCheckDrift warns when a GitHub workflow run step invokes a
// recognized lint or format tool that no local required check invokes. It only
// reads the workflows and policy; suggestions are for maintainers to apply.
func doctorRequiredCheckDrift(repoRoot string, spec *contract.ValidationSpec) []DoctorCheck {
	warn := func(detail string) DoctorCheck {
		return DoctorCheck{Category: "harness", Name: ciDriftCheckName, Status: "WARN", Detail: detail}
	}
	covered := map[string]bool{}
	names := map[string]bool{}
	for _, check := range spec.RequiredChecks {
		names[check.Name] = true
		for _, use := range recognizeTools(check.Command) {
			covered[use.tool.id] = true
		}
	}
	root, err := safefs.Open(repoRoot)
	if err != nil {
		return []DoctorCheck{warn(fmt.Sprintf("cannot open repository to read workflows: %v", err))}
	}
	defer root.Close()
	const dir = ".github/workflows"
	files, err := workflowFiles(root, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []DoctorCheck{{Category: "harness", Name: ciDriftCheckName, Status: "OK", Detail: "no .github/workflows to compare with required checks"}}
	}
	if err != nil {
		return []DoctorCheck{warn(fmt.Sprintf("cannot read %s: %v", dir, err))}
	}
	var checks []DoctorCheck
	for _, file := range files {
		rel := dir + "/" + file
		data, err := root.ReadFile(filepath.FromSlash(rel), 1<<20)
		if err != nil {
			checks = append(checks, warn(fmt.Sprintf("cannot read %s: %v", rel, err)))
			continue
		}
		var workflow struct {
			Jobs map[string]struct {
				Steps []struct {
					Run string `yaml:"run"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			checks = append(checks, warn(fmt.Sprintf("cannot parse %s: %v", rel, err)))
			continue
		}
		jobs := make([]string, 0, len(workflow.Jobs))
		for job := range workflow.Jobs {
			jobs = append(jobs, job)
		}
		sort.Strings(jobs)
		reported := map[string]bool{}
		for _, job := range jobs {
			for _, step := range workflow.Jobs[job].Steps {
				for _, use := range recognizeTools(step.Run) {
					if covered[use.tool.id] || reported[use.tool.id] {
						continue
					}
					reported[use.tool.id] = true
					checks = append(checks, warn(fmt.Sprintf("%s runs %q (%s), but no required check does; suggested .swarm/validation/required-checks.yaml entry: %s",
						rel, use.command, use.tool.id, suggestedCheck(use, names))))
				}
			}
		}
	}
	if len(checks) == 0 {
		return []DoctorCheck{{Category: "harness", Name: ciDriftCheckName, Status: "OK", Detail: "required checks cover every recognized lint/format tool in .github/workflows"}}
	}
	return checks
}

// workflowFiles lists regular .yml and .yaml files in dir, sorted by name.
func workflowFiles(root *safefs.Root, dir string) ([]string, error) {
	d, err := root.OpenDirectory(filepath.FromSlash(dir))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		ext := path.Ext(entry.Name())
		if entry.Type().IsRegular() && (ext == ".yml" || ext == ".yaml") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}

// suggestedCheck renders a single-line YAML required check for use, keeping
// `go tool` invocations and avoiding names the policy already uses.
func suggestedCheck(use toolUse, names map[string]bool) string {
	command := use.tool.command
	if fields := stripLaunchers(strings.Fields(use.command)); len(fields) > 2 && path.Base(fields[0]) == "go" && fields[1] == "tool" && !strings.HasPrefix(command, "go ") {
		command = "go tool " + command
	}
	name := use.tool.name
	for i := 2; names[name]; i++ {
		name = fmt.Sprintf("%s-%d", use.tool.name, i)
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
	for _, kv := range [][2]string{{"name", name}, {"command", command}, {"timeout", "5m"}} {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: kv[0]}, &yaml.Node{Kind: yaml.ScalarNode, Value: kv[1]})
	}
	out, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Sprintf("name %s, command %q", name, command)
	}
	return "- " + strings.TrimSpace(string(out))
}
