package coordination

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// CheckpointRef is permanent object retention, independent of task branches and
// the ledger's code-free ancestry. Cleanup must never remove this namespace.
func CheckpointRef(n int, head string) string {
	return fmt.Sprintf("refs/heads/swarm-checkpoints/issue-%d/%s", n, head)
}

// retentionArgs builds create-only leases for an atomic publication. A ref
// already present must contain precisely the commit encoded in its name.
func (s *GitStore) retentionArgs(ctx context.Context, n int, heads ...string) ([]string, error) {
	var args []string
	seen := make(map[string]bool)
	for _, head := range heads {
		if head == "" || seen[head] {
			continue
		}
		seen[head] = true
		if len(head) != 40 {
			return nil, fmt.Errorf("invalid checkpoint SHA %q", head)
		}
		if _, err := hex.DecodeString(head); err != nil {
			return nil, fmt.Errorf("invalid checkpoint SHA %q", head)
		}
		ref := CheckpointRef(n, head)
		if _, err := s.run(ctx, "", "cat-file", "-e", head+"^{commit}"); err != nil {
			if _, fetchErr := s.run(ctx, "", "fetch", "--no-write-fetch-head", "origin", ref); fetchErr != nil {
				return nil, fmt.Errorf("checkpoint object %s unavailable (including retention ref %s): %w", head, ref, fetchErr)
			}
			if _, err := s.run(ctx, "", "cat-file", "-e", head+"^{commit}"); err != nil {
				return nil, err
			}
		}
		actual, err := s.remoteSHA(ctx, ref)
		if err == nil {
			if actual != head {
				return nil, fmt.Errorf("%w: retention ref %s contains %s", ErrConflict, ref, actual)
			}
			// Include even an existing ref: the lease detects concurrent corruption.
			args = append(args, "--force-with-lease="+ref+":"+head, head+":"+ref)
		} else if err == ErrNotFound {
			args = append(args, "--force-with-lease="+ref+":", head+":"+ref)
		} else {
			return nil, err
		}
	}
	return args, nil
}

// historicalRetentionArgs migrates available legacy ledger references. Missing
// older history is reported but cannot prevent progress; current predecessor
// and candidate availability is checked separately by retentionArgs.
func (s *GitStore) historicalRetentionArgs(ctx context.Context, n int, old string, required ...string) ([]string, error) {
	heads := append([]string{}, required...)
	if old != "" {
		commits, err := s.run(ctx, "", "rev-list", old)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool)
		for _, sha := range required {
			seen[sha] = true
		}
		for _, commit := range strings.Fields(commits) {
			raw, err := s.run(ctx, "", "show", commit+":state.json")
			if err != nil {
				return nil, err
			}
			var state struct {
				Head      string `json:"head_sha"`
				Submitted string `json:"submitted_head_sha"`
				Rebase    *struct {
					Old string `json:"old_head"`
					New string `json:"new_head"`
				} `json:"last_rebase"`
			}
			if json.Unmarshal([]byte(raw), &state) != nil {
				continue
			}
			older := []string{state.Head, state.Submitted}
			if state.Rebase != nil {
				older = append(older, state.Rebase.Old, state.Rebase.New)
			}
			for _, head := range older {
				if head == "" || seen[head] {
					continue
				}
				seen[head] = true
				if _, err := s.run(ctx, "", "cat-file", "-e", head+"^{commit}"); err != nil {
					log.Printf("historical checkpoint %s unavailable locally; retaining available history only", head)
					continue
				}
				heads = append(heads, head)
			}
		}
	}
	return s.retentionArgs(ctx, n, heads...)
}
