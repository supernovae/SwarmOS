package lease

import (
	"context"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/workspace"
)

func preserveCheckpointHeads(st *IssueState, heads ...string) {
	for _, head := range heads {
		if head == "" {
			continue
		}
		found := false
		for _, old := range st.CheckpointHeads {
			if old == head {
				found = true
				break
			}
		}
		if !found {
			st.CheckpointHeads = append(st.CheckpointHeads, head)
		}
	}
}

func (r *RebaseEvidence) checkpoint() coordination.CheckpointRebase {
	return coordination.CheckpointRebase{OldHead: r.OldHead, NewHead: r.NewHead, OldBase: r.OldBase, NewBase: r.NewBase, Status: r.Status, PatchIdentity: r.PatchIdentity, Verified: r.Verified}
}

// verifiedRebase reports whether a recorded transition is completed
// coordinator evidence. Conflict resolutions that changed the patch count;
// identities the coordinator could not compute do not.
func verifiedRebase(r coordination.CheckpointRebase) bool {
	identity := r.PatchIdentity == "unchanged" || r.PatchIdentity == "changed"
	return r.Status == "completed" && r.Verified && identity && r.OldHead != "" && r.NewHead != "" && r.OldHead != r.NewHead
}

// recordRebaseTransition keeps a published verified rebase after a later
// rebase replaces LastRebase, so local state can prove a chain.
func recordRebaseTransition(st *IssueState, published string) {
	if st.LastRebase == nil || st.LastRebase.NewHead != published {
		return
	}
	if r := st.LastRebase.checkpoint(); verifiedRebase(r) {
		st.RebaseTransitions = coordination.AppendCheckpointRebases(st.RebaseTransitions, r)
	}
}

func (m *Manager) cleanupHeadReason(ctx context.Context, n int, st *IssueState, revision, head, submitted string) string {
	if head == submitted && head != "" {
		return ""
	}
	if head == "" {
		return "local head unavailable"
	}
	heads := append([]string{st.HeadSHA, st.SubmittedHeadSHA}, st.CheckpointHeads...)
	rebases := append([]coordination.CheckpointRebase(nil), st.RebaseTransitions...)
	if st.LastRebase != nil {
		rebases = coordination.AppendCheckpointRebases(rebases, st.LastRebase.checkpoint())
	}
	if reader, ok := m.store.(interface {
		CheckpointHistory(context.Context, int, string) (*coordination.CheckpointHistory, error)
	}); ok {
		history, err := reader.CheckpointHistory(ctx, n, revision)
		if err != nil {
			return "checkpoint history unavailable: " + err.Error()
		}
		heads = append(heads, history.Heads...)
		rebases = coordination.AppendCheckpointRebases(rebases, history.Rebases...)
	}
	found := false
	for _, h := range heads {
		if h == head {
			found = true
			break
		}
	}
	if found {
		ancestor, err := workspace.BaseIsAncestor(ctx, m.repoRoot, head, submitted)
		if err != nil {
			return "checkpoint ancestry unavailable: " + err.Error()
		}
		if ancestor {
			return ""
		}
	}
	// A verified rebase's old endpoint is lineage on its own; any other
	// head must already be a recorded checkpoint.
	if !found && !rebaseStartsAt(rebases, head) {
		return "head is not in recorded checkpoint lineage"
	}
	if st.ForkRepo != "" || len(rebases) == 0 {
		return "recorded checkpoint is not an ancestor of submitted tip"
	}
	return m.rebasePathReason(ctx, head, submitted, rebases)
}

func rebaseStartsAt(rebases []coordination.CheckpointRebase, head string) bool {
	for _, r := range rebases {
		if verifiedRebase(r) && r.OldHead == head {
			return true
		}
	}
	return false
}

// rebasePathReason searches directed paths from head to submitted. The first
// edge must be a verified rebase whose old head is exactly head; later edges
// are Git ancestry or verified rebases. Evidence that cannot be evaluated is
// never an edge.
func (m *Manager) rebasePathReason(ctx context.Context, head, submitted string, rebases []coordination.CheckpointRebase) string {
	var verified []coordination.CheckpointRebase
	for _, r := range rebases {
		if verifiedRebase(r) {
			verified = append(verified, r)
		}
	}
	if len(verified) == 0 {
		return "recorded checkpoint is not an ancestor of submitted tip and no rebase has verified evidence"
	}
	if !rebaseStartsAt(verified, head) {
		return "recorded checkpoint is not an ancestor of submitted tip or the old head of a verified rebase"
	}
	// Equal heads still go through Git so every endpoint must be a commit.
	contains := func(a, b string) bool {
		ok, err := workspace.BaseIsAncestor(ctx, m.repoRoot, a, b)
		return err == nil && ok
	}
	visited := map[string]bool{head: true}
	queue := []string{head}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, r := range verified {
			if visited[r.NewHead] || (cur == head && r.OldHead != head) || !contains(cur, r.OldHead) {
				continue
			}
			if contains(r.NewHead, submitted) {
				return ""
			}
			visited[r.NewHead] = true
			queue = append(queue, r.NewHead)
		}
	}
	return "recorded checkpoint is not an ancestor of submitted tip and no verified rebase path reaches it"
}
