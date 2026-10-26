package coordination

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrLedgerNamespace reports a ledger ref found in the namespace this
// repository did not select, alone or alongside the selected one. SwarmOS
// never falls back between namespaces.
var ErrLedgerNamespace = errors.New("ledger namespace conflict")

// RefLayout names the ledger ref families for one namespace mode. Task
// branches are PR heads and are never part of a layout.
type RefLayout struct{ hidden bool }

var (
	// BranchLayout keeps ledger refs under refs/heads/, as before namespaces existed.
	BranchLayout = RefLayout{}
	// HiddenLayout keeps ledger refs under refs/swarm/, outside the branch list.
	HiddenLayout = RefLayout{hidden: true}
)

// LayoutFor maps a ledger_namespace value to its layout; empty means branch.
func LayoutFor(namespace string) (RefLayout, error) {
	switch namespace {
	case "", "branch":
		return BranchLayout, nil
	case "hidden":
		return HiddenLayout, nil
	}
	return RefLayout{}, fmt.Errorf("unknown ledger namespace %q (must be branch or hidden)", namespace)
}

// Name is the ledger_namespace value selecting this layout.
func (l RefLayout) Name() string {
	if l.hidden {
		return "hidden"
	}
	return "branch"
}

// Counterpart is the layout of the namespace this repository did not select.
func (l RefLayout) Counterpart() RefLayout { return RefLayout{hidden: !l.hidden} }

// StatePrefix holds one authoritative state ref per issue.
func (l RefLayout) StatePrefix() string {
	if l.hidden {
		return "refs/swarm/state/"
	}
	return "refs/heads/swarm-state/"
}

// CheckpointPrefix holds permanent checkpoint object retention refs.
func (l RefLayout) CheckpointPrefix() string {
	if l.hidden {
		return "refs/swarm/checkpoints/"
	}
	return "refs/heads/swarm-checkpoints/"
}

// ContributorPrefix holds fork contributor claim indexes.
func (l RefLayout) ContributorPrefix() string {
	if l.hidden {
		return "refs/swarm/contributors/"
	}
	return "refs/heads/swarm-contributors/"
}

// StateRef is the authoritative state ref of issue n.
func (l RefLayout) StateRef(n int) string { return fmt.Sprintf("%sissue-%d", l.StatePrefix(), n) }

// CheckpointRef is permanent object retention, independent of task branches and
// the ledger's code-free ancestry. Cleanup must never remove this namespace.
func (l RefLayout) CheckpointRef(n int, head string) string {
	return fmt.Sprintf("%sissue-%d/%s", l.CheckpointPrefix(), n, head)
}

// ContributorRef is the claim index of a lower-cased, validated actor.
func (l RefLayout) ContributorRef(actor string) string { return l.ContributorPrefix() + actor }

// prefixes lists the three ledger families in a stable order.
func (l RefLayout) prefixes() []string {
	return []string{l.StatePrefix(), l.CheckpointPrefix(), l.ContributorPrefix()}
}

// counterpartRef maps a ledger ref of this layout to the same ledger entry in
// the other namespace. Non-ledger refs such as task branches have none.
func (l RefLayout) counterpartRef(ref string) (string, bool) {
	other := l.Counterpart().prefixes()
	for i, prefix := range l.prefixes() {
		if rest, ok := strings.CutPrefix(ref, prefix); ok && rest != "" {
			return other[i] + rest, true
		}
	}
	return "", false
}

// IsLedgerRef reports whether ref belongs to either namespace's ledger
// families, so callers can keep task-branch operations away from them.
func IsLedgerRef(ref string) bool {
	_, branch := BranchLayout.counterpartRef(ref)
	_, hidden := HiddenLayout.counterpartRef(ref)
	return branch || hidden
}

// ledgerConflict describes a counterpart found for the selected ref.
func (l RefLayout) ledgerConflict(ref, counterpart string, selectedExists bool) error {
	if selectedExists {
		return fmt.Errorf("%w: both %s and %s exist; remove the stale copy or rerun swarm repo migrate-refs before continuing", ErrLedgerNamespace, ref, counterpart)
	}
	return fmt.Errorf("%w: %s is absent but %s exists in the %s namespace; this repository selects ledger_namespace: %s, so run swarm repo migrate-refs or select %s on the default branch", ErrLedgerNamespace, ref, counterpart, l.Counterpart().Name(), l.Name(), l.Counterpart().Name())
}

// Layout is the ledger namespace this store reads and writes.
func (s *GitStore) Layout() RefLayout { return s.layout }

// checkLedger fails closed when ref's counterpart exists, ignoring absence of
// ref itself. Callers use it before writes that do not otherwise read ref.
func (s *GitStore) checkLedger(ctx context.Context, ref string) error {
	if _, err := s.remoteSHA(ctx, ref); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// remoteRefs reads the exact remote SHAs of refs, omitting absent ones.
func (s *GitStore) remoteRefs(ctx context.Context, refs ...string) (map[string]string, error) {
	out, err := s.run(ctx, "", append([]string{"ls-remote", "--refs", s.Remote()}, refs...)...)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(refs))
	for _, ref := range refs {
		want[ref] = true
	}
	found := make(map[string]string)
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid ls-remote result for %s", strings.Join(refs, " "))
		}
		// ls-remote patterns match ref suffixes; keep only exact names.
		if want[fields[1]] {
			found[fields[1]] = fields[0]
		}
	}
	return found, nil
}
