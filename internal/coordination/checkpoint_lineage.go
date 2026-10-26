package coordination

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// CheckpointLineage reads only the loaded ledger revision and its ancestors.
// Retention refs preserve objects but are never checkpoint authority.
func (s *GitStore) CheckpointLineage(ctx context.Context, n int, revision string) ([]string, error) {
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
	var heads []string
	for _, commit := range strings.Fields(commits) {
		raw, err := s.run(ctx, "", "show", commit+":state.json")
		if err != nil {
			return nil, err
		}
		var st struct {
			Lease struct {
				IssueNumber int `json:"issue_number"`
			} `json:"lease"`
			Head      string   `json:"head_sha"`
			Submitted string   `json:"submitted_head_sha"`
			Heads     []string `json:"checkpoint_heads"`
		}
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return nil, err
		}
		if st.Lease.IssueNumber != n {
			return nil, fmt.Errorf("checkpoint history issue identity mismatch")
		}
		heads = append(heads, st.Head, st.Submitted)
		heads = append(heads, st.Heads...)
	}
	return heads, nil
}
