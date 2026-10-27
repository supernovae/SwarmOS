package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// Session-log collection limits. Any breach makes every log-derived value
// unavailable, so a partial total is never presented as complete.
const (
	telemetryMaxEntries   = 128
	telemetryMaxDepth     = 16
	telemetryMaxFiles     = 16
	telemetryMaxBytes     = 4 << 20
	telemetryMaxLineBytes = 256 << 10
)

var errTelemetryUnavailable = errors.New("session telemetry unavailable")

// sessionUsage is the scalar result of reading one run's harness session logs.
// Nil fields were not reported. Raw records, paths and commands never leave it.
type sessionUsage struct {
	input, output, cached, toolCalls, repeated *int64
	model, effort                              *string
}

func (u sessionUsage) apply(t *contract.RoleRunTelemetry) {
	t.InputTokens, t.OutputTokens, t.CachedInputTokens = u.input, u.output, u.cached
	t.ToolCalls, t.RepeatedCommands = u.toolCalls, u.repeated
	t.ActualModel, t.Effort = u.model, u.effort
	for _, v := range []any{u.input, u.output, u.cached, u.toolCalls, u.repeated, u.model, u.effort} {
		switch v := v.(type) {
		case *int64:
			if v != nil {
				t.UsageSource = contract.UsageSourceSessionLog
			}
		case *string:
			if v != nil {
				t.UsageSource = contract.UsageSourceSessionLog
			}
		}
	}
}

// sessionLogDir is where each supported harness keeps its own session log,
// relative to the run's scratch HOME. Other harnesses report nothing.
func sessionLogDir(driver string) string {
	switch driver {
	case "claude":
		return filepath.Join(".claude", "projects")
	case "codex":
		return filepath.Join(".codex", "sessions")
	}
	return ""
}

// collectSessionUsage reads only the run's scratch HOME, never the host HOME.
// Absent, unreadable, oversized, malformed or ambiguous logs yield no values.
func collectSessionUsage(driver, home string) sessionUsage {
	dir := sessionLogDir(driver)
	if dir == "" || home == "" {
		return sessionUsage{}
	}
	root, err := safefs.Open(home)
	if err != nil {
		return sessionUsage{}
	}
	defer root.Close()
	lines, err := readSessionLogs(root, dir)
	if err != nil {
		return sessionUsage{}
	}
	var usage sessionUsage
	switch driver {
	case "claude":
		usage, err = parseClaudeSession(lines)
	case "codex":
		usage, err = parseCodexSession(lines)
	}
	if err != nil {
		return sessionUsage{}
	}
	return usage
}

// readSessionLogs returns each .jsonl file's lines in path order. Symlinks
// and special files anywhere in the tree make the whole result unavailable.
func readSessionLogs(root *safefs.Root, dir string) ([][]string, error) {
	var files []string
	entries := 0
	var walk func(string, int) error
	walk = func(name string, depth int) error {
		if depth > telemetryMaxDepth {
			return errTelemetryUnavailable
		}
		f, err := root.OpenDirectory(name)
		if err != nil {
			return errTelemetryUnavailable
		}
		list, err := f.ReadDir(telemetryMaxEntries - entries + 1)
		f.Close()
		if err != nil && err != io.EOF {
			return errTelemetryUnavailable
		}
		entries += len(list)
		if entries > telemetryMaxEntries {
			return errTelemetryUnavailable
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name() < list[j].Name() })
		for _, entry := range list {
			child := filepath.Join(name, entry.Name())
			switch {
			case entry.Type()&fs.ModeSymlink != 0:
				return errTelemetryUnavailable
			case entry.IsDir():
				if err := walk(child, depth+1); err != nil {
					return err
				}
			case !entry.Type().IsRegular():
				return errTelemetryUnavailable
			case strings.HasSuffix(entry.Name(), ".jsonl"):
				if files = append(files, child); len(files) > telemetryMaxFiles {
					return errTelemetryUnavailable
				}
			}
		}
		return nil
	}
	if err := walk(dir, 0); err != nil {
		return nil, err
	}
	var out [][]string
	remaining := int64(telemetryMaxBytes)
	for _, name := range files {
		lines, read, err := readSessionFile(root, name, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= read
		out = append(out, lines)
	}
	return out, nil
}

func readSessionFile(root *safefs.Root, name string, budget int64) ([]string, int64, error) {
	f, err := root.OpenRegular(name)
	if err != nil {
		return nil, 0, errTelemetryUnavailable
	}
	defer f.Close()
	// The limit bounds reads even if the harness or a tool keeps growing the file.
	limited := &io.LimitedReader{R: f, N: budget + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 0, 64<<10), telemetryMaxLineBytes)
	var lines []string
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if scanner.Err() != nil || limited.N <= 0 {
		return nil, 0, errTelemetryUnavailable
	}
	return lines, budget + 1 - limited.N, nil
}

// addCount sums non-negative values without exceeding the telemetry range.
func addCount(total *int64, v int64) error {
	if v < 0 || *total > (1<<53)-v {
		return errTelemetryUnavailable
	}
	*total += v
	return nil
}

// soleLabel reports a model or effort only when the log names exactly one.
func soleLabel(seen map[string]bool) *string {
	if len(seen) != 1 {
		return nil
	}
	for label := range seen {
		return contract.Label(label)
	}
	return nil
}

// commandKey identifies a structured shell payload in memory only; it is never
// hashed, stored or returned.
func commandKey(tool string, input json.RawMessage) (string, bool) {
	var fields struct {
		Command json.RawMessage `json:"command"`
	}
	if len(input) == 0 || json.Unmarshal(input, &fields) != nil || len(fields.Command) == 0 || string(fields.Command) == "null" {
		return "", false
	}
	var command any
	if json.Unmarshal(fields.Command, &command) != nil {
		return "", false
	}
	canonical, err := json.Marshal(command)
	if err != nil {
		return "", false
	}
	return tool + "\x00" + string(canonical), true
}

type callCounter struct {
	ids      map[string]bool
	commands map[string]int
	calls    int64
	repeated int64
}

func newCallCounter() *callCounter {
	return &callCounter{ids: map[string]bool{}, commands: map[string]int{}}
}

// start counts a tool call once per identifier, ignoring completion echoes.
func (c *callCounter) start(id, tool string, input json.RawMessage) error {
	if id == "" {
		return errTelemetryUnavailable
	}
	if c.ids[id] {
		return nil
	}
	c.ids[id] = true
	c.calls++
	if key, ok := commandKey(tool, input); ok {
		if c.commands[key] > 0 {
			c.repeated++
		}
		c.commands[key]++
	}
	return nil
}

// parseClaudeSession reads Claude Code project logs. Assistant records carry
// per-API-message usage; streamed records repeat a message id, so the last
// usage per id is kept and distinct messages are summed. input_tokens excludes
// cache reads, which are reported separately as cache_read_input_tokens.
// Claude Code logs carry no confirmed effort field, so effort stays unavailable.
func parseClaudeSession(files [][]string) (sessionUsage, error) {
	type usage struct {
		Input     *int64 `json:"input_tokens"`
		Output    *int64 `json:"output_tokens"`
		CacheRead *int64 `json:"cache_read_input_tokens"`
	}
	type record struct {
		Type    string `json:"type"`
		Message *struct {
			ID      string          `json:"id"`
			Model   string          `json:"model"`
			Usage   *usage          `json:"usage"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	type block struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	messages := map[string]usage{}
	models := map[string]bool{}
	calls := newCallCounter()
	assistant := false
	for _, lines := range files {
		for _, line := range lines {
			var rec record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				return sessionUsage{}, errTelemetryUnavailable
			}
			if rec.Type != "assistant" || rec.Message == nil {
				continue
			}
			assistant = true
			if rec.Message.ID == "" {
				return sessionUsage{}, errTelemetryUnavailable
			}
			if rec.Message.Model != "" && rec.Message.Model != "<synthetic>" {
				models[rec.Message.Model] = true
			}
			if rec.Message.Usage != nil {
				messages[rec.Message.ID] = *rec.Message.Usage
			}
			var blocks []block
			if json.Unmarshal(rec.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type == "tool_use" {
					if err := calls.start(b.ID, b.Name, b.Input); err != nil {
						return sessionUsage{}, err
					}
				}
			}
		}
	}
	if !assistant {
		return sessionUsage{}, nil
	}
	out := sessionUsage{toolCalls: contract.Count(calls.calls), repeated: contract.Count(calls.repeated), model: soleLabel(models)}
	if len(messages) == 0 {
		return out, nil
	}
	var input, output, cached int64
	cachedKnown := true
	for _, u := range messages {
		if u.Input == nil || u.Output == nil {
			return out, nil
		}
		if addCount(&input, *u.Input) != nil || addCount(&output, *u.Output) != nil {
			return out, nil
		}
		if u.CacheRead == nil || addCount(&cached, *u.CacheRead) != nil {
			cachedKnown = false
		}
	}
	out.input, out.output = &input, &output
	if cachedKnown {
		out.cached = &cached
	}
	return out, nil
}

// parseCodexSession reads Codex rollout logs. token_count events carry the
// session's cumulative total_token_usage, so each file contributes its last
// snapshot and snapshots are never summed within a file. Each ephemeral
// follow-up turn writes its own session file, so files are summed. Codex
// input_tokens includes cached_input_tokens. turn_context names the model and
// reasoning effort.
func parseCodexSession(files [][]string) (sessionUsage, error) {
	type tokens struct {
		Input  *int64 `json:"input_tokens"`
		Cached *int64 `json:"cached_input_tokens"`
		Output *int64 `json:"output_tokens"`
	}
	type record struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	type payload struct {
		Type      string          `json:"type"`
		Model     string          `json:"model"`
		Effort    string          `json:"effort"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		Info      *struct {
			Total *tokens `json:"total_token_usage"`
		} `json:"info"`
	}
	models, efforts := map[string]bool{}, map[string]bool{}
	calls := newCallCounter()
	var input, output, cached int64
	tokensKnown, cachedKnown, events := true, true, false
	for _, lines := range files {
		var last *tokens
		for _, line := range lines {
			var rec record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				return sessionUsage{}, errTelemetryUnavailable
			}
			var p payload
			if len(rec.Payload) != 0 && json.Unmarshal(rec.Payload, &p) != nil {
				continue
			}
			switch rec.Type {
			case "turn_context":
				events = true
				if p.Model != "" {
					models[p.Model] = true
				}
				if p.Effort != "" {
					efforts[p.Effort] = true
				}
			case "event_msg":
				if p.Type != "token_count" || p.Info == nil || p.Info.Total == nil {
					continue
				}
				events = true
				t := p.Info.Total
				if t.Input == nil || t.Output == nil {
					return sessionUsage{}, errTelemetryUnavailable
				}
				// Cumulative snapshots never decrease; anything else is ambiguous.
				if last != nil && (*t.Input < *last.Input || *t.Output < *last.Output) {
					return sessionUsage{}, errTelemetryUnavailable
				}
				last = t
			case "response_item":
				switch p.Type {
				case "function_call", "custom_tool_call", "local_shell_call":
					events = true
					args := p.Arguments
					var encoded string
					if json.Unmarshal(args, &encoded) == nil {
						args = json.RawMessage(encoded)
					}
					if len(args) == 0 {
						args = p.Input
					}
					if err := calls.start(p.CallID, p.Name, args); err != nil {
						return sessionUsage{}, err
					}
				}
			}
		}
		if last == nil {
			tokensKnown = false
			continue
		}
		if addCount(&input, *last.Input) != nil || addCount(&output, *last.Output) != nil {
			tokensKnown = false
		}
		if last.Cached == nil || addCount(&cached, *last.Cached) != nil {
			cachedKnown = false
		}
	}
	if !events {
		return sessionUsage{}, nil
	}
	out := sessionUsage{toolCalls: contract.Count(calls.calls), repeated: contract.Count(calls.repeated), model: soleLabel(models), effort: soleLabel(efforts)}
	if tokensKnown && len(files) > 0 {
		out.input, out.output = &input, &output
		if cachedKnown {
			out.cached = &cached
		}
	}
	return out, nil
}

// runObservation measures one Runner.Run invocation at coordinator-owned
// boundaries. It reads the harness's session log only after the harness exits.
type runObservation struct {
	started   time.Time
	driver    string
	lifecycle sandbox.Lifecycle
	usage     sessionUsage
}

func newRunObservation(driver string) *runObservation {
	o := &runObservation{started: time.Now(), driver: driver}
	o.lifecycle.ObserveScratch = func(home string) { o.usage = collectSessionUsage(o.driver, home) }
	return o
}

func (o *runObservation) telemetry(role, harness string) *contract.RoleRunTelemetry {
	t := contract.NewRoleRunTelemetry(role, harness, o.started)
	t.ElapsedMS = contract.Millis(time.Since(o.started))
	for dst, src := range map[**int64]*time.Duration{&t.PreflightMS: o.lifecycle.Preflight, &t.SetupMS: o.lifecycle.Setup, &t.SnapshotMS: o.lifecycle.Snapshot} {
		if src != nil {
			*dst = contract.Millis(*src)
		}
	}
	o.usage.apply(t)
	return t
}
