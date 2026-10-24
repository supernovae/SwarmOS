package lease

import (
	"context"
	"fmt"
	"os"
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
	Eligible     bool   `json:"eligible"`
	Absent       bool   `json:"absent"`
	Reason       string `json:"reason"`
}
type CleanupPreview struct {
	RemoteDeleted bool              `json:"remote_deleted,omitempty"`
	Status        string            `json:"status"`
	Error         string            `json:"error,omitempty"`
	Kind          string            `json:"kind"`
	IssueNumber   int               `json:"issue_number"`
	Outcome       *PROutcome        `json:"outcome"`
	Resources     []CleanupResource `json:"resources"`
}

// InspectCleanup never acquires a claim or reservation, updates projections,
// fetches refs, or changes worktree files. Local resources are conservatively
// retained: Git cannot fence outside filesystem writers during removal.
func InspectCleanup(ctx context.Context, root string, tr tracker.Tracker, m *Manager, n int) (*CleanupPreview, error) {
	st, o, err := m.InspectSubmission(ctx, n)
	if err != nil {
		return nil, err
	}
	p := &CleanupPreview{Kind: "cleanup", IssueNumber: n, Outcome: o}
	reason := ""
	switch {
	case st.Lease.Status == "active" || st.PRRevision != nil:
		reason = "active attempt or pending PR revision"
	case st.Cleanup != nil:
		reason = "cleanup reservation is active; explicit recovery is required"
	case o.Status != "merged":
		reason = "recorded PR is not merged"
	case o.MergeCommitSHA == "" || o.MergedAt == nil || o.MergedAt.IsZero():
		reason = "merge evidence is incomplete"
	case o.SubmittedHeadSHA == "" || o.FinalHeadSHA != o.SubmittedHeadSHA:
		reason = "final PR head differs from recorded submission or submission evidence is missing"
	case st.ForkRepo != "":
		reason = "fork branch retirement is unsupported"
	case !strings.HasPrefix(st.Branch, fmt.Sprintf("swarm/issue-%d-", n)):
		reason = "branch is outside the issue task namespace"
	}
	upstream, err := workspace.ResolveRemoteGitHubSlug(root, "origin")
	if err != nil || !strings.EqualFold(upstream, o.BaseRepo) || !strings.EqualFold(upstream, o.HeadRepo) || o.HeadRepo == "" {
		reason = "origin and PR repository identity could not be verified"
	}
	if st.Branch == o.TargetBranch || o.TargetBranch == "" || st.Branch == "main" || st.Branch == "master" {
		reason = "base/default branches are retained"
	}
	if reason == "" {
		if e := m.verifyCleanupRemote(ctx, o); e != nil {
			reason = e.Error()
		}
	}
	prs, err := tr.ListPullRequests(ctx, tracker.PullRequestFilter{State: "open"})
	if err != nil {
		return nil, err
	}
	for _, pr := range prs {
		if pr == nil || pr.HeadRepo == "" {
			reason = "open PR repository identity is incomplete"
			continue
		}
		branch := pr.Head
		if i := strings.IndexByte(branch, ':'); i >= 0 {
			branch = branch[i+1:]
		}
		if strings.EqualFold(pr.HeadRepo, o.HeadRepo) && branch == st.Branch {
			reason = "branch is used by an open PR"
		}
	}
	r := CleanupResource{Kind: "remote-code-branch", Name: st.Branch, ExpectedHead: o.SubmittedHeadSHA, Reason: reason}
	if reason == "" {
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
	p.Resources = append(p.Resources, r)
	for _, b := range []string{st.Branch, workspace.PRBranchName(st.PRNumber)} {
		r := CleanupResource{Kind: "local-code-branch", Name: b, ExpectedHead: o.SubmittedHeadSHA, Reason: "retained: local checkout creation cannot be fenced against ref deletion"}

		if b != "" {
			out, e := cleanupGitOutput(ctx, root, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"+b)
			if e != nil {
				r.Reason = "local ref inspection failed"
			} else {
				for _, line := range strings.Split(out, "\n") {
					fields := strings.Fields(line)
					if len(fields) == 2 && fields[0] == "refs/heads/"+b {
						r.CurrentHead = fields[1]
					}
				}
				if r.CurrentHead == "" {
					r.Absent = true
					r.Reason = "already absent"
				} else if r.CurrentHead != r.ExpectedHead {
					r.Reason = "retained: local branch advanced since submission"
				}
			}
		}

		p.Resources = append(p.Resources, r)
	}
	for _, path := range []string{workspace.WorktreePath(root, n), workspace.PRWorktreePath(root, st.PRNumber)} {
		r := CleanupResource{Kind: "worktree", Name: path, ExpectedHead: o.SubmittedHeadSHA, Reason: "retained: Git removal can discard concurrently created ignored artifacts"}
		if _, e := os.Lstat(path); os.IsNotExist(e) {
			r.Absent = true
			r.Reason = "already absent"
		} else if e != nil {
			r.Reason = "worktree inspection failed"
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
