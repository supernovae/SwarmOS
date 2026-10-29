package coordination

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
)

func validCheckpointSHA(head string) error {
	if len(head) != 40 {
		return fmt.Errorf("invalid checkpoint SHA %q", head)
	}
	if _, err := hex.DecodeString(head); err != nil {
		return fmt.Errorf("invalid checkpoint SHA %q", head)
	}
	return nil
}

// CompareAndSwapRetaining pushes the state commit and the retention refs for
// heads atomically, leased on the loaded state revision. A head missing locally
// is fetched from the upstream remote first; an unavailable commit fails closed.
func (s *GitStore) CompareAndSwapRetaining(ctx context.Context, n int, old string, next []byte, heads ...string) (string, error) {
	if len(heads) == 0 {
		return "", fmt.Errorf("no checkpoint to retain")
	}
	for _, head := range heads {
		if err := validCheckpointSHA(head); err != nil {
			return "", err
		}
		if _, err := s.run(ctx, "", "cat-file", "-e", head+"^{commit}"); err != nil {
			// Best effort: retentionArgs reports the commit as unavailable.
			_, _ = s.run(ctx, "", "fetch", "--no-write-fetch-head", s.Remote(), head)
		}
	}
	blob, err := s.run(ctx, string(next), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	tree, err := s.run(ctx, fmt.Sprintf("100644 blob %s\tstate.json\n", blob), "mktree")
	if err != nil {
		return "", err
	}
	args := []string{"-c", "user.name=SwarmOS", "-c", "user.email=swarmos@localhost", "commit-tree", tree}
	if old != "" {
		args = append(args, "-p", old)
	}
	commit, err := s.run(ctx, "swarm coordination checkpoint retained\n", args...)
	if err != nil {
		return "", err
	}
	retention, err := s.historicalRetentionArgs(ctx, n, old, heads...)
	if err != nil {
		return "", err
	}
	stateRef := s.ref(n)
	stateLease := stateRef + ":" + old
	if old == "" {
		stateLease = stateRef + ":"
	}
	push := append([]string{"push", "--atomic", s.Remote(), "--force-with-lease=" + stateLease, commit + ":" + stateRef}, retention...)
	if err = s.push(ctx, push...); err != nil {
		if publicationStatus(err) {
			return "", err
		}
		actual, readErr := s.remoteSHA(ctx, stateRef)
		if (readErr == nil && actual != old) || (errors.Is(readErr, ErrNotFound) && old != "") {
			return "", fmt.Errorf("%w: %w", ErrConflict, err)
		}
		return "", fmt.Errorf("atomic retained state push: %w", err)
	}
	return commit, nil
}

// CompareAndSwapRetaining creates each local retention ref (or verifies the
// existing one) before the single locked state write. A losing write leaves at
// most an extra retention ref for a real commit, never a second state write.
func (s *FileStore) CompareAndSwapRetaining(ctx context.Context, n int, old string, next []byte, heads ...string) (string, error) {
	if len(heads) == 0 {
		return "", fmt.Errorf("no checkpoint to retain")
	}
	git := &GitStore{root: s.repoRoot}
	for _, head := range heads {
		if err := validCheckpointSHA(head); err != nil {
			return "", err
		}
		if _, err := git.run(ctx, "", "cat-file", "-e", head+"^{commit}"); err != nil {
			return "", fmt.Errorf("checkpoint object %s unavailable: %w", head, err)
		}
		ref := BranchLayout.CheckpointRef(n, head)
		if actual, err := git.run(ctx, "", "rev-parse", "--verify", "--quiet", ref); err == nil {
			if actual != head {
				return "", fmt.Errorf("%w: retention ref %s contains %s", ErrConflict, ref, actual)
			}
			continue
		}
		// The zero old value makes this create-only.
		if _, err := git.run(ctx, "", "update-ref", ref, head, strings.Repeat("0", 40)); err != nil {
			return "", fmt.Errorf("create retention ref %s: %w", ref, err)
		}
	}
	return s.CompareAndSwap(ctx, n, old, next)
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
		ref := s.layout.CheckpointRef(n, head)
		if _, err := s.run(ctx, "", "cat-file", "-e", head+"^{commit}"); err != nil {
			if _, fetchErr := s.run(ctx, "", "fetch", "--no-write-fetch-head", s.Remote(), ref); fetchErr != nil {
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
		} else if errors.Is(err, ErrNotFound) {
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
