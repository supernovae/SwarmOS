package workspace

import (
	"context"
	"fmt"

	"github.com/supernovae/swarmos/internal/harness"
)

// TrustedLedgerNamespace reads ledger_namespace from the upstream remote's
// advertised default branch. Task worktrees, candidate commits, local
// configuration, and contributor forks never select the namespace, and a
// missing or unreadable upstream is an error rather than a fallback.
func TrustedLedgerNamespace(repoRoot, upstreamRemote string) (string, error) {
	return TrustedLedgerNamespaceContext(context.Background(), repoRoot, upstreamRemote)
}

// TrustedLedgerNamespaceContext preserves cancellation during upstream discovery.
func TrustedLedgerNamespaceContext(ctx context.Context, repoRoot, upstreamRemote string) (string, error) {
	sha, err := ResolveTrustedDefaultContext(ctx, repoRoot, upstreamRemote)
	if err != nil {
		return "", fmt.Errorf("resolve ledger namespace from upstream default branch: %w", err)
	}
	ns, err := harness.LedgerNamespaceAt(repoRoot, sha)
	if err != nil {
		return "", fmt.Errorf("resolve ledger namespace from upstream default branch %s: %w", sha, err)
	}
	return ns, nil
}
