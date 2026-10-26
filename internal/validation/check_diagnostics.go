package validation

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"github.com/supernovae/swarmos/internal/process"
)

const diagnosticBytes = 16 << 10
const diagnosticLines = 60
const truncationMarker = "[swarm: output truncated]"

// EscapeDiagnostic neutralizes terminal controls using the progress display's
// ASCII Go quoting rules, without the enclosing quotes.
func EscapeDiagnostic(s string) string {
	q := strconv.QuoteToASCII(s)
	return q[1 : len(q)-1]
}

type checkCapture struct {
	mu             sync.Mutex
	raw            process.Output
	tail           []byte
	dropped        bool
	pending        []byte
	longLine       bool
	names          []string
	namesBytes     int
	namesTruncated bool
}

func (c *checkCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	_, _ = c.raw.Write(p)
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
			if !c.longLine {
				c.parseLine()
			}
			c.pending = c.pending[:0]
			c.longLine = false
		} else if !c.longLine {
			if len(c.pending) < diagnosticBytes {
				c.pending = append(c.pending, b)
			} else {
				c.longLine = true
				c.pending = c.pending[:0]
			}
		}
	}
	return n, nil
}

func (c *checkCapture) parseLine() {
	line := strings.TrimLeft(string(c.pending), " \t")
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

func (c *checkCapture) diagnostics() (string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.longLine && len(c.pending) > 0 {
		c.parseLine()
	}
	names := append([]string{}, c.names...)
	if c.namesTruncated {
		names = append(names, truncationMarker)
	}
	// Escape individual runes into indivisible tokens so truncation cannot split
	// either UTF-8 or a visible escape sequence.
	text := strings.TrimSuffix(string(c.tail), "\n")
	lines := strings.Split(text, "\n")
	dropped := c.dropped
	if len(lines) > diagnosticLines {
		lines = lines[len(lines)-diagnosticLines:]
		dropped = true
	}
	tokens := make([]string, 0, len(text))
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
	budget := diagnosticBytes
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
		if strings.Count(result, "\n") >= diagnosticLines-1 {
			result = result[strings.IndexByte(result, '\n')+1:]
		}
		result = truncationMarker + "\n" + result
	}
	return result, names
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
