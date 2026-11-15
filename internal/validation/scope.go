package validation

import (
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/safefs"
)

type goModuleGraph struct {
	modulePath string
	packages   map[string]bool
	importers  map[string]map[string]bool
}

type goDiffScope struct {
	allAffected bool
	packages    map[string]bool
	affected    map[string]bool
}

// scopeChecks filters and narrows required checks according to their approved
// tier and diff scope. When modified is empty (for example push validation at
// HEAD == baseSHA), diff scoping is bypassed so the full policy executes.
func scopeChecks(worktreePath string, checks []contract.CheckSpec, modified []string, ciMode bool) ([]contract.CheckSpec, string) {
	var active []contract.CheckSpec
	var deferredCI []string
	var skippedDiff []string

	var diffComputed bool
	var diff goDiffScope

	for _, check := range checks {
		if check.Tier == "ci" && !ciMode {
			deferredCI = append(deferredCI, check.Name)
			continue
		}
		if len(modified) == 0 {
			active = append(active, check)
			continue
		}
		switch check.Scope {
		case "", "always":
			active = append(active, check)
		case "paths":
			if matchesAnyGlob(check.Paths, modified) {
				active = append(active, check)
			} else {
				skippedDiff = append(skippedDiff, check.Name)
			}
		case "affected":
			if !diffComputed {
				diff = analyzeGoDiff(worktreePath, modified)
				diffComputed = true
			}
			pathMatched := len(check.Paths) > 0 && matchesAnyGlob(check.Paths, modified)
			if !pathMatched && !diff.allAffected && len(diff.affected) == 0 {
				skippedDiff = append(skippedDiff, check.Name)
				continue
			}
			if ciMode || diff.allAffected || pathMatched {
				active = append(active, check)
				continue
			}
			narrowed, keep := narrowGoCheckCommand(check.Command, diff.packages, diff.affected)
			if !keep {
				skippedDiff = append(skippedDiff, check.Name)
				continue
			}
			c := check
			c.Command = narrowed
			active = append(active, c)
		default:
			active = append(active, check)
		}
	}

	var notes []string
	if len(deferredCI) > 0 {
		notes = append(notes, "deferred to CI ("+strings.Join(deferredCI, ", ")+")")
	}
	if len(skippedDiff) > 0 {
		notes = append(notes, "skipped outside diff scope ("+strings.Join(skippedDiff, ", ")+")")
	}
	if len(notes) == 0 {
		return active, ""
	}
	return active, "Check scope: " + strings.Join(notes, "; ") + "."
}

func matchesAnyGlob(patterns, files []string) bool {
	for _, file := range files {
		clean := filepath.ToSlash(file)
		for _, pattern := range patterns {
			if matchGlobPattern(pattern, clean) {
				return true
			}
		}
	}
	return false
}

func isGlobalGoTrigger(file string) bool {
	switch file {
	case "go.mod", "go.sum", "go.work", "go.work.sum", "Makefile", "AGENTS.md", "CLAUDE.md":
		return true
	}
	for _, prefix := range []string{".github/", ".swarm/", ".claude/", ".opencode/"} {
		if strings.HasPrefix(file, prefix) {
			return true
		}
	}
	return path.Dir(file) == "." && strings.HasSuffix(file, ".go")
}

func isProseOnlyFile(file string) bool {
	switch file {
	case "README.md", "SECURITY.md", "CHANGELOG.md", ".gitignore":
		return true
	}
	if strings.HasPrefix(file, "LICENSE") || strings.HasPrefix(file, "changes/") {
		return true
	}
	if strings.HasPrefix(file, "docs/") && file != "docs/human-led.md" {
		return true
	}
	return path.Dir(file) == "." && strings.HasSuffix(file, ".md") && file != "AGENTS.md" && file != "CLAUDE.md"
}

func analyzeGoDiff(worktreePath string, modified []string) goDiffScope {
	var codeFiles []string
	for _, raw := range modified {
		file := filepath.ToSlash(raw)
		if isGlobalGoTrigger(file) {
			return goDiffScope{allAffected: true}
		}
		if isProseOnlyFile(file) {
			continue
		}
		codeFiles = append(codeFiles, file)
	}
	if len(codeFiles) == 0 {
		return goDiffScope{}
	}
	graph, ok := discoverModuleGraph(worktreePath)
	if !ok {
		return goDiffScope{allAffected: true}
	}
	changed := make(map[string]bool)
	for _, file := range codeFiles {
		if file == "docs/human-led.md" {
			if !graph.packages["./internal/cli"] {
				return goDiffScope{allAffected: true}
			}
			changed["./internal/cli"] = true
			continue
		}
		pkg := enclosingGoPackage(file, graph.packages)
		if pkg == "" {
			return goDiffScope{allAffected: true}
		}
		changed[pkg] = true
	}
	affected := make(map[string]bool, len(changed)*2)
	for pkg := range changed {
		affected[pkg] = true
		for imp := range graph.importers[pkg] {
			affected[imp] = true
		}
	}
	return goDiffScope{packages: graph.packages, affected: affected}
}

func enclosingGoPackage(file string, packages map[string]bool) string {
	dir := path.Dir(file)
	for dir != "." && dir != "/" && dir != "" {
		cand := "./" + dir
		if packages[cand] {
			return cand
		}
		dir = path.Dir(dir)
	}
	if packages["."] {
		return "."
	}
	return ""
}

func discoverModuleGraph(worktreePath string) (goModuleGraph, bool) {
	if _, err := os.Lstat(filepath.Join(worktreePath, "go.work")); err == nil {
		return goModuleGraph{}, false
	}
	modBytes, err := safefs.ReadFile(worktreePath, "go.mod", 1<<20)
	if err != nil {
		return goModuleGraph{}, false
	}
	modPath := parseModulePath(string(modBytes))
	if modPath == "" {
		return goModuleGraph{}, false
	}
	graph := goModuleGraph{
		modulePath: modPath,
		packages:   make(map[string]bool),
		importers:  make(map[string]map[string]bool),
	}
	fset := token.NewFileSet()
	var walk func(relDir string) bool
	walk = func(relDir string) bool {
		absDir := worktreePath
		if relDir != "" {
			absDir = filepath.Join(worktreePath, filepath.FromSlash(relDir))
		}
		entries, err := os.ReadDir(absDir)
		if err != nil {
			return false
		}
		var goFiles []string
		for _, entry := range entries {
			name := entry.Name()
			if entry.Type()&os.ModeSymlink != 0 {
				return false
			}
			if entry.IsDir() {
				if name == ".git" || name == ".swarm" || goIgnoredDir(name) {
					continue
				}
				sub := name
				if relDir != "" {
					sub = relDir + "/" + name
				}
				if !walk(sub) {
					return false
				}
				continue
			}
			if relDir != "" && name == "go.mod" {
				return false
			}
			if strings.HasSuffix(name, ".go") && entry.Type().IsRegular() {
				goFiles = append(goFiles, name)
			}
		}
		if len(goFiles) == 0 {
			return true
		}
		pkgKey := "."
		if relDir != "" {
			pkgKey = "./" + relDir
		}
		graph.packages[pkgKey] = true
		for _, name := range goFiles {
			relFile := name
			if relDir != "" {
				relFile = relDir + "/" + name
			}
			src, err := safefs.ReadFile(worktreePath, filepath.FromSlash(relFile), 4<<20)
			if err != nil {
				return false
			}
			parsed, err := parser.ParseFile(fset, relFile, src, parser.ImportsOnly)
			if err != nil {
				return false
			}
			for _, impSpec := range parsed.Imports {
				imp := strings.Trim(impSpec.Path.Value, `"`)
				target := ""
				if imp == modPath {
					target = "."
				} else if sub, ok := strings.CutPrefix(imp, modPath+"/"); ok && sub != "" {
					target = "./" + sub
				}
				if target == "" || target == pkgKey {
					continue
				}
				if graph.importers[target] == nil {
					graph.importers[target] = make(map[string]bool)
				}
				graph.importers[target][pkgKey] = true
			}
		}
		return true
	}
	if !walk("") {
		return goModuleGraph{}, false
	}
	// Prune importer keys that do not correspond to discovered packages.
	for target := range graph.importers {
		if !graph.packages[target] {
			delete(graph.importers, target)
		}
	}
	return graph, len(graph.packages) > 0
}

func parseModulePath(goMod string) string {
	for _, line := range strings.Split(goMod, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`)
		}
	}
	return ""
}

var goCheckBoolFlags = map[string]bool{
	"-v": true, "-race": true, "-short": true, "-failfast": true, "-json": true,
	"-trimpath": true, "-a": true, "-n": true, "-x": true,
}

var goCheckValueFlags = map[string]bool{
	"-run": true, "-skip": true, "-count": true, "-timeout": true,
	"-parallel": true, "-p": true, "-cpu": true, "-shuffle": true,
	"-vet": true, "-tags": true, "-mod": true,
}

type shellToken struct {
	val    string
	quoted bool
}

func tokenizeSimpleCommand(cmd string) ([]shellToken, bool) {
	var tokens []shellToken
	var cur strings.Builder
	inToken := false
	quoted := false

	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		switch ch {
		case ' ', '\t', '\n', '\r':
			if inToken {
				tokens = append(tokens, shellToken{val: cur.String(), quoted: quoted})
				cur.Reset()
				inToken = false
				quoted = false
			}
		case '\'':
			inToken = true
			quoted = true
			i++
			for i < len(cmd) && cmd[i] != '\'' {
				cur.WriteByte(cmd[i])
				i++
			}
			if i >= len(cmd) {
				return nil, false
			}
		case '"', ';', '|', '&', '$', '`', '>', '<', '(', ')', '\\':
			return nil, false
		default:
			inToken = true
			cur.WriteByte(ch)
		}
	}
	if inToken {
		tokens = append(tokens, shellToken{val: cur.String(), quoted: quoted})
	}
	return tokens, len(tokens) > 0
}

func narrowGoCheckCommand(command string, packages, affected map[string]bool) (string, bool) {
	tokens, ok := tokenizeSimpleCommand(command)
	if !ok || len(tokens) < 3 || tokens[0].val != "go" || tokens[0].quoted {
		return command, true
	}
	start := 2
	switch tokens[1].val {
	case "build", "vet", "test":
	case "tool":
		if len(tokens) < 4 || tokens[2].val != "staticcheck" || tokens[2].quoted {
			return command, true
		}
		start = 3
	default:
		return command, true
	}

	hasQuotes := false
	for _, tok := range tokens {
		if tok.quoted {
			hasQuotes = true
			break
		}
	}

	var patternIndices []int
	var expandedAll []string
	expandedSet := make(map[string]bool)
	var matchedAffected []string
	matchedSet := make(map[string]bool)

	for i := start; i < len(tokens); i++ {
		tok := tokens[i]
		if !tok.quoted && strings.HasPrefix(tok.val, "-") {
			name, _, hasEq := strings.Cut(tok.val, "=")
			if hasEq {
				if !goCheckBoolFlags[name] && !goCheckValueFlags[name] {
					return command, true
				}
				continue
			}
			if goCheckBoolFlags[name] {
				continue
			}
			if goCheckValueFlags[name] {
				i++
				if i >= len(tokens) {
					return command, true
				}
				continue
			}
			return command, true
		}
		if tok.quoted {
			return command, true
		}
		expanded, ok := expandGoPattern(tok.val, packages)
		if !ok {
			return command, true
		}
		patternIndices = append(patternIndices, i)
		for _, pkg := range expanded {
			if !expandedSet[pkg] {
				expandedSet[pkg] = true
				expandedAll = append(expandedAll, pkg)
			}
			if affected[pkg] && !matchedSet[pkg] {
				matchedSet[pkg] = true
				matchedAffected = append(matchedAffected, pkg)
			}
		}
	}

	if len(patternIndices) == 0 {
		return command, true
	}
	if len(matchedAffected) == 0 {
		return "", false
	}
	if hasQuotes || len(matchedAffected) == len(expandedAll) {
		return command, true
	}

	sort.Strings(matchedAffected)
	var out []string
	inserted := false
	for i, tok := range tokens {
		if slices.Contains(patternIndices, i) {
			if !inserted {
				out = append(out, matchedAffected...)
				inserted = true
			}
			continue
		}
		out = append(out, tok.val)
	}
	return strings.Join(out, " "), true
}

func expandGoPattern(pattern string, packages map[string]bool) ([]string, bool) {
	if pattern == "./..." {
		out := make([]string, 0, len(packages))
		for pkg := range packages {
			out = append(out, pkg)
		}
		return out, true
	}
	if prefix, ok := strings.CutSuffix(pattern, "/..."); ok && strings.HasPrefix(prefix, "./") {
		var out []string
		for pkg := range packages {
			if pkg == prefix || strings.HasPrefix(pkg, prefix+"/") {
				out = append(out, pkg)
			}
		}
		return out, len(out) > 0
	}
	if pattern == "." || strings.HasPrefix(pattern, "./") {
		clean := "."
		if pattern != "." {
			clean = "./" + strings.TrimPrefix(path.Clean(pattern), "./")
		}
		if packages[clean] {
			return []string{clean}, true
		}
		return nil, false
	}
	return nil, false
}
