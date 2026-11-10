package validation

import (
	"bytes"
	"slices"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
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

// goIgnoredDir reports directories that `./...` never matches.
func goIgnoredDir(dir string) bool {
	for _, part := range strings.Split(dir, "/") {
		if part == "testdata" || part == "vendor" || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return true
		}
	}
	return false
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
