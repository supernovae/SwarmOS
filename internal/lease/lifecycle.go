package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
)

// PROutcome records GitHub's observation separately from the execution lease.
// SubmittedHeadSHA remains publication evidence even if the PR later advances.
type PROutcome struct {
	TargetBranch     string     `json:"target_branch"`
	Status           string     `json:"status"`
	Number           int        `json:"number"`
	URL              string     `json:"url"`
	HeadRepo         string     `json:"head_repo"`
	BaseRepo         string     `json:"base_repo"`
	Branch           string     `json:"branch"`
	SubmittedHeadSHA string     `json:"submitted_head_sha"`
	FinalHeadSHA     string     `json:"final_head_sha"`
	MergeCommitSHA   string     `json:"merge_commit_sha,omitempty"`
	MergedAt         *time.Time `json:"merged_at,omitempty"`
	ObservedAt       time.Time  `json:"observed_at"`
}

// InspectSubmission reads the recorded PR directly; issue closure is never
// evidence of a merge. It performs no state or tracker writes.
func (m *Manager) InspectSubmission(ctx context.Context, n int) (*IssueState, *PROutcome, error) {
	st, _, err := m.loadReadOnly(ctx, n)
	if err != nil {
		return nil, nil, err
	}
	o, err := m.observeSubmission(ctx, st)
	return st, o, err
}

func (m *Manager) observeSubmission(ctx context.Context, st *IssueState) (*PROutcome, error) {
	if st.PRNumber <= 0 || st.PRURL == "" || st.Branch == "" {
		return nil, fmt.Errorf("submission lacks recorded PR identity")
	}
	p, err := m.tracker.GetPullRequest(ctx, st.PRNumber)
	if err != nil {
		return nil, err
	}
	return m.submissionOutcome(st, p)
}

// submissionOutcome validates one tracker observation of the recorded PR.
func (m *Manager) submissionOutcome(st *IssueState, p *tracker.PullRequest) (*PROutcome, error) {
	if p == nil || p.Number != st.PRNumber || p.HTMLURL != st.PRURL {
		return nil, fmt.Errorf("recorded PR identity differs from tracker")
	}
	branch := p.Head
	if i := strings.IndexByte(branch, ':'); i >= 0 {
		branch = branch[i+1:]
	}
	if branch != st.Branch || p.HeadSHA == "" || (p.IssueNumber != 0 && p.IssueNumber != st.Lease.IssueNumber) {
		return nil, fmt.Errorf("PR branch/head/issue identity mismatch")
	}
	if provider, ok := m.tracker.(interface{ UpstreamRepo() string }); ok {
		upstream := provider.UpstreamRepo()
		headRepo := upstream
		if st.ForkRepo != "" {
			headRepo = st.ForkRepo
		}
		if upstream == "" || !strings.EqualFold(p.BaseRepo, upstream) || !strings.EqualFold(p.HeadRepo, headRepo) {
			return nil, fmt.Errorf("PR repository identity mismatch")
		}
	}
	if old := st.PROutcome; old != nil && (old.Number != p.Number || old.Branch != branch || !strings.EqualFold(old.HeadRepo, p.HeadRepo) || !strings.EqualFold(old.BaseRepo, p.BaseRepo)) {
		return nil, fmt.Errorf("PR identity changed since previous observation")
	}
	status := ""
	switch strings.ToLower(p.State) {
	case "open":
		if p.Merged || p.MergedAt != nil {
			return nil, fmt.Errorf("contradictory open PR merge evidence")
		}
		status = "open"
	case "closed":
		status = "closed-unmerged"
		if p.Merged {
			status = "merged"
		} else if p.MergedAt != nil {
			return nil, fmt.Errorf("contradictory closed PR merge evidence")
		}
	default:
		return nil, fmt.Errorf("unknown PR state %q", p.State)
	}
	submitted := st.SubmittedHeadSHA
	// Old completed/submitted records already retained the checkpoint head.
	if submitted == "" && st.Lease.Status == "completed" && st.Lease.Phase == "submitted" {
		submitted = st.HeadSHA
	}
	return &PROutcome{Status: status, TargetBranch: p.Base, Number: p.Number, URL: p.HTMLURL, HeadRepo: p.HeadRepo, BaseRepo: p.BaseRepo, Branch: branch, SubmittedHeadSHA: submitted, FinalHeadSHA: p.HeadSHA, MergeCommitSHA: p.MergeCommitSHA, MergedAt: p.MergedAt, ObservedAt: m.nowFn().UTC()}, nil
}

// ReconcileSubmission is an explicit operator mutation. A successful CAS can
// precede a projection failure; rerunning repairs projections without rewriting
// unchanged lifecycle evidence or creating a replacement audit comment.
func (m *Manager) ReconcileSubmission(ctx context.Context, n int) (*IssueState, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("submission reconciliation requires a repository operator")
	}
	if err := m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return nil, err
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if st.Lease.Status == "active" || st.PRRevision != nil {
		return nil, fmt.Errorf("submission has an active attempt or pending PR revision")
	}
	o, err := m.observeSubmission(ctx, st)
	if err != nil {
		return nil, err
	}
	if old := st.PROutcome; old != nil {
		o.ObservedAt = old.ObservedAt
	}
	if !reflect.DeepEqual(st.PROutcome, o) || st.SubmittedHeadSHA != o.SubmittedHeadSHA {
		o.ObservedAt = m.nowFn().UTC()
		st.PROutcome = o
		st.SubmittedHeadSHA = o.SubmittedHeadSHA
		raw, e := json.Marshal(st)
		if e != nil {
			return nil, e
		}
		if _, e = m.store.CompareAndSwap(ctx, n, rev, raw); e != nil {
			return nil, e
		}
	}
	if err = m.projectSubmission(ctx, n, st); err != nil {
		return st, fmt.Errorf("outcome recorded; rerun reconciliation to repair projections: %w", err)
	}
	return st, nil
}

func (m *Manager) projectSubmission(ctx context.Context, n int, st *IssueState) error {
	if st.PROutcome.Status == "open" {
		if err := m.tracker.AddLabels(ctx, n, []string{"swarm:pr-open"}); err != nil {
			return err
		}
	} else if err := m.tracker.RemoveLabel(ctx, n, "swarm:pr-open"); err != nil {
		return err
	}
	comments, err := m.tracker.ListComments(ctx, n)
	if err != nil {
		return err
	}
	var wu *contract.WorkUnit
	body := RenderStateProjection(wu, st, m.LedgerLayout())
	for _, c := range comments {
		if strings.HasPrefix(strings.TrimSpace(c.Body), "<!-- swarm:audit:v1 -->") {
			if c.Body == body {
				return nil
			}
			_, err = m.tracker.UpdateComment(ctx, c.ID, body)
			return err
		}
	}
	_, err = m.tracker.CreateComment(ctx, n, body)
	return err
}

// DecompositionParent verifies a WorkUnit or native parent hint against its recorded graph.
// Native relationships alone can never authorize a rollup.
func (m *Manager) DecompositionParent(ctx context.Context, n int) (int, error) {
	st, _, err := m.loadReadOnly(ctx, n)
	if err == nil && st.Decomposition != nil {
		return n, nil
	}
	if err != nil && !errors.Is(err, coordination.ErrNotFound) {
		return 0, err
	}
	// The WorkUnit parent is a lookup hint; membership in the parent's
	// coordination graph remains the authority even if native links drift.
	issue, err := m.tracker.GetIssue(ctx, n)
	if err != nil {
		return 0, err
	}
	parent := 0
	if wu, parseErr := contract.ParseWorkUnitFromIssue(n, issue.Title, issue.Body); parseErr == nil {
		parent = wu.ParentIssue
	}
	if parent == 0 {
		parent, err = m.tracker.ParentIssueNumber(ctx, n)
		if err != nil {
			return 0, err
		}
	}
	if parent == 0 {
		return 0, nil
	}
	p, _, err := m.loadReadOnly(ctx, parent)
	if errors.Is(err, coordination.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if p.Decomposition != nil {
		for _, c := range p.Decomposition.Children {
			if c.Number == n {
				return parent, nil
			}
		}
	}
	return 0, nil
}

// ReconcileDecomposition is an explicit, merge-based parent rollup. It never
// writes a child's claim, outcome, or artifacts, even when observation fails.
func (m *Manager) ReconcileDecomposition(ctx context.Context, n int) (*IssueState, error) {
	if m.gateway != nil {
		return nil, fmt.Errorf("parent reconciliation requires a repository operator")
	}
	if err := m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return nil, err
	}
	st, rev, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	d := st.Decomposition
	if d == nil || !d.Complete || st.Lease.Status == "active" || st.PRRevision != nil {
		return nil, fmt.Errorf("parent decomposition is not complete or has an active attempt")
	}
	closer, ok := m.tracker.(interface {
		CloseIssueCompleted(context.Context, int) error
	})
	if !ok {
		return nil, fmt.Errorf("tracker cannot close parent as completed")
	}
	evidence := map[int]*PROutcome{}
	revisions := map[int]string{}
	for _, c := range d.Children {
		child, childRev, err := m.loadReadOnly(ctx, c.Number)
		if err != nil {
			return st, fmt.Errorf("child #%d: %w", c.Number, err)
		}
		if child.Lease.Status == "active" || child.PRRevision != nil || child.Cleanup != nil {
			return st, fmt.Errorf("child #%d has an active attempt", c.Number)
		}
		issue, err := m.tracker.GetIssue(ctx, c.Number)
		if err != nil {
			return st, err
		}
		wu, err := contract.ParseWorkUnitFromIssue(c.Number, issue.Title, issue.Body)
		if err != nil || wu.ParentIssue != n {
			return st, fmt.Errorf("child #%d parent identity mismatch", c.Number)
		}
		outcome, err := m.observeSubmission(ctx, child)
		if err != nil {
			return st, fmt.Errorf("child #%d has no verified merge (closed or superseded issues still require a merge): %w", c.Number, err)
		}
		if outcome.Status != "merged" || outcome.MergeCommitSHA == "" || outcome.MergedAt == nil || outcome.MergedAt.IsZero() || outcome.SubmittedHeadSHA == "" || outcome.FinalHeadSHA != outcome.SubmittedHeadSHA || outcome.TargetBranch == "" || (child.BaseBranch != "" && child.BaseBranch != outcome.TargetBranch) {
			return st, fmt.Errorf("child #%d is %s or lacks complete merge evidence; closed or superseded is insufficient", c.Number, outcome.Status)
		}
		evidence[c.Number] = outcome
		revisions[c.Number] = childRev
	}
	// Reinspect the full child snapshot before committing parent intent.
	for child, expected := range revisions {
		_, actual, err := m.loadReadOnly(ctx, child)
		if err != nil {
			return st, err
		}
		if actual != expected {
			return st, fmt.Errorf("child #%d changed during reconciliation: %w", child, coordination.ErrConflict)
		}
	}
	d.Evidence = evidence
	d.CompletionIntent = true
	raw, err := marshalCoordinatorState(st)
	if err != nil {
		return nil, err
	}
	intentRev, err := m.store.CompareAndSwap(ctx, n, rev, raw)
	if err != nil {
		return nil, err
	}
	current, currentRev, err := m.load(ctx, n)
	if err != nil {
		return st, err
	}
	if currentRev != intentRev || current.Decomposition == nil || current.Decomposition.ID != d.ID || !current.Decomposition.CompletionIntent {
		return st, coordination.ErrConflict
	}
	for child, expected := range revisions {
		_, actual, err := m.loadReadOnly(ctx, child)
		if err != nil {
			return st, err
		}
		if actual != expected {
			return st, coordination.ErrConflict
		}
	}
	if err = closer.CloseIssueCompleted(ctx, n); err != nil {
		return st, fmt.Errorf("parent completion intent recorded; retry close: %w", err)
	}
	current.Decomposition.Closed = true
	return m.saveDecomposition(ctx, n, currentRev, current)
}

// DecompositionChildStatus is a read-only view in the recorded topological order.
type DecompositionChildStatus struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}
type DecompositionStatus struct {
	Children  []DecompositionChildStatus `json:"children"`
	Next      int                        `json:"next,omitempty"`
	NextState string                     `json:"next_state,omitempty"`
	AllMerged bool                       `json:"all_merged"`
}

func (m *Manager) InspectDecomposition(ctx context.Context, st *IssueState) *DecompositionStatus {
	if st == nil || st.Decomposition == nil {
		return nil
	}
	out := &DecompositionStatus{AllMerged: st.Decomposition.Complete}
	for _, c := range st.Decomposition.Children {
		row := DecompositionChildStatus{Number: c.Number, Title: c.WorkUnit.Title, State: "pending creation"}
		if c.Number > 0 {
			issue, err := m.tracker.GetIssue(ctx, c.Number)
			if err != nil {
				row.State = "unknown"
				row.Reason = err.Error()
			} else {
				child, _, stateErr := m.loadReadOnly(ctx, c.Number)
				row.State = "approval needed"
				if stateErr != nil && !errors.Is(stateErr, coordination.ErrNotFound) {
					row.State = "unknown"
					row.Reason = stateErr.Error()
				} else if child != nil && child.PRNumber > 0 {
					outcome, err := m.observeSubmission(ctx, child)
					if err != nil {
						row.State = "unknown"
						row.Reason = err.Error()
					} else if outcome.Status == "merged" && outcome.MergeCommitSHA != "" && outcome.MergedAt != nil && !outcome.MergedAt.IsZero() {
						row.State = "merged"
					} else {
						row.State = outcome.Status
					}
				} else if issue.State == "closed" {
					row.State = "closed-unmerged"
				} else {
					wu, err := contract.ParseWorkUnitFromIssue(c.Number, issue.Title, issue.Body)
					if err != nil {
						row.State = "unknown"
						row.Reason = err.Error()
					} else if err = m.VerifyDependencies(ctx, wu); err != nil {
						row.State = "waiting on prerequisite"
						row.Reason = err.Error()
					} else if child != nil {
						cfg, err := harness.LoadConfigAt(m.repoRoot, child.BaseSHA)
						if err != nil {
							row.State = "unknown"
							row.Reason = err.Error()
						} else if TaskApprovalCurrent(child, wu, cfg) && slices.Contains(issue.Labels, cfg.Security.ReadyLabel) {
							row.State = "workable"
							if child.Lease.Status == "active" && child.Lease.ExpiresAt.After(m.nowFn()) {
								row.State = "claimed"
							}
						}
					}
				}
			}
		}
		if row.State != "merged" {
			out.AllMerged = false
		}
		if out.Next == 0 && (row.State == "workable" || row.State == "approval needed") {
			out.Next = row.Number
			out.NextState = row.State
		}
		out.Children = append(out.Children, row)
	}
	return out
}
