package coordination

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
)

// CompareAndSwapAndDeleteBranch retires only a task code ref, in the same
// remote transaction as its durable progress record. Both leases are supplied
// by the caller's reserved snapshot; neither is refreshed on failure.
func (s *GitStore) CompareAndSwapAndDeleteBranch(ctx context.Context, n int, old string, next []byte, branch, expected string) (string, error) {
	if n <= 0 || old == "" || !strings.HasPrefix(branch, fmt.Sprintf("swarm/issue-%d-", n)) || strings.Contains(branch, "..") || len(expected) != 40 {
		return "", fmt.Errorf("invalid task branch deletion identity")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return "", err
	}
	ref := "refs/heads/" + branch
	if _, err := s.run(ctx, "", "check-ref-format", ref); err != nil {
		return "", err
	}
	fetchURL, err := s.run(ctx, "", "remote", "get-url", "--all", "origin")
	if err != nil {
		return "", err
	}
	pushURL, err := s.run(ctx, "", "remote", "get-url", "--push", "--all", "origin")
	if err != nil {
		return "", err
	}
	if strings.Contains(fetchURL, "\n") || strings.Contains(pushURL, "\n") || fetchURL == "" || fetchURL != pushURL {
		return "", fmt.Errorf("cleanup requires one identical fetch and push destination")
	}
	blob, err := s.run(ctx, string(next), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	tree, err := s.run(ctx, fmt.Sprintf("100644 blob %s\tstate.json\n", blob), "mktree")
	if err != nil {
		return "", err
	}
	commit, err := s.run(ctx, "swarm cleanup progress\n", "-c", "user.name=SwarmOS", "-c", "user.email=swarmos@localhost", "commit-tree", tree, "-p", old)
	if err != nil {
		return "", err
	}
	retention, err := s.historicalRetentionArgs(ctx, n, old, expected)
	if err != nil {
		return "", err
	}
	push := append([]string{"push", "--atomic", pushURL, "--force-with-lease=" + s.ref(n) + ":" + old, "--force-with-lease=" + ref + ":" + expected, commit + ":" + s.ref(n), ":" + ref}, retention...)
	if _, err = s.run(ctx, "", push...); err != nil {
		return "", fmt.Errorf("%w: atomic cleanup rejected: %v", ErrConflict, err)
	}
	return commit, nil
}
