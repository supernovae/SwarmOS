package lease

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/supernovae/swarmos/internal/coordination"
)

// InventoryState deliberately excludes credentials, claim tokens and artifact
// contents. Only authoritative state, never a PR body, links an issue and PR.
type InventoryState struct {
	Issue                           int
	Branch, SubmittedHead, ForkRepo string
	PR                              int
	Active, Pending, Completed      bool
}

func ReadInventoryState(ctx context.Context, store coordination.InventoryPreviewStore, n int, reserve func(int64) error) (InventoryState, error) {
	var out InventoryState
	data, err := store.LoadInventoryPreview(ctx, n, coordination.InventoryInputLimit, reserve, ProjectInventory)
	if err != nil {
		return out, err
	}
	return DecodeInventoryState(ctx, data, n)
}

// DecodeInventoryState projects an already bounded, pinned authoritative read.
func DecodeInventoryState(ctx context.Context, data []byte, n int) (InventoryState, error) {
	compact, err := ProjectInventory(ctx, bytes.NewReader(data), func(int64) error { return nil })
	if err != nil {
		return InventoryState{}, err
	}
	data = compact
	var out InventoryState

	var st struct {
		Lease struct {
			Issue   int    `json:"issue_number"`
			Version int    `json:"version"`
			Status  string `json:"status"`
		} `json:"lease"`
		Branch        string    `json:"branch"`
		SubmittedHead string    `json:"submitted_head_sha"`
		PR            int       `json:"pr_number"`
		ForkRepo      string    `json:"fork_repo"`
		Revision      *struct{} `json:"pr_revision"`
		Cleanup       *struct{} `json:"cleanup"`
	}
	if err = json.Unmarshal(data, &st); err != nil {
		return out, err
	}
	if st.Lease.Version != 1 || st.Lease.Issue != n || st.PR < 0 {
		return out, fmt.Errorf("state identity unavailable")
	}
	return InventoryState{Issue: n, Branch: st.Branch, SubmittedHead: st.SubmittedHead, PR: st.PR, ForkRepo: st.ForkRepo, Active: st.Lease.Status == "active", Completed: st.Lease.Status == "completed", Pending: st.Revision != nil || st.Cleanup != nil}, nil
}

// ProjectInventory validates the entire pinned JSON stream while retaining only
// cleanup inspection inputs. It is read-only evidence, never mutation authority.
func ProjectInventory(ctx context.Context, r io.Reader, reserve func(int64) error) ([]byte, error) {
	if err := reserve(96 << 10); err != nil {
		return nil, err
	}
	p := inventoryParser{ctx: ctx, r: bufio.NewReaderSize(io.LimitReader(r, coordination.InventoryInputLimit+1), 4096), out: make([]byte, 0, 16<<10)}
	if err := p.value(0, true, ""); err != nil {
		return nil, err
	}
	if err := p.space(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("trailing inventory JSON")
		}
		return nil, err
	}
	return p.out, nil
}

type inventoryParser struct {
	ctx     context.Context
	r       *bufio.Reader
	out     []byte
	used    int64
	records int
}

func (p *inventoryParser) read() (byte, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	c, err := p.r.ReadByte()
	if err == nil {
		p.used++
		if p.used > coordination.InventoryInputLimit {
			return 0, fmt.Errorf("inventory input exceeds %d byte limit", coordination.InventoryInputLimit)
		}
	}
	return c, err
}
func (p *inventoryParser) peek() (byte, error) {
	b, err := p.r.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}
func (p *inventoryParser) space() error {
	for {
		c, err := p.peek()
		if err != nil {
			return err
		}
		if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			return nil
		}
		if _, err = p.read(); err != nil {
			return err
		}
	}
}
func (p *inventoryParser) emit(c byte, keep bool) error {
	if !keep {
		return nil
	}
	if len(p.out) == cap(p.out) {
		return fmt.Errorf("inventory retained projection limit")
	}
	p.out = append(p.out, c)
	return nil
}
func (p *inventoryParser) take(want byte, keep bool) error {
	c, err := p.read()
	if err != nil {
		return err
	}
	if c != want {
		return fmt.Errorf("invalid inventory JSON")
	}
	return p.emit(c, keep)
}

// Match encoding/json's case folding so alternate spellings cannot hide an
// authority field from the projection and reappear in a full-state reader.
func inventoryField(path, key string) string {
	var keys []string
	switch path {
	case "lease":
		keys = []string{"version", "issue_number", "status", "phase"}
	case "":
		keys = []string{"lease", "branch", "submitted_head_sha", "head_sha", "base_sha", "pr_number", "pr_url", "fork_repo", "pr_revision", "cleanup", "pr_outcome", "checkpoint_heads", "last_rebase", "rebase_transitions"}
	case "last_rebase", "rebase_transitions":
		keys = []string{"old_head", "new_head", "old_base", "new_base", "status", "patch_identity", "verified"}
	case "pr_outcome":
		keys = []string{"target_branch", "status", "number", "url", "head_repo", "base_repo", "branch", "submitted_head_sha", "final_head_sha", "merge_commit_sha", "merged_at", "observed_at"}
	default:
		return ""
	}
	for _, canonical := range keys {
		if strings.EqualFold(canonical, key) {
			return canonical
		}
	}
	return ""
}
func (p *inventoryParser) str(keep bool) (string, error) {
	var key []byte
	if keep {
		key = make([]byte, 0, 128)
	}
	if err := p.take('"', keep); err != nil {
		return "", err
	}
	escaped := false
	unicode := 0
	for {
		c, err := p.read()
		if err != nil {
			return "", err
		}
		if err = p.emit(c, keep); err != nil {
			return "", err
		}
		if keep {
			if len(key) >= 4096 {
				return "", fmt.Errorf("inventory retained string limit")
			}
			key = append(key, c)
		}
		if unicode > 0 {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return "", fmt.Errorf("invalid JSON unicode escape")
			}
			unicode--
			continue
		}
		if escaped {
			escaped = false
			switch c {
			case 'u':
				unicode = 4
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			default:
				return "", fmt.Errorf("invalid JSON escape")
			}
			continue
		}
		if c == '"' {
			break
		}
		if c < 32 {
			return "", fmt.Errorf("invalid JSON string")
		}
		if c == '\\' {
			escaped = true
		}
	}
	if !keep {
		return "", nil
	}
	var out string
	err := json.Unmarshal(append([]byte{'"'}, key...), &out)
	return out, err
}
func (p *inventoryParser) value(depth int, keep bool, path string) error {
	if depth > 32 {
		return fmt.Errorf("state nesting exceeds inventory limit")
	}
	if err := p.space(); err != nil {
		return err
	}
	c, err := p.peek()
	if err != nil {
		return err
	}
	if depth >= 32 && (c == '{' || c == '[') {
		return fmt.Errorf("state nesting exceeds inventory limit")
	}
	switch c {
	case '{':
		if err = p.take('{', keep); err != nil {
			return err
		}
		first := true
		seen := map[string]bool{}
		if err = p.space(); err != nil {
			return err
		}
		c, _ = p.peek()
		if c == '}' {
			return p.take('}', keep)
		}
		for {
			if err = p.space(); err != nil {
				return err
			}
			// Keys are bounded even on ignored objects; they never grow with values.
			start := len(p.out)
			key, e := p.str(true)
			p.out = p.out[:start]
			if e != nil {
				return e
			}
			canonical := inventoryField(path, key)
			if keep && canonical != "" && canonical != key {
				return fmt.Errorf("ambiguous inventory authority field")
			}
			if len(key) > 256 {
				return fmt.Errorf("inventory key limit")
			}
			selected := keep && canonical != ""
			if selected {
				p.records++
				if p.records > 128 {
					return fmt.Errorf("inventory record limit")
				}
			}
			if selected {
				for previous := range seen {
					if strings.EqualFold(previous, key) {
						return fmt.Errorf("duplicate inventory authority field %q", key)
					}
				}
				seen[key] = true
				if !first {
					if err = p.emit(',', true); err != nil {
						return err
					}
				}
				first = false
				for _, b := range []byte(strconvQuote(key)) {
					if err = p.emit(b, true); err != nil {
						return err
					}
				}
				if err = p.emit(':', true); err != nil {
					return err
				}
			}
			if err = p.space(); err != nil {
				return err
			}
			if err = p.take(':', false); err != nil {
				return err
			}
			if selected && path == "" && (key == "cleanup" || key == "pr_revision") {
				if err = p.space(); err != nil {
					return err
				}
				next, _ := p.peek()
				if err = p.value(depth+1, false, key); err != nil {
					return err
				}
				literal := "{}"
				if next == 'n' {
					literal = "null"
				}
				for _, b := range []byte(literal) {
					if err = p.emit(b, true); err != nil {
						return err
					}
				}
			} else {
				child := key
				if err = p.value(depth+1, selected, child); err != nil {
					return err
				}
			}
			if err = p.space(); err != nil {
				return err
			}
			c, _ = p.peek()
			if c == '}' {
				return p.take('}', keep)
			}
			if err = p.take(',', false); err != nil {
				return err
			}
		}
	case '[':
		if err = p.take('[', keep); err != nil {
			return err
		}
		if err = p.space(); err != nil {
			return err
		}
		c, _ = p.peek()
		if c == ']' {
			return p.take(']', keep)
		}
		for {
			if err = p.value(depth+1, keep, path); err != nil {
				return err
			}
			if err = p.space(); err != nil {
				return err
			}
			c, _ = p.peek()
			if c == ']' {
				return p.take(']', keep)
			}
			if err = p.take(',', keep); err != nil {
				return err
			}
		}
	case '"':
		_, err = p.str(keep)
		return err
	default:
		// Scalars have a fixed grammar and bounded length, independently of input.
		var scalar [128]byte
		n := 0
		for {
			c, e := p.peek()
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
			if c == ',' || c == ']' || c == '}' || c == ' ' || c == '\n' || c == '\r' || c == '\t' {
				break
			}
			if n == len(scalar) {
				return fmt.Errorf("inventory scalar limit")
			}
			c, err = p.read()
			if err != nil {
				return err
			}
			scalar[n] = c
			n++
			if err = p.emit(c, keep); err != nil {
				return err
			}
		}
		if n == 0 || !json.Valid(scalar[:n]) {
			return fmt.Errorf("invalid inventory scalar")
		}
		return nil
	}
}
func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
