package cli

import (
	"testing"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/workspace"
)

// trustedLedgerStore selects the store namespace as production does: from the
// upstream default branch configuration, never the local checkout.
func trustedLedgerStore(t *testing.T, root, remote string) *coordination.GitStore {
	t.Helper()
	ns, err := workspace.TrustedLedgerNamespace(root, remote)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := coordination.LayoutFor(ns)
	if err != nil {
		t.Fatal(err)
	}
	return coordination.NewGitStoreWithLayout(root, remote, layout)
}
