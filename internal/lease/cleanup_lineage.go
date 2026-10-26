package lease

import (
	"context"
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

func (m *Manager) cleanupHeadReason(ctx context.Context, n int, st *IssueState, revision, head, submitted string) string {
	if head == submitted && head != "" {
		return ""
	}
	if head == "" {
		return "local head unavailable"
	}
	heads := append([]string{st.HeadSHA, st.SubmittedHeadSHA}, st.CheckpointHeads...)
	if reader, ok := m.store.(interface {
		CheckpointLineage(context.Context, int, string) ([]string, error)
	}); ok {
		history, err := reader.CheckpointLineage(ctx, n, revision)
		if err != nil {
			return "checkpoint history unavailable: " + err.Error()
		}
		heads = append(heads, history...)
	}
	found := false
	for _, h := range heads {
		if h == head {
			found = true
			break
		}
	}
	if !found {
		return "head is not in recorded checkpoint lineage"
	}
	ancestor, err := workspace.BaseIsAncestor(ctx, m.repoRoot, head, submitted)
	if err != nil {
		return "checkpoint ancestry unavailable: " + err.Error()
	}
	if !ancestor {
		return "recorded checkpoint is not an ancestor of submitted tip"
	}
	return ""
}
