package validation

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/supernovae/swarmos/internal/process"
)

const diagnosticBytes = 16 << 10
const diagnosticLines = 60
const truncationMarker = "[swarm: output truncated]"

// Go failure blocks are shown ahead of the plain tail, inside the shared
// bounds above, so trailing noise cannot hide why tests failed.
const failBlockLines = 8
const failBlockBytes = 2 << 10
const failSectionLines = 40
const failSectionBytes = 12 << 10

// EscapeDiagnostic neutralizes terminal controls using the progress display's
// ASCII Go quoting rules, without the enclosing quotes.
func EscapeDiagnostic(s string) string {
	q := strconv.QuoteToASCII(s)
	return q[1 : len(q)-1]
}

type checkCapture struct {
	finished       bool
	compiler       []string
	compilerBytes  int
	jsonMode       bool
	decodingEvent  bool
	preserveJSON   bool
	jsonPending    []byte
	jsonLong       bool
	skips          []SkippedTest
	skipTotal      int
	contexts       map[string]string
	jsonFailures   map[goFailure]*jsonFailure
	buildFailures  map[string]bool
	summaries      map[string]bool
	mu             sync.Mutex
	raw            process.Output
	tail           []byte
	dropped        bool
	pending        []byte
	longLine       bool
	longIndented   bool
	lineNo         int
	fails          failSection
	names          []string
	namesBytes     int
	namesTruncated bool
	// goTests attributes raw failing test names to the package summary line
	// that follows them in plain `go test` output.
	goTests        []goFailure
	pendingTests   []string
	unattributed   []string
	testsTruncated bool
}

// goFailure is one failing Go test and the package whose summary line
// reported it. Both are raw, untrusted output and must be escaped for display.
type goFailure struct {
	pkg, name string
}

// maxGoFailures bounds the attributed failures kept for repair diagnosis.
const maxGoFailures = 60

func (c *checkCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if c.jsonMode {
		if c.preserveJSON {
			_, _ = c.raw.Write(p)
		}
		for _, b := range p {
			if b == '\n' {
				c.eventLine()
				c.jsonPending = nil
				c.jsonLong = false
			} else if !c.jsonLong {
				if len(c.jsonPending) < 1<<20 {
					c.jsonPending = append(c.jsonPending, b)
				} else {
					c.jsonLong = true
					c.jsonPending = nil
				}
			}
		}
		return n, nil
	}
	c.readable(p)
	return n, nil
}

func (c *checkCapture) readable(p []byte) {
	if !c.preserveJSON {
		_, _ = c.raw.Write(p)
	}
	if len(p) >= diagnosticBytes {
		c.dropped = c.dropped || len(c.tail)+len(p) > diagnosticBytes
		c.tail = append(c.tail[:0], p[len(p)-diagnosticBytes:]...)
	} else {
		if excess := len(c.tail) + len(p) - diagnosticBytes; excess > 0 {
			c.tail = append(c.tail[:0], c.tail[excess:]...)
			c.dropped = true
		}
		c.tail = append(c.tail, p...)
	}
	for _, b := range p {
		if b == '\n' {
			if c.longLine {
				if !c.decodingEvent {
					c.fails.overlong(c.longIndented)
				}
			} else {
				line := string(c.pending)
				if !c.decodingEvent {
					c.fails.line(line, c.lineNo)
				}
				c.parseName(line)
				if !c.jsonMode || !c.decodingEvent {
					c.goLine(line)
				}
			}
			c.pending = c.pending[:0]
			c.longLine = false
			c.lineNo++
		} else if !c.longLine {
			if len(c.pending) < diagnosticBytes {
				c.pending = append(c.pending, b)
			} else {
				c.longLine = true
				c.longIndented = isIndented(string(c.pending))
				c.pending = c.pending[:0]
			}
		}
	}
}

func (c *checkCapture) parseName(line string) {
	line = strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(line, "--- FAIL: ") {
		return
	}
	fields := strings.Fields(strings.TrimPrefix(line, "--- FAIL: "))
	if len(fields) == 0 {
		return
	}
	name := EscapeDiagnostic(fields[0])
	for _, existing := range c.names {
		if existing == name {
			return
		}
	}
	if len(c.names) >= 60 || c.namesBytes+len(name) > diagnosticBytes-64 {
		c.namesTruncated = true
		return
	}
	c.names = append(c.names, name)
	c.namesBytes += len(name)
}

// goLine collects raw `--- FAIL:` names and assigns them to the following
// `FAIL\t<pkg>\t...` summary line. A failed package without named tests, such
// as a build failure or a panic in TestMain, is recorded as unattributed.
func (c *checkCapture) goLine(line string) {
	if trimmed := strings.TrimLeft(line, " \t"); strings.HasPrefix(trimmed, "--- FAIL: ") {
		fields := strings.Fields(strings.TrimPrefix(trimmed, "--- FAIL: "))
		switch {
		case len(fields) == 0, slices.Contains(c.pendingTests, fields[0]):
		case len(c.pendingTests) >= maxGoFailures:
			c.testsTruncated = true
		default:
			c.pendingTests = append(c.pendingTests, fields[0])
		}
		return
	}
	switch {
	case strings.HasPrefix(line, "ok  \t"), strings.HasPrefix(line, "?   \t"):
		c.pendingTests = c.pendingTests[:0]
	case strings.HasPrefix(line, "FAIL\t"):
		pkg, _, _ := strings.Cut(strings.TrimPrefix(line, "FAIL\t"), "\t")
		pkg, _, _ = strings.Cut(pkg, " ")
		if pkg == "" {
			return
		}
		if len(c.pendingTests) == 0 {
			if len(c.unattributed) < maxGoFailures {
				c.unattributed = append(c.unattributed, pkg)
			} else {
				c.testsTruncated = true
			}
		}
		for _, name := range c.pendingTests {
			if len(c.goTests) >= maxGoFailures {
				c.testsTruncated = true
				break
			}
			c.goTests = append(c.goTests, goFailure{pkg: pkg, name: name})
		}
		c.pendingTests = c.pendingTests[:0]
	}
}

// goFailures returns the attributed failing tests, the packages that failed
// without a named test, and whether either list was cut.
// An unterminated final line is parsed on a copy, as in diagnostics.
func (c *checkCapture) goFailures() ([]goFailure, []string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tmp := checkCapture{goTests: slices.Clone(c.goTests), pendingTests: slices.Clone(c.pendingTests), unattributed: slices.Clone(c.unattributed), testsTruncated: c.testsTruncated || c.longLine}
	if !c.longLine && len(c.pending) > 0 {
		tmp.goLine(string(c.pending))
	}
	return tmp.goTests, tmp.unattributed, tmp.testsTruncated || len(tmp.pendingTests) > 0
}

// evidence packages goFailures for a failed run of the trusted command source.
func (c *checkCapture) evidence(source string) *goEvidence {
	tests, unattributed, truncated := c.goFailures()
	return &goEvidence{source: source, tests: tests, unattributed: unattributed, truncated: truncated}
}

func (c *checkCapture) diagnostics() (string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Finish an unterminated final line on a copy so repeated calls, and later
	// writes, see the same capture state.
	fails := c.fails.clone()
	partial := c.longLine || len(c.pending) > 0
	if c.longLine {
		fails.overlong(c.longIndented)
	} else if len(c.pending) > 0 {
		line := string(c.pending)
		fails.line(line, c.lineNo)
		c.parseName(line)
	}
	fails.close()
	names := append([]string{}, c.names...)
	if c.namesTruncated {
		names = append(names, truncationMarker)
	}
	text := strings.TrimSuffix(string(c.tail), "\n")
	lines := strings.Split(text, "\n")
	result, cut := renderTail(lines, c.dropped, diagnosticLines, diagnosticBytes)
	if !cut || len(fails.blocks) == 0 {
		return result, names
	}
	// The tail would lose output, so show the failure blocks first and fill
	// the remaining budget with the tail lines they do not already show.
	section, sectionLines := fails.render()
	last := c.lineNo
	if !partial {
		last--
	}
	var rest []string
	for i, line := range lines {
		if !fails.covers(last - (len(lines) - 1 - i)) {
			rest = append(rest, line)
		}
	}
	if len(rest) == 0 {
		return section, names
	}
	tail, _ := renderTail(rest, c.dropped, diagnosticLines-sectionLines, diagnosticBytes-len(section)-1)
	if tail == "" {
		return section, names
	}
	return section + "\n" + tail, names
}

// renderTail keeps the final lines within maxLines and maxBytes, prefixing a
// truncation marker when anything is cut. It reports whether it cut output.
func renderTail(lines []string, dropped bool, maxLines, maxBytes int) (string, bool) {
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		dropped = true
	}
	// Escape individual runes into indivisible tokens so truncation cannot split
	// either UTF-8 or a visible escape sequence.
	var tokens []string
	size := 0
	for i, line := range lines {
		if i > 0 {
			tokens = append(tokens, "\n")
			size++
		}
		for _, r := range line {
			token := EscapeDiagnostic(string(r))
			tokens = append(tokens, token)
			size += len(token)
		}
	}
	budget := maxBytes
	if dropped || size > budget {
		dropped = true
		budget -= len(truncationMarker) + 1
	}
	start := 0
	for size > budget {
		size -= len(tokens[start])
		start++
		dropped = true
	}
	result := strings.Join(tokens[start:], "")
	if dropped {
		// The marker occupies one of the rendered lines.
		if strings.Count(result, "\n") >= maxLines-1 {
			result = result[strings.IndexByte(result, '\n')+1:]
		}
		result = truncationMarker + "\n" + result
	}
	return result, dropped
}

// failBlock is one escaped Go "--- FAIL:" header and the indented message
// lines that follow it, starting at stream line first.
type failBlock struct {
	lines     []string
	first     int
	size      int
	cut       bool
	synthetic bool
}

// failSection collects the earliest failure blocks that fit the section
// budget while output streams. Plain output supplies header-before-message blocks.
// JSON output supplies messages before the header; eventLine buffers their prefix
// per package/test and assembles a header-first block at the named fail event.
type failSection struct {
	blocks  []failBlock
	cur     failBlock
	open    bool
	lines   int
	size    int
	omitted bool
}

func isIndented(line string) bool {
	return line != "" && (line[0] == ' ' || line[0] == '\t')
}

// escapeWithin escapes s rune by rune, stopping before budget is exceeded.
func escapeWithin(s string, budget int) (string, bool) {
	var b strings.Builder
	for _, r := range s {
		token := EscapeDiagnostic(string(r))
		if b.Len()+len(token) > budget {
			return b.String(), true
		}
		b.WriteString(token)
	}
	return b.String(), false
}

func (f *failSection) line(line string, n int) {
	if strings.HasPrefix(strings.TrimLeft(line, " \t"), "--- FAIL: ") {
		f.close()
		if f.omitted {
			return
		}
		header, cut := escapeWithin(line, failBlockBytes-len(truncationMarker)-1)
		f.cur = failBlock{lines: []string{header}, first: n, size: len(header), cut: cut}
		f.open = true
		return
	}
	if !f.open || !isIndented(line) {
		f.close()
		return
	}
	if f.cur.cut {
		return
	}
	message, cut := escapeWithin(line, failBlockBytes-f.cur.size-1)
	if cut || len(f.cur.lines) >= failBlockLines {
		f.cur.cut = true
		return
	}
	f.cur.lines = append(f.cur.lines, message)
	f.cur.size += len(message) + 1
}

// overlong handles a line too long to parse, which is omitted from its block.
func (f *failSection) overlong(indented bool) {
	if f.open && indented {
		f.cur.cut = true
	} else {
		f.close()
	}
}

func (f *failSection) close() {
	if !f.open {
		return
	}
	f.open = false
	b := f.cur
	f.cur = failBlock{}
	if b.cut {
		// Make room for the block's own marker; the header always fits.
		for len(b.lines) > 1 && (len(b.lines)+1 > failBlockLines || b.size+1+len(truncationMarker) > failBlockBytes) {
			b.size -= len(b.lines[len(b.lines)-1]) + 1
			b.lines = b.lines[:len(b.lines)-1]
		}
	}
	lines, size := b.rendered()
	if len(f.blocks) > 0 {
		size++
	}
	// Leave room for the section marker in case later blocks are omitted.
	if f.lines+lines+1 > failSectionLines || f.size+size+1+len(truncationMarker) > failSectionBytes {
		f.omitted = true
		return
	}
	f.blocks = append(f.blocks, b)
	f.lines += lines
	f.size += size
}

func (b failBlock) rendered() (int, int) {
	if b.cut {
		return len(b.lines) + 1, b.size + 1 + len(truncationMarker)
	}
	return len(b.lines), b.size
}

func (f failSection) clone() failSection {
	f.blocks = slices.Clone(f.blocks)
	f.cur.lines = slices.Clone(f.cur.lines)
	return f
}

func (f *failSection) covers(n int) bool {
	for _, b := range f.blocks {
		if !b.synthetic && n >= b.first && n < b.first+len(b.lines) {
			return true
		}
	}
	return false
}

// render returns the section text and its line count.
func (f *failSection) render() (string, int) {
	var parts []string
	for _, b := range f.blocks {
		parts = append(parts, b.lines...)
		if b.cut {
			parts = append(parts, truncationMarker)
		}
	}
	if f.omitted {
		parts = append(parts, truncationMarker)
	}
	return strings.Join(parts, "\n"), len(parts)
}

func (c CheckRunOutcome) MarshalJSON() ([]byte, error) {
	type alias CheckRunOutcome
	if c.Passed {
		c.OutputTail = ""
		c.FailingTests = nil
		return json.Marshal(alias(c))
	}
	names := c.FailingTests
	if names == nil {
		names = []string{}
	}
	return json.Marshal(struct {
		alias
		OutputTail   string   `json:"output_tail"`
		FailingTests []string `json:"failing_tests"`
	}{alias(c), c.OutputTail, names})
}

// SkippedTest is bounded evidence from a trusted required Go test stream.
type SkippedTest struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func boundedSkip(s string) string {
	if len(s) > 2048 {
		return s[:2048] + truncationMarker
	}
	return s
}

func (c *checkCapture) eventLine() {
	if c.jsonLong {
		c.readable([]byte(truncationMarker + "\n"))
		return
	}
	var e struct{ Action, Package, Test, Output, ImportPath, FailedBuild string }
	if json.Unmarshal(c.jsonPending, &e) != nil || e.Action == "" {
		c.readable(append(slices.Clone(c.jsonPending), '\n'))
		return
	}
	if e.Output != "" {
		c.decodingEvent = true
		c.readable([]byte(e.Output))
		c.decodingEvent = false
		if e.Action == "build-output" {
			for _, line := range strings.Split(strings.TrimSuffix(e.Output, "\n"), "\n") {
				if len(c.compiler) < 8 && c.compilerBytes < 2048 {
					line, _ = escapeWithin(line, 2048-c.compilerBytes)
					c.compiler = append(c.compiler, line)
					c.compilerBytes += len(line)
				}
			}
		}
	}
	if c.buildFailures == nil {
		c.buildFailures = map[string]bool{}
		c.summaries = map[string]bool{}
	}
	if e.Action == "build-fail" && len(c.buildFailures) < 60 {
		c.buildFailures[boundedSkip(e.ImportPath)] = true
	}
	if e.Action == "fail" && e.Test == "" && e.FailedBuild != "" && len(c.buildFailures) < 60 {
		delete(c.buildFailures, boundedSkip(e.FailedBuild))
		c.buildFailures[boundedSkip(e.Package)] = true
	}
	if strings.HasPrefix(e.Output, "FAIL\t") {
		pkg, _, _ := strings.Cut(strings.TrimPrefix(e.Output, "FAIL\t"), "\t")
		pkg, _, _ = strings.Cut(pkg, " ")
		if len(c.summaries) < 60 {
			c.summaries[boundedSkip(pkg)] = true
		}
	}
	if e.Action == "fail" && e.Package != "" {
		if e.Test != "" {
			if len(c.goTests) < maxGoFailures {
				c.goTests = append(c.goTests, goFailure{pkg: e.Package, name: e.Test})
			} else {
				c.testsTruncated = true
			}
		} else {
			named := false
			for _, test := range c.goTests {
				if test.pkg == e.Package {
					named = true
					break
				}
			}
			if !named && !slices.Contains(c.unattributed, e.Package) && len(c.unattributed) < maxGoFailures {
				c.unattributed = append(c.unattributed, e.Package)
			}
		}
	}
	if e.Package == "" || e.Test == "" {
		return
	}
	failureKey := goFailure{pkg: boundedSkip(e.Package), name: boundedSkip(e.Test)}
	if c.jsonFailures == nil {
		c.jsonFailures = map[goFailure]*jsonFailure{}
	}
	if e.Output != "" && (len(c.jsonFailures) < 120 || c.jsonFailures[failureKey] != nil) {
		prefix := c.jsonFailures[failureKey]
		if prefix == nil {
			prefix = &jsonFailure{}
			c.jsonFailures[failureKey] = prefix
		}
		prefix.output(e.Output)
	}
	defer func() {
		switch e.Action {
		case "pass", "skip", "fail":
			delete(c.jsonFailures, failureKey)
		}
	}()
	key := boundedSkip(e.Package) + "\x00" + boundedSkip(e.Test)
	if c.contexts == nil {
		c.contexts = map[string]string{}
	}
	if e.Output != "" && (len(c.contexts) < 120 || c.contexts[key] != "") {
		text := c.contexts[key] + e.Output
		if len(text) > 2048 {
			text = text[len(text)-2048:]
		}
		c.contexts[key] = text
	}
	switch e.Action {
	case "skip":
		c.skipTotal++
		if len(c.skips) < 60 {
			c.skips = append(c.skips, SkippedTest{Package: boundedSkip(e.Package), Test: boundedSkip(e.Test), Message: strings.TrimSpace(c.contexts[key]), Status: "unknown"})
		}
		delete(c.contexts, key)
	case "fail":
		if prefix := c.jsonFailures[failureKey]; prefix != nil {
			c.fails.jsonBlock(prefix)
		}
		delete(c.contexts, key)
	case "pass":
		delete(c.contexts, key)
	}
}

// finish is called after process completion, before any output is read.
func (c *checkCapture) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.jsonMode || c.finished {
		return
	}
	c.finished = true
	if len(c.jsonPending) > 0 || c.jsonLong {
		c.eventLine()
		c.jsonPending = nil
		c.jsonLong = false
	}
	if len(c.compiler) > 0 {
		c.fails.close()
		c.fails.blocks = append(c.fails.blocks, failBlock{lines: c.compiler, first: -100, size: c.compilerBytes})
		c.fails.lines += len(c.compiler)
		c.fails.size += c.compilerBytes
	}
	for pkg := range c.buildFailures {
		if pkg != "" && !slices.Contains(c.unattributed, pkg) && len(c.unattributed) < maxGoFailures {
			c.unattributed = append(c.unattributed, pkg)
		}
		if pkg != "" && !c.summaries[pkg] {
			c.decodingEvent = true
			c.readable([]byte("FAIL\t" + pkg + " [build failed]\n"))
			c.decodingEvent = false
			c.summaries[pkg] = true
		}
	}
}

// jsonFailure retains the earliest message lines independently of the skip tail.
type jsonFailure struct {
	header   string
	messages []string
	size     int
	cut      bool
}

func (p *jsonFailure) output(output string) {
	for line := range strings.SplitSeq(strings.TrimSuffix(output, "\n"), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "--- FAIL: ") {
			p.header = line[:min(len(line), failBlockBytes)]
			continue
		}
		if p.cut || !isIndented(line) || strings.HasPrefix(trimmed, "--- ") || strings.HasPrefix(trimmed, "=== ") {
			continue
		}
		if len(p.messages) >= failBlockLines || p.size >= failBlockBytes {
			p.cut = true
			continue
		}
		n := min(len(line), failBlockBytes-p.size)
		p.messages = append(p.messages, line[:n])
		p.size += n
		p.cut = n < len(line)
	}
}

func (f *failSection) jsonBlock(p *jsonFailure) {
	f.close()
	if p.header == "" || f.omitted {
		return
	}
	f.line(p.header, 0)
	f.cur.synthetic = true
	for _, line := range p.messages {
		if len(f.cur.lines) >= failBlockLines-1 {
			f.cur.cut = true
			break
		}
		message, cut := escapeWithin(line, failBlockBytes-f.cur.size-2-len(truncationMarker))
		if message != "" {
			f.cur.lines = append(f.cur.lines, message)
			f.cur.size += len(message) + 1
		}
		if cut {
			f.cur.cut = true
			break
		}
	}
	f.cur.cut = f.cur.cut || p.cut
	f.close()
}
