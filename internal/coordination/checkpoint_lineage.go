package coordination

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// CheckpointRebase is a coordinator-recorded rebase transition. Only completed,
// verified records with unchanged patch identity can connect cleanup lineage.
type CheckpointRebase struct {
	OldHead       string `json:"old_head"`
	NewHead       string `json:"new_head,omitempty"`
	OldBase       string `json:"old_base"`
	NewBase       string `json:"new_base"`
	Status        string `json:"status"`
	PatchIdentity string `json:"patch_identity"`
	Verified      bool   `json:"verified"`
}

// CheckpointHistory is the checkpoint evidence of a pinned ledger revision.
type CheckpointHistory struct {
	Heads   []string
	Rebases []CheckpointRebase
}

// CheckpointLineage reads only the loaded ledger revision and its ancestors.
// Retention refs preserve objects but are never checkpoint authority.
func (s *GitStore) CheckpointLineage(ctx context.Context, n int, revision string) ([]string, error) {
	history, err := s.CheckpointHistory(ctx, n, revision)
	if err != nil {
		return nil, err
	}
	return history.Heads, nil
}

// CheckpointHistory reads checkpoint heads and recorded rebase transitions from
// the loaded ledger revision and its ancestors in one walk.
func (s *GitStore) CheckpointHistory(ctx context.Context, n int, revision string) (*CheckpointHistory, error) {
	if len(revision) != 40 {
		return nil, fmt.Errorf("invalid state revision")
	}
	if _, err := hex.DecodeString(revision); err != nil {
		return nil, err
	}
	commits, err := s.run(ctx, "", "rev-list", revision)
	if err != nil {
		return nil, err
	}
	history := &CheckpointHistory{}
	for _, commit := range strings.Fields(commits) {
		raw, err := s.run(ctx, "", "show", commit+":state.json")
		if err != nil {
			return nil, err
		}
		var st struct {
			Lease struct {
				IssueNumber int `json:"issue_number"`
			} `json:"lease"`
			Head        string             `json:"head_sha"`
			Submitted   string             `json:"submitted_head_sha"`
			Heads       []string           `json:"checkpoint_heads"`
			LastRebase  *CheckpointRebase  `json:"last_rebase"`
			Transitions []CheckpointRebase `json:"rebase_transitions"`
		}
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return nil, err
		}
		if st.Lease.IssueNumber != n {
			return nil, fmt.Errorf("checkpoint history issue identity mismatch")
		}
		history.Heads = append(history.Heads, st.Head, st.Submitted)
		history.Heads = append(history.Heads, st.Heads...)
		if st.LastRebase != nil {
			history.Rebases = AppendCheckpointRebases(history.Rebases, *st.LastRebase)
		}
		history.Rebases = AppendCheckpointRebases(history.Rebases, st.Transitions...)
	}
	return history, nil
}

// AppendCheckpointRebases appends transitions not already present.
func AppendCheckpointRebases(list []CheckpointRebase, add ...CheckpointRebase) []CheckpointRebase {
	for _, r := range add {
		found := false
		for _, old := range list {
			if old == r {
				found = true
				break
			}
		}
		if !found {
			list = append(list, r)
		}
	}
	return list
}
