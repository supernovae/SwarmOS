package lease

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
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
	body := RenderStateProjection(wu, st)
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
