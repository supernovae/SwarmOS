package lease

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

// CleanupSnapshot holds one paginated listing of every PR for previewing many
// issues. List-endpoint heads are not re-resolved and pages can shift while
// listing, so snapshot eligibility is advisory: apply always reinspects with
// fresh tracker reads and never receives a snapshot.
type CleanupSnapshot struct {
	prs  map[int]*tracker.PullRequest
	open []*tracker.PullRequest
	err  error
}

// SnapshotCleanup lists PRs once. A listing failure is retained and makes
// every snapshot lookup fail, so it can never grant eligibility.
func (m *Manager) SnapshotCleanup(ctx context.Context) *CleanupSnapshot {
	s := &CleanupSnapshot{prs: map[int]*tracker.PullRequest{}}
	prs, err := m.tracker.ListPullRequests(ctx, tracker.PullRequestFilter{State: "all"})
	if err != nil {
		s.err = fmt.Errorf("list pull requests: %w", err)
		return s
	}
	for _, pr := range prs {
		if pr == nil {
			s.err = fmt.Errorf("pull request identity unavailable")
			return s
		}
		// Keep the most recently updated copy if pagination repeated a PR.
		if s.prs[pr.Number] != nil {
			continue
		}
		s.prs[pr.Number] = pr
		if pr.State == "open" {
			s.open = append(s.open, pr)
		}
	}
	return s
}

func (s *CleanupSnapshot) pullRequest(n int) (*tracker.PullRequest, error) {
	if s.err != nil {
		return nil, s.err
	}
	pr := s.prs[n]
	if pr == nil {
		return nil, fmt.Errorf("PR #%d is absent from the tracker listing", n)
	}
	return pr, nil
}

func (s *CleanupSnapshot) openPullRequests() ([]*tracker.PullRequest, error) {
	return s.open, s.err
}

// InspectCleanupSnapshot previews issue n like InspectCleanup but reads PR
// evidence from snap. An issue without a recorded submission is retained.
func InspectCleanupSnapshot(ctx context.Context, root string, m *Manager, n int, snap *CleanupSnapshot) (*CleanupPreview, error) {
	if snap == nil {
		return nil, fmt.Errorf("cleanup snapshot required")
	}
	return inspectCleanup(ctx, root, m, n, snap)
}

func unsubmittedCleanupPreview(ctx context.Context, root string, m *Manager, n int, st *IssueState) *CleanupPreview {
	const reason = "no recorded PR submission"
	p := &CleanupPreview{Kind: "cleanup", IssueNumber: n, Blocked: reason, RemoteApplicable: !m.cleanupLocal()}
	p.Resources = append(p.Resources, CleanupResource{Kind: "remote-code-branch", Name: st.Branch, Reason: reason})
	i := workspace.InspectCleanupWorktree(ctx, root, workspace.WorktreePath(root, n), st.Branch, "")
	p.Resources = append(p.Resources, CleanupResource{Kind: "worktree", Name: workspace.WorktreePath(root, n), CurrentHead: i.Head, Absent: i.Absent, Reason: reason})
	if st.Branch != "" {
		b := workspace.InspectCleanupBranch(ctx, root, st.Branch, "", nil)
		p.Resources = append(p.Resources, CleanupResource{Kind: "local-code-branch", Name: st.Branch, CurrentHead: b.Head, Absent: b.Absent, Reason: reason})
	}
	for _, name := range []string{fmt.Sprintf("refs/heads/swarm-state/issue-%d", n), "refs/heads/swarm-contributors/*"} {
		p.Resources = append(p.Resources, CleanupResource{Kind: "ledger", Name: name, Reason: "retained authoritative state and provenance"})
	}
	return p
}

// UnresolvedCleanupWorktree is a discovered worktree no issue authoritatively
// records. It is always retained.
type UnresolvedCleanupWorktree struct {
	workspace.CleanupWorktree
	Reason string `json:"reason"`
}

var cleanupClosingReference = regexp.MustCompile(`(?im)^\s*(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

// CleanupCandidate is an issue to inspect and the discovered worktrees that
// led to it.
type CleanupCandidate struct {
	IssueNumber int
	Worktrees   []workspace.CleanupWorktree
}

// ResolveCleanupWorktrees maps discovered worktrees to issues. PR bodies and
// tracker issue fields are only hints: a PR worktree belongs to an issue only
// when that issue's authoritative state records the PR. Issues are listed at
// most once, and only when hints cannot resolve a PR worktree.
func (m *Manager) ResolveCleanupWorktrees(ctx context.Context, snap *CleanupSnapshot, worktrees []workspace.CleanupWorktree) ([]CleanupCandidate, []UnresolvedCleanupWorktree) {
	states := map[int]*IssueState{}
	loaded := map[int]bool{}
	load := func(n int) (*IssueState, error) {
		if loaded[n] {
			return states[n], nil
		}
		st, _, err := m.loadReadOnly(ctx, n)
		if errors.Is(err, coordination.ErrNotFound) {
			loaded[n] = true
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		loaded[n], states[n] = true, st
		return st, nil
	}
	linked := func(pr int) []int {
		var out []int
		for n, st := range states {
			if st != nil && st.PRNumber == pr {
				out = append(out, n)
			}
		}
		sort.Ints(out)
		return out
	}
	issues := map[int][]workspace.CleanupWorktree{}
	var unresolved []UnresolvedCleanupWorktree
	var pending []workspace.CleanupWorktree
	for _, w := range worktrees {
		switch w.Kind {
		case "issue":
			if _, err := load(w.Number); errors.Is(err, coordination.ErrNotFound) {
				unresolved = append(unresolved, UnresolvedCleanupWorktree{w, "no authoritative coordination state"})
				continue
			}
			// Other load errors surface when the issue itself is inspected.
			issues[w.Number] = append(issues[w.Number], w)
		case "pr":
			pending = append(pending, w)
		default:
			unresolved = append(unresolved, UnresolvedCleanupWorktree{w, "unrecognized managed worktree name"})
		}
	}
	listed := false
	var listErr error
	for _, w := range pending {
		if len(linked(w.Number)) == 0 {
			if pr, err := snap.pullRequest(w.Number); err == nil {
				for _, hint := range cleanupLinkHints(pr) {
					_, _ = load(hint)
				}
			}
		}
		if len(linked(w.Number)) == 0 && !listed {
			listed = true
			all, err := m.tracker.ListIssues(ctx, tracker.IssueFilter{State: "all"})
			listErr = err
			for _, issue := range all {
				// An unreadable state cannot link this worktree; it is not fatal
				// to unrelated candidates.
				_, _ = load(issue.Number)
			}
		}
		switch candidates := linked(w.Number); {
		case len(candidates) == 1:
			issues[candidates[0]] = append(issues[candidates[0]], w)
		case len(candidates) > 1:
			unresolved = append(unresolved, UnresolvedCleanupWorktree{w, "multiple authoritative issue links"})
		case listErr != nil:
			unresolved = append(unresolved, UnresolvedCleanupWorktree{w, "no verified issue link: " + listErr.Error()})
		default:
			unresolved = append(unresolved, UnresolvedCleanupWorktree{w, "no verified issue link"})
		}
	}
	out := make([]CleanupCandidate, 0, len(issues))
	for n, w := range issues {
		out = append(out, CleanupCandidate{IssueNumber: n, Worktrees: w})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IssueNumber < out[j].IssueNumber })
	return out, unresolved
}

func cleanupLinkHints(pr *tracker.PullRequest) []int {
	var hints []int
	if pr.IssueNumber > 0 {
		hints = append(hints, pr.IssueNumber)
	}
	if match := cleanupClosingReference.FindStringSubmatch(pr.Body); match != nil {
		if n, err := strconv.Atoi(match[1]); err == nil && n > 0 && n != pr.IssueNumber {
			hints = append(hints, n)
		}
	}
	return hints
}
