package lease

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

type CleanupResource struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	ExpectedHead string `json:"expected_head,omitempty"`
	CurrentHead  string `json:"current_head,omitempty"`
	Retired      bool   `json:"retired,omitempty"`
	Eligible     bool   `json:"eligible"`
	Absent       bool   `json:"absent"`
	Reason       string `json:"reason"`
}
type CleanupPreview struct {
	Blocked          string            `json:"blocked,omitempty"`
	RemoteApplicable bool              `json:"remote_applicable"`
	RemoteDeleted    bool              `json:"remote_deleted,omitempty"`
	Status           string            `json:"status"`
	Error            string            `json:"error,omitempty"`
	Kind             string            `json:"kind"`
	IssueNumber      int               `json:"issue_number"`
	Outcome          *PROutcome        `json:"outcome"`
	Resources        []CleanupResource `json:"resources"`
}

// InspectCleanup never acquires a claim or reservation, updates projections,
// fetches refs, or changes worktree files. Apply reinspects eligible resources
// before retiring them; outside filesystem writers must be stopped.
func InspectCleanup(ctx context.Context, root string, tr tracker.Tracker, m *Manager, n int) (*CleanupPreview, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve cleanup root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve cleanup root: %w", err)
	}
	root = filepath.Clean(root)
	st, o, err := m.InspectSubmission(ctx, n)
	if err != nil {
		return nil, err
	}
	p := &CleanupPreview{Kind: "cleanup", IssueNumber: n, Outcome: o}
	reason := m.cleanupPolicy(ctx, st, o, false)
	p.Blocked = reason
	p.RemoteApplicable = !m.cleanupLocal()
	r := CleanupResource{Kind: "remote-code-branch", Name: st.Branch, ExpectedHead: o.SubmittedHeadSHA, Reason: reason}
	if reason == "" && p.RemoteApplicable {
		out, e := cleanupGitOutput(ctx, root, "ls-remote", "--refs", "origin", "refs/heads/"+st.Branch)
		if e != nil {
			return nil, e
		}
		fields := strings.Fields(out)
		if len(fields) == 0 {
			r.Absent = true
			r.Reason = "already absent"
		} else if len(fields) != 2 || fields[1] != "refs/heads/"+st.Branch {
			r.Reason = "unexpected remote ref identity"
		} else {
			r.CurrentHead = fields[0]
			if r.CurrentHead != r.ExpectedHead {
				r.Reason = "remote branch advanced since submission"
			} else {
				r.Eligible = true
				r.Reason = "verified merged submission"
			}
		}
	}
	if !p.RemoteApplicable {
		r.Reason = "remote branch verification not applicable to local backend"
	}
	p.Resources = append(p.Resources, r)
	var retiring []string
	for _, item := range []struct{ path, branch string }{{workspace.WorktreePath(root, n), st.Branch}, {workspace.PRWorktreePath(root, st.PRNumber), workspace.PRBranchName(st.PRNumber)}} {
		i := workspace.InspectCleanupWorktree(ctx, root, item.path, item.branch, o.SubmittedHeadSHA)
		r := CleanupResource{Kind: "worktree", Name: item.path, ExpectedHead: o.SubmittedHeadSHA, CurrentHead: i.Head, Absent: i.Absent, Eligible: i.Eligible, Reason: i.Reason}
		if reason != "" {
			r.Eligible = false
			r.Reason = reason
		}
		if r.Eligible {
			retiring = append(retiring, item.path)
		}
		p.Resources = append(p.Resources, r)
	}
	for _, b := range []string{st.Branch, workspace.PRBranchName(st.PRNumber)} {
		i := workspace.InspectCleanupBranch(ctx, root, b, o.SubmittedHeadSHA, retiring)
		r := CleanupResource{Kind: "local-code-branch", Name: b, ExpectedHead: o.SubmittedHeadSHA, CurrentHead: i.Head, Absent: i.Absent, Eligible: i.Eligible, Reason: i.Reason}
		if reason != "" {
			r.Eligible = false
			r.Reason = reason
		}
		p.Resources = append(p.Resources, r)
	}
	for _, name := range []string{fmt.Sprintf("refs/heads/swarm-state/issue-%d", n), "refs/heads/swarm-contributors/*"} {
		p.Resources = append(p.Resources, CleanupResource{Kind: "ledger", Name: name, Reason: "retained authoritative state and provenance"})
	}
	return p, nil
}

func cleanupGitOutput(ctx context.Context, root string, args ...string) (string, error) {
	b, e := gitcredential.Output(ctx, root, args...)
	return string(b), e
}
