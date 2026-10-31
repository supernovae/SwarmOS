package lease

import (
	"context"
	"encoding/json"
	"fmt"

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
	data, err := store.LoadInventoryPreview(ctx, n, 128<<10, reserve)
	if err != nil {
		return out, err
	}
	return DecodeInventoryState(ctx, data, n)
}

// DecodeInventoryState projects an already bounded, pinned authoritative read.
func DecodeInventoryState(ctx context.Context, data []byte, n int) (InventoryState, error) {
	var out InventoryState
	var err error
	// Bound nesting independently of byte size, before invoking encoding/json.
	depth := 0
	quoted, escape := false, false
	for _, c := range data {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		if quoted {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
		if depth > 32 || depth < 0 {
			return out, fmt.Errorf("state nesting exceeds inventory limit")
		}
	}
	var st struct {
		Lease struct {
			Issue   int    `json:"issue_number"`
			Version int    `json:"version"`
			Status  string `json:"status"`
		} `json:"lease"`
		Branch        string          `json:"branch"`
		SubmittedHead string          `json:"submitted_head_sha"`
		PR            int             `json:"pr_number"`
		ForkRepo      string          `json:"fork_repo"`
		Revision      json.RawMessage `json:"pr_revision"`
		Cleanup       json.RawMessage `json:"cleanup"`
	}
	if err = json.Unmarshal(data, &st); err != nil {
		return out, err
	}
	if st.Lease.Version != 1 || st.Lease.Issue != n || st.PR < 0 {
		return out, fmt.Errorf("state identity unavailable")
	}
	pending := func(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }
	return InventoryState{Issue: n, Branch: st.Branch, SubmittedHead: st.SubmittedHead, PR: st.PR, ForkRepo: st.ForkRepo, Active: st.Lease.Status == "active", Completed: st.Lease.Status == "completed", Pending: pending(st.Revision) || pending(st.Cleanup)}, nil
}
