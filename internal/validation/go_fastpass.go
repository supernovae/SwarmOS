package validation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
)

const (
	// Larger selections fall back to the full check alone; the generated
	// command must stay well within shell argument limits.
	maxFastPassPackages = 200
	maxFastPassCommand  = 32 << 10
)

// goTestFlags are accepted in a recognized `go test ./...` command. Build
// flags must also be passed to package discovery, so that `go list` sees the
// same files as the test run; the remaining flags only affect test execution.
var goTestFlags = map[string]struct{ value, build bool }{
	"-v": {}, "-short": {}, "-failfast": {}, "-json": {},
	"-race": {build: true}, "-trimpath": {build: true},
	"-count": {value: true}, "-run": {value: true}, "-skip": {value: true}, "-timeout": {value: true},
	"-shuffle": {value: true}, "-parallel": {value: true}, "-cpu": {value: true}, "-p": {value: true}, "-vet": {value: true},
	"-tags": {value: true, build: true}, "-mod": {value: true, build: true},
}

// goTestCommand is a recognized required check of the form `go test [flags] ./...`.
type goTestCommand struct {
	flags      []string // all flags, in their original order
	buildFlags []string // flags that change which files are built
}

// parseGoTestAll recognizes a direct `go test` over `./...`. It is deliberately
// narrow: shell syntax, wrappers, quoting, other package patterns, and flags
// that take a separate value argument are never rewritten.
func parseGoTestAll(command string) (goTestCommand, bool) {
	var cmd goTestCommand
	for _, r := range command {
		if !safeShellRune(r) && r != ' ' && r != '\t' {
			return cmd, false
		}
	}
	fields := strings.Fields(command)
	if len(fields) < 3 || fields[0] != "go" || fields[1] != "test" {
		return cmd, false
	}
	pattern := 0
	for _, field := range fields[2:] {
		if field == "./..." {
			pattern++
			continue
		}
		name, value, hasValue := strings.Cut(field, "=")
		flag, ok := goTestFlags[name]
		if !ok || flag.value != hasValue || (hasValue && value == "") {
			return cmd, false
		}
		cmd.flags = append(cmd.flags, field)
		if flag.build {
			cmd.buildFlags = append(cmd.buildFlags, field)
		}
	}
	return cmd, pattern == 1
}

// safeShellRune reports whether r needs no quoting in a POSIX shell word.
func safeShellRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.,:/+=", r)
}

// fastPassFits bounds the generated command's package count and length.
func fastPassFits(flags, packages []string) bool {
	length := len("go test ") + len(strings.Join(flags, " "))
	for _, pkg := range packages {
		length += len(pkg) + 3
	}
	return len(packages) <= maxFastPassPackages && length <= maxFastPassCommand
}

// goIgnoredDir reports directories that `./...` never matches.
func goIgnoredDir(dir string) bool {
	for _, part := range strings.Split(dir, "/") {
		if part == "testdata" || part == "vendor" || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return true
		}
	}
	return false
}

// goPackage is a package from `go list -json`, with Dir relative to the module root.
type goPackage struct {
	ImportPath   string
	Dir          string
	Imports      []string
	TestImports  []string
	XTestImports []string
	Module       *struct {
		Path string
		Dir  string
		Main bool
	}
}

// parseGoList reads `go list -json` output, keeping only main-module packages
// rooted at the module directory. It treats the output as untrusted.
func parseGoList(data []byte) ([]goPackage, error) {
	var packages []goPackage
	var modulePath, moduleDir string
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var pkg goPackage
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("invalid package metadata: %w", err)
		}
		if pkg.Module == nil || !pkg.Module.Main {
			return nil, errors.New("package outside the main module")
		}
		if modulePath == "" {
			modulePath, moduleDir = pkg.Module.Path, pkg.Module.Dir
		}
		if pkg.Module.Path != modulePath || pkg.Module.Dir != moduleDir {
			return nil, errors.New("more than one module")
		}
		if !validImportPath(pkg.ImportPath) || (pkg.ImportPath != modulePath && !strings.HasPrefix(pkg.ImportPath, modulePath+"/")) {
			return nil, fmt.Errorf("unexpected package %q", pkg.ImportPath)
		}
		rel, err := filepath.Rel(moduleDir, pkg.Dir)
		if err != nil || !filepath.IsLocal(rel) && rel != "." {
			return nil, fmt.Errorf("package %q outside the module root", pkg.ImportPath)
		}
		pkg.Dir = filepath.ToSlash(rel)
		packages = append(packages, pkg)
	}
	if len(packages) == 0 {
		return nil, errors.New("no packages")
	}
	return packages, nil
}

// validImportPath accepts only paths that are safe as a quoted shell word and
// cannot be read as a flag.
func validImportPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "-") || strings.HasPrefix(p, "/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	for _, r := range p {
		if !safeShellRune(r) && r != '~' {
			return false
		}
	}
	return true
}

// affectedPackages maps changed directories to packages and adds every
// package whose code or tests import one of them, directly or transitively.
// The result is sorted. A changed directory that is no longer a package (for
// example a deleted one) cannot be resolved, so it is an error.
func affectedPackages(packages []goPackage, dirs []string) ([]string, error) {
	byDir := make(map[string]string, len(packages))
	importers := make(map[string][]string)
	for _, pkg := range packages {
		byDir[pkg.Dir] = pkg.ImportPath
		for _, list := range [][]string{pkg.Imports, pkg.TestImports, pkg.XTestImports} {
			for _, imported := range list {
				if imported != pkg.ImportPath {
					importers[imported] = append(importers[imported], pkg.ImportPath)
				}
			}
		}
	}
	seen := map[string]bool{}
	var queue []string
	for _, dir := range dirs {
		pkg, ok := byDir[dir]
		if !ok {
			return nil, fmt.Errorf("changed directory %q is not a package in this module", dir)
		}
		if !seen[pkg] {
			seen[pkg] = true
			queue = append(queue, pkg)
		}
	}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		for _, importer := range importers[pkg] {
			if !seen[importer] {
				seen[importer] = true
				queue = append(queue, importer)
			}
		}
	}
	selected := make([]string, 0, len(seen))
	for pkg := range seen {
		selected = append(selected, pkg)
	}
	slices.Sort(selected)
	return selected, nil
}

// boundedBuffer keeps at most limit bytes and records any overflow.
type boundedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.buf.Len(); n > room {
		b.overflow = true
		p = p[:max(room, 0)]
	}
	b.buf.Write(p)
	return n, nil
}

// goCapture recognizes only approved direct all-package Go commands. OCI
// retains its existing command and output behavior.
func (r *checkRunner) goCapture(command string) (string, *checkCapture) {
	c := &checkCapture{}
	if !r.runnerIsolated && r.execution.Mode == contract.ExecutionIsolated && r.execution.Backend != contract.BackendNative {
		return command, c
	}
	parsed, ok := parseGoTestAll(command)
	if !ok {
		return command, c
	}
	c.jsonMode = true
	c.preserveJSON = slices.Contains(parsed.flags, "-json")
	if !c.preserveJSON {
		command = "go test -json " + strings.Join(strings.Fields(command)[2:], " ")
	}
	return command, c
}
