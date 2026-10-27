package validation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/workspace"
)

const (
	// Larger selections fall back to the full check alone; the generated
	// command must stay well within shell argument limits.
	maxFastPassPackages = 200
	maxFastPassCommand  = 32 << 10
	maxDiscoveryOutput  = 16 << 20
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

// goSelection is the affected package list for one set of build flags.
type goSelection struct {
	packages []string
	reason   string // why no preliminary run happens, when packages is empty
	quiet    bool   // the reason is ordinary (no Go changes) and not reported
}

// goFastPass prepares and runs changed-package tests for one validation.
type goFastPass struct {
	runner   *checkRunner
	worktree string
	baseSHA  string
	// changed is computed once from the coordinator's own checkpoint diff.
	changed    []string
	tree       string
	changedErr error
	diffed     bool
	selections map[string]goSelection
	// note is a non-gating reason recorded in the report for the current check.
	note string
}

// run executes the changed-package pass before an eligible check. A non-nil
// outcome is recorded in the report. stop reports a genuine test failure (or
// cancellation of validation), after which the full check must not run.
func (f *goFastPass) run(ctx context.Context, check contract.CheckSpec, duration time.Duration, onProgress func(progress.Event)) (*CheckRunOutcome, bool, error) {
	cmd, ok := parseGoTestAll(check.Command)
	if !ok {
		return nil, false, nil
	}
	// Preparation has its own bound, so it can never use up the full check's time.
	prepCtx, cancel := context.WithTimeout(ctx, duration)
	selection := f.selection(prepCtx, cmd, duration)
	cancel()
	if len(selection.packages) == 0 {
		if !selection.quiet && ctx.Err() == nil {
			f.note = selection.reason
		}
		// After cancellation the full check fails at once without starting.
		return nil, false, nil
	}
	command := "go test"
	for _, flag := range cmd.flags {
		command += " " + flag
	}
	for _, pkg := range selection.packages {
		command += " '" + pkg + "'"
	}
	name := check.Name + " (changed packages)"
	fastCtx, cancelFast := context.WithTimeout(ctx, duration)
	defer cancelFast()
	if onProgress != nil {
		onProgress(progress.Event{Check: name, Status: "started"})
	}
	start := time.Now()
	var output checkCapture
	runErr := f.runner.run(fastCtx, command, duration, &output, &output)
	outcome := &CheckRunOutcome{Name: name, Command: command, Duration: time.Since(start), Output: output.raw.String(), Passed: runErr == nil, Preliminary: true, Packages: selection.packages}
	status := "PASS"
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case ctx.Err() != nil:
		// Validation itself was cancelled or ran out of time: stop without
		// starting the full run.
		outcome.OutputTail, outcome.FailingTests = output.diagnostics()
		outcome.InfrastructureError, outcome.ExitCode, status = true, 1, "FAIL"
		runErr = fmt.Errorf("%w (validation cancelled)", runErr)
	case fastCtx.Err() != nil:
		// A killed process also returns *exec.ExitError, so check the
		// local deadline first.
		outcome.FallbackReason, status = "timed out; the full check decides", "skipped"
	case errors.As(runErr, &exitErr):
		outcome.OutputTail, outcome.FailingTests = output.diagnostics()
		outcome.goFailures = output.evidence(check.Command)
		outcome.ExitCode, status = exitErr.ExitCode(), "FAIL"
	default:
		outcome.FallbackReason, status = "did not run: "+EscapeDiagnostic(runErr.Error())+"; the full check decides", "skipped"
	}
	if onProgress != nil {
		onProgress(progress.Event{Check: name, Status: status, StageElapsed: outcome.Duration})
	}
	return outcome, status == "FAIL", runErr
}

// selection returns the packages to test first for these build flags.
func (f *goFastPass) selection(ctx context.Context, cmd goTestCommand, duration time.Duration) goSelection {
	if !f.diffed {
		f.tree, f.changed, f.changedErr = f.changedFiles(ctx)
		// Retry on a later check only if preparation was cut short.
		f.diffed = ctx.Err() == nil
	}
	if f.changedErr != nil {
		return goSelection{reason: "could not compute changed files: " + EscapeDiagnostic(f.changedErr.Error())}
	}
	key := strings.Join(cmd.buildFlags, " ")
	if selection, ok := f.selections[key]; ok {
		return selection
	}
	selection := f.select1(ctx, cmd, duration)
	if ctx.Err() == nil {
		if f.selections == nil {
			f.selections = map[string]goSelection{}
		}
		f.selections[key] = selection
	}
	return selection
}

// changedFiles diffs the trusted base against a coordinator-built checkpoint
// tree, including uncommitted edits, without consulting agent output.
func (f *goFastPass) changedFiles(ctx context.Context) (string, []string, error) {
	tree, err := workspace.CheckpointTree(ctx, f.worktree)
	if err != nil {
		return "", nil, err
	}
	out, err := gitOutputContext(ctx, f.worktree, "diff", "--name-only", "-z", "--no-renames", f.baseSHA, tree, "--")
	if err != nil {
		return "", nil, err
	}
	var files []string
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) > 0 {
			files = append(files, string(raw))
		}
	}
	return tree, files, nil
}

func (f *goFastPass) select1(ctx context.Context, cmd goTestCommand, duration time.Duration) goSelection {
	var dirs []string
	for _, file := range f.changed {
		switch path.Base(file) {
		case "go.mod", "go.sum", "go.work", "go.work.sum":
			return goSelection{reason: "module configuration changed"}
		}
		if strings.HasSuffix(file, ".go") && !goIgnoredDir(path.Dir(file)) {
			dirs = append(dirs, path.Dir(file))
		}
	}
	if len(dirs) == 0 {
		return goSelection{reason: "no Go packages changed", quiet: true}
	}
	// A single module at the repository root is the only supported layout.
	if _, err := gitOutputContext(ctx, f.worktree, "cat-file", "-e", f.tree+":go.mod"); err != nil {
		return goSelection{reason: "no go.mod at the repository root"}
	}
	if _, err := gitOutputContext(ctx, f.worktree, "cat-file", "-e", f.tree+":go.work"); err == nil {
		return goSelection{reason: "go.work workspaces are not supported"}
	}
	discovery := "go list -e -json=ImportPath,Dir,Module,Imports,TestImports,XTestImports"
	for _, flag := range cmd.buildFlags {
		discovery += " " + flag
	}
	discovery += " ./..."
	// Keep stderr separate so diagnostics cannot corrupt the JSON stream.
	out, stderr := boundedBuffer{limit: maxDiscoveryOutput}, boundedBuffer{limit: diagnosticBytes}
	if err := f.runner.run(ctx, discovery, duration, &out, &stderr); err != nil {
		return goSelection{reason: "package discovery failed: " + EscapeDiagnostic(err.Error())}
	}
	if out.overflow {
		return goSelection{reason: "package discovery output too large"}
	}
	packages, err := parseGoList(out.buf.Bytes())
	if err != nil {
		return goSelection{reason: "package discovery: " + EscapeDiagnostic(err.Error())}
	}
	selected, err := affectedPackages(packages, dirs)
	if err != nil {
		return goSelection{reason: EscapeDiagnostic(err.Error())}
	}
	if len(selected) == len(packages) {
		return goSelection{reason: "every package is affected", quiet: true}
	}
	if !fastPassFits(cmd.flags, selected) {
		return goSelection{reason: fmt.Sprintf("%d affected packages exceed the preliminary limit", len(selected))}
	}
	return goSelection{packages: selected}
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
