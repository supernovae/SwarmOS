package agent

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// Delegated and native harnesses run with the host HOME, so their session
// records sit among the user's own. These locators read only the run's own
// session, named by its unique control directory or session id, and open
// nothing else. Absent, ambiguous, linked, oversized or malformed records
// yield no values.

var errTelemetryAbsent = errors.New("session directory absent")

var (
	telemetryUUID       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	telemetryOpenCodeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// codexDayEntriesMax bounds a Codex day directory listing. Only names are
// compared, so it can be larger than the scratch-HOME entry limit.
const codexDayEntriesMax = 4096

// collectHarnessSession reads session s, which ran between started and ended.
func collectHarnessSession(s sandbox.HarnessSession, started, ended time.Time) harnessUsage {
	var usage harnessUsage
	var err error
	switch s.Driver {
	case "claude":
		usage, err = collectClaudeSession(s)
	case "codex":
		usage, err = collectCodexSession(s, started, ended)
	case "omp":
		usage, err = collectOMPSession(s)
	case "opencode":
		if len(s.SessionIDs) != 1 {
			return harnessUsage{}
		}
		usage, err = parseOpenCodeMessages(s.Records, s.SessionIDs[0])
	default:
		return harnessUsage{}
	}
	if err != nil {
		return harnessUsage{}
	}
	return usage
}

// openHome opens the absolute harness HOME the run used.
func openHome(home string) (*safefs.Root, error) {
	if home == "" || !filepath.IsAbs(home) {
		return nil, errTelemetryUnavailable
	}
	root, err := safefs.Open(home)
	if err != nil {
		return nil, errTelemetryUnavailable
	}
	return root, nil
}

// claudeProjectSlug is Claude Code's project directory name for cwd: every
// character other than an ASCII letter or digit becomes "-".
func claudeProjectSlug(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, cwd)
}

// ompSessionSlug is omp's session directory name for cwd: "/" becomes "-".
func ompSessionSlug(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}

// collectClaudeSession reads exactly <slug>/<id>.jsonl for each turn's id.
// A missing turn makes the whole run unavailable rather than undercounted.
func collectClaudeSession(s sandbox.HarnessSession) (harnessUsage, error) {
	if !filepath.IsAbs(s.Control) || len(s.SessionIDs) == 0 || len(s.SessionIDs) > telemetryMaxFiles {
		return harnessUsage{}, errTelemetryUnavailable
	}
	root, err := openHome(s.Home)
	if err != nil {
		return harnessUsage{}, err
	}
	defer root.Close()
	dir := filepath.Join(".claude", "projects", claudeProjectSlug(s.Control))
	var files [][]string
	remaining := int64(telemetryMaxBytes)
	for _, id := range s.SessionIDs {
		if !telemetryUUID.MatchString(id) {
			return harnessUsage{}, errTelemetryUnavailable
		}
		lines, read, err := readSessionFile(root, filepath.Join(dir, id+".jsonl"), remaining)
		if err != nil {
			return harnessUsage{}, err
		}
		remaining -= read
		files = append(files, lines)
	}
	usage, err := parseClaudeSession(files)
	return harnessUsage{sessionUsage: usage}, err
}

// collectOMPSession lists only <slug(control)>/ and requires exactly one
// session file whose header names this cwd and the id in its file name.
func collectOMPSession(s sandbox.HarnessSession) (harnessUsage, error) {
	if !filepath.IsAbs(s.Control) {
		return harnessUsage{}, errTelemetryUnavailable
	}
	root, err := openHome(s.Home)
	if err != nil {
		return harnessUsage{}, err
	}
	defer root.Close()
	dir := filepath.Join(".omp", "agent", "sessions", ompSessionSlug(s.Control))
	names, err := listSessionNames(root, dir, telemetryMaxEntries, func(name string) bool { return strings.HasSuffix(name, ".jsonl") })
	if err != nil || len(names) != 1 {
		return harnessUsage{}, errTelemetryUnavailable
	}
	_, id, ok := strings.Cut(strings.TrimSuffix(names[0], ".jsonl"), "_")
	if !ok || id == "" {
		return harnessUsage{}, errTelemetryUnavailable
	}
	lines, _, err := readSessionFile(root, filepath.Join(dir, names[0]), telemetryMaxBytes)
	if err != nil {
		return harnessUsage{}, err
	}
	return parseOMPSession(lines, s.Control, id)
}

// collectCodexSession opens, for each turn's id, the one rollout file named
// rollout-*-<id>.jsonl in the day directories the run spans. Codex names day
// directories by date; local time and UTC are both checked. No other file in
// those directories is opened.
func collectCodexSession(s sandbox.HarnessSession, started, ended time.Time) (harnessUsage, error) {
	if len(s.SessionIDs) == 0 || len(s.SessionIDs) > telemetryMaxFiles || ended.Before(started) || ended.Sub(started) > 31*24*time.Hour {
		return harnessUsage{}, errTelemetryUnavailable
	}
	home, sessions := s.Home, filepath.Join(".codex", "sessions")
	if s.CodexHome != "" {
		home, sessions = s.CodexHome, "sessions"
	}
	root, err := openHome(home)
	if err != nil {
		return harnessUsage{}, err
	}
	defer root.Close()
	var days []string
	seen := map[string]bool{}
	for t := started; ; t = t.Add(24 * time.Hour) {
		if t.After(ended) {
			t = ended
		}
		for _, at := range []time.Time{t.Local(), t.UTC()} {
			if day := at.Format(filepath.Join("2006", "01", "02")); !seen[day] {
				seen[day] = true
				days = append(days, day)
			}
		}
		if !t.Before(ended) {
			break
		}
	}
	var files [][]string
	remaining := int64(telemetryMaxBytes)
	for _, id := range s.SessionIDs {
		if !telemetryUUID.MatchString(id) {
			return harnessUsage{}, errTelemetryUnavailable
		}
		match := func(name string) bool {
			return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, "-"+id+".jsonl")
		}
		var found []string
		for _, day := range days {
			dir := filepath.Join(sessions, day)
			names, err := listSessionNames(root, dir, codexDayEntriesMax, match)
			if errors.Is(err, errTelemetryAbsent) {
				continue
			}
			if err != nil {
				return harnessUsage{}, err
			}
			for _, name := range names {
				found = append(found, filepath.Join(dir, name))
			}
		}
		if len(found) != 1 {
			return harnessUsage{}, errTelemetryUnavailable
		}
		lines, read, err := readSessionFile(root, found[0], remaining)
		if err != nil {
			return harnessUsage{}, err
		}
		remaining -= read
		files = append(files, lines)
	}
	return parseCodexSession(files)
}

// listSessionNames returns the names in dir accepted by match, opening none of
// them. A matching name that is not a regular file makes the result
// unavailable; a missing dir is errTelemetryAbsent.
func listSessionNames(root *safefs.Root, dir string, limit int, match func(string) bool) ([]string, error) {
	f, err := root.OpenDirectory(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errTelemetryAbsent
		}
		return nil, errTelemetryUnavailable
	}
	defer f.Close()
	list, err := f.ReadDir(limit + 1)
	if err != nil && err != io.EOF || len(list) > limit {
		return nil, errTelemetryUnavailable
	}
	var names []string
	for _, entry := range list {
		if !match(entry.Name()) {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, errTelemetryUnavailable
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// tokenSums totals per-message usage; a value any message omits stays
// unavailable rather than undercounted.
type tokenSums struct {
	total [4]int64 // input, output, cached, reasoning
	known [4]bool
}

func newTokenSums() tokenSums { return tokenSums{known: [4]bool{true, true, true, true}} }

func (s *tokenSums) add(input, output, cached, reasoning *int64) {
	for i, v := range []*int64{input, output, cached, reasoning} {
		if v == nil || addCount(&s.total[i], *v) != nil {
			s.known[i] = false
		}
	}
}

func (s *tokenSums) apply(u *harnessUsage) {
	if !s.known[0] || !s.known[1] {
		return
	}
	u.input, u.output = contract.Count(s.total[0]), contract.Count(s.total[1])
	if s.known[2] {
		u.cached = contract.Count(s.total[2])
	}
	if s.known[3] {
		u.reasoning = contract.Count(s.total[3])
	}
}

// parseOMPSession reads one omp session file. Usage is per assistant message
// record: input excludes cacheRead, which is reported as cached, and
// reasoningTokens is reported as reasoning. message.model is omp's configured
// model id. Tool calls are toolCall content blocks.
func parseOMPSession(lines []string, cwd, id string) (harnessUsage, error) {
	type usage struct {
		Input     *int64 `json:"input"`
		Output    *int64 `json:"output"`
		CacheRead *int64 `json:"cacheRead"`
		Reasoning *int64 `json:"reasoningTokens"`
	}
	type record struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		CWD     string `json:"cwd"`
		Message *struct {
			Role    string          `json:"role"`
			Model   string          `json:"model"`
			Usage   *usage          `json:"usage"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	type block struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	header := false
	messages := map[string]usage{}
	models := map[string]bool{}
	calls := newCallCounter()
	for _, line := range lines {
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return harnessUsage{}, errTelemetryUnavailable
		}
		switch rec.Type {
		case "session":
			if header || rec.ID != id || rec.CWD != cwd {
				return harnessUsage{}, errTelemetryUnavailable
			}
			header = true
		case "message":
			if !header {
				return harnessUsage{}, errTelemetryUnavailable
			}
			if rec.Message == nil || rec.Message.Role != "assistant" {
				continue
			}
			if rec.ID == "" {
				return harnessUsage{}, errTelemetryUnavailable
			}
			if rec.Message.Model != "" {
				models[rec.Message.Model] = true
			}
			if rec.Message.Usage != nil {
				messages[rec.ID] = *rec.Message.Usage
			}
			var blocks []block
			if json.Unmarshal(rec.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type == "toolCall" {
					if err := calls.start(b.ID, b.Name, b.Arguments); err != nil {
						return harnessUsage{}, err
					}
				}
			}
		}
	}
	if !header {
		return harnessUsage{}, errTelemetryUnavailable
	}
	out := harnessUsage{sessionUsage: sessionUsage{toolCalls: contract.Count(calls.calls), repeated: contract.Count(calls.repeated), model: soleLabel(models)}}
	if len(messages) == 0 {
		return out, nil
	}
	sums := newTokenSums()
	for _, u := range messages {
		sums.add(u.Input, u.Output, u.CacheRead, u.Reasoning)
	}
	sums.apply(&out)
	return out, nil
}

// parseOpenCodeMessages reads the run's own session messages as exported by
// OpenCode's session messages API. Every message and part must belong to id;
// one foreign record makes the whole export unavailable. Assistant messages
// carry tokens{input,output,reasoning,cache{read}} and modelID; tool parts
// carry callID, tool and state.input.
func parseOpenCodeMessages(raw []byte, id string) (harnessUsage, error) {
	if !telemetryOpenCodeID.MatchString(id) || len(raw) == 0 || len(raw) > telemetryMaxBytes {
		return harnessUsage{}, errTelemetryUnavailable
	}
	type tokens struct {
		Input     *int64 `json:"input"`
		Output    *int64 `json:"output"`
		Reasoning *int64 `json:"reasoning"`
		Cache     *struct {
			Read *int64 `json:"read"`
		} `json:"cache"`
	}
	var export struct {
		Data *[]struct {
			Info *struct {
				ID        string  `json:"id"`
				SessionID string  `json:"sessionID"`
				Role      string  `json:"role"`
				ModelID   string  `json:"modelID"`
				Tokens    *tokens `json:"tokens"`
			} `json:"info"`
			Parts []struct {
				SessionID string `json:"sessionID"`
				Type      string `json:"type"`
				CallID    string `json:"callID"`
				Tool      string `json:"tool"`
				State     struct {
					Input json.RawMessage `json:"input"`
				} `json:"state"`
			} `json:"parts"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &export) != nil || export.Data == nil {
		return harnessUsage{}, errTelemetryUnavailable
	}
	messages := map[string]tokens{}
	models := map[string]bool{}
	calls := newCallCounter()
	for _, m := range *export.Data {
		if m.Info == nil || m.Info.SessionID != id {
			return harnessUsage{}, errTelemetryUnavailable
		}
		for _, p := range m.Parts {
			if p.SessionID != "" && p.SessionID != id {
				return harnessUsage{}, errTelemetryUnavailable
			}
			if p.Type == "tool" {
				if err := calls.start(p.CallID, p.Tool, p.State.Input); err != nil {
					return harnessUsage{}, err
				}
			}
		}
		if m.Info.Role != "assistant" {
			continue
		}
		if m.Info.ID == "" {
			return harnessUsage{}, errTelemetryUnavailable
		}
		if m.Info.ModelID != "" {
			models[m.Info.ModelID] = true
		}
		if m.Info.Tokens != nil {
			messages[m.Info.ID] = *m.Info.Tokens
		}
	}
	out := harnessUsage{sessionUsage: sessionUsage{toolCalls: contract.Count(calls.calls), repeated: contract.Count(calls.repeated), model: soleLabel(models)}}
	if len(messages) == 0 {
		return out, nil
	}
	sums := newTokenSums()
	for _, t := range messages {
		var read *int64
		if t.Cache != nil {
			read = t.Cache.Read
		}
		sums.add(t.Input, t.Output, read, t.Reasoning)
	}
	sums.apply(&out)
	return out, nil
}
