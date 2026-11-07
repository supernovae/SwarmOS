package lease

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/tracker"
)

type ArchiveCheck struct {
	Name     string `json:"name"`
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}
type ArchiveReport struct {
	Kind   string                `json:"kind"`
	Status string                `json:"status"`
	Issue  int                   `json:"issue"`
	Checks []ArchiveCheck        `json:"checks"`
	Refs   []contract.ArchiveRef `json:"refs"`
	Error  string                `json:"error,omitempty"`
}

func (m *Manager) archiveOperator(ctx context.Context) (*coordination.GitStore, string, string, error) {
	if m.configErr != nil {
		return nil, "", "", m.configErr
	}
	if m.gateway != nil || m.tracker.BackendName() != "github" {
		return nil, "", "", fmt.Errorf("archive requires an upstream GitHub repository operator")
	}
	s, ok := m.store.(*coordination.GitStore)
	if !ok {
		return nil, "", "", fmt.Errorf("archive requires a Git coordination store")
	}
	if err := m.tracker.Authorize(ctx, "approve-task"); err != nil {
		return nil, "", "", err
	}
	actor, err := m.tracker.CurrentUser(ctx)
	if err != nil {
		return nil, "", "", err
	}
	if actor == "" {
		return nil, "", "", fmt.Errorf("archive operator identity missing")
	}
	tr, ok := m.tracker.(interface{ UpstreamRepo() string })
	if !ok || tr.UpstreamRepo() == "" {
		return nil, "", "", fmt.Errorf("archive repository identity missing")
	}
	return s, actor, tr.UpstreamRepo(), nil
}
func (m *Manager) archiveEligibility(ctx context.Context, s *coordination.GitStore, n int) (*ArchiveReport, *IssueState, error) {
	report := &ArchiveReport{Kind: "archive", Status: "preview", Issue: n, Checks: []ArchiveCheck{}}
	add := func(name string, ok bool, reason string) {
		report.Checks = append(report.Checks, ArchiveCheck{name, ok, reason})
	}
	issue, err := m.tracker.GetIssue(ctx, n)
	if err != nil {
		return report, nil, err
	}
	if issue == nil || issue.Number != n {
		return report, nil, fmt.Errorf("archive issue identity mismatch")
	}
	add("closed-issue", issue.State == "closed", "issue must be closed")
	raw, _, err := s.LoadCheckpointPreview(ctx, n)
	if err != nil {
		return report, nil, err
	}
	var st IssueState
	if err = json.Unmarshal(raw, &st); err != nil {
		return report, nil, err
	}
	if st.Lease.IssueNumber != n {
		return report, nil, fmt.Errorf("archive state issue mismatch")
	}
	add("lease", st.Lease.Status == "draft" || st.Lease.Status == "released" || st.Lease.Status == "completed" || st.Lease.Status == "revoked", "valid ended or unclaimed lease; no active or handoff lease")
	add("pr-revision", st.PRRevision == nil, "no PR revision")
	add("cleanup", st.Cleanup == nil, "no cleanup reservation")
	outcome := "none"
	if st.PRNumber > 0 {
		pr, e := m.tracker.GetPullRequest(ctx, st.PRNumber)
		if e != nil {
			return report, nil, e
		}
		if pr == nil || pr.Number != st.PRNumber {
			return report, nil, fmt.Errorf("archive PR identity mismatch")
		}
		if pr.Merged {
			outcome = "merged"
		} else if pr.State == "closed" {
			outcome = "closed"
		} else {
			outcome = "open"
		}
	}
	add("pr-outcome", outcome != "open", "linked PR must be merged, closed, or absent")
	branchAbsent := true
	if st.Branch != "" {
		out, e := cleanupGitOutput(ctx, m.repoRoot, "ls-remote", "--refs", m.UpstreamRemote(), "refs/heads/"+st.Branch)
		if e != nil {
			return report, nil, e
		}
		branchAbsent = strings.TrimSpace(out) == ""
	}
	add("task-branch", branchAbsent, "remote task branch must be absent")
	referenced, err := s.ArchiveContributorReferences(ctx, n)
	if err != nil {
		return report, nil, err
	}
	add("contributor-index", !referenced, "no contributor index references")
	issues, err := m.tracker.ListIssues(ctx, tracker.IssueFilter{State: "open"})
	if err != nil {
		return report, nil, err
	}
	dependent := false
	for _, candidate := range issues {
		if candidate == nil {
			return report, nil, fmt.Errorf("incomplete dependent issue evidence")
		}
		if candidate.IsPullRequest || candidate.IsParent() {
			continue
		}
		cleanBody := contract.StripHTMLComments(candidate.Body)
		if strings.Contains(cleanBody, "swarm-workunit-path:") {
			return report, nil, fmt.Errorf("cannot verify external dependency evidence for issue #%d; inline its WorkUnit before archiving", candidate.Number)
		}
		workUnit := strings.Contains(cleanBody, "swarm-workunit") || strings.Contains(cleanBody, "### Objective")
		for _, label := range candidate.Labels {
			if strings.HasPrefix(label, "swarm:") && label != "swarm:community" {
				workUnit = true
			}
		}
		wu, e := contract.ParseWorkUnitFromIssue(candidate.Number, candidate.Title, candidate.Body)
		if e != nil {
			if !workUnit {
				continue
			}
			return report, nil, fmt.Errorf("parse open WorkUnit #%d: %w", candidate.Number, e)
		}
		for _, dep := range wu.DependsOn {
			if dep == n {
				dependent = true
			}
		}
	}
	add("open-dependents", !dependent, "no open WorkUnit depends on this issue")
	report.Refs, _, err = s.ArchiveSnapshot(ctx, n)
	return report, &st, err
}
func archiveEligible(report *ArchiveReport) error {
	var reasons []string
	for _, c := range report.Checks {
		if !c.Eligible {
			reasons = append(reasons, c.Name+": "+c.Reason)
		}
	}
	if len(reasons) > 0 {
		return fmt.Errorf("archive refused: %s", strings.Join(reasons, "; "))
	}
	return nil
}
func (m *Manager) ArchiveIssue(ctx context.Context, n int, destination string, apply bool) (*ArchiveReport, error) {
	s, actor, repository, err := m.archiveOperator(ctx)
	if err != nil {
		return nil, err
	}
	if destination != "" {
		if err = contract.ValidateArchiveDestination(destination); err != nil {
			return nil, err
		}
	} else if apply {
		return nil, fmt.Errorf("archive --apply requires --to")
	}
	if destination != "" {
		if err = s.CheckArchiveDestination(ctx, destination); err != nil {
			return nil, err
		}
	}
	report, st, err := m.archiveEligibility(ctx, s, n)
	if err != nil {
		return report, err
	}
	if err = archiveEligible(report); err != nil {
		return report, err
	}
	if !apply {
		return report, nil
	}
	outcome := "none"
	if st.PRNumber > 0 {
		pr, e := m.tracker.GetPullRequest(ctx, st.PRNumber)
		if e != nil {
			return report, e
		}
		if pr.Merged {
			outcome = "merged"
		} else if pr.State == "closed" {
			outcome = "closed"
		} else {
			return report, fmt.Errorf("PR reopened")
		}
	}
	var tip string
	for _, r := range report.Refs {
		if r.Source == s.Layout().StateRef(n) {
			tip = r.SHA
		}
	}
	manifest := &contract.ArchiveManifest{Version: 1, Repository: repository, Issue: n, Namespace: s.Layout().Name(), Destination: destination, Refs: report.Refs, StateTip: tip, PROutcome: outcome, Actor: actor, ArchivedAt: m.nowFn().UTC()}
	err = s.Archive(ctx, manifest, func(ctx context.Context) error {
		r, _, e := m.archiveEligibility(ctx, s, n)
		if e != nil {
			return e
		}
		return archiveEligible(r)
	})
	if err == nil {
		report.Status = "completed"
	}
	return report, err
}
func (m *Manager) RestoreIssueArchive(ctx context.Context, n int, apply bool) (*ArchiveReport, error) {
	s, _, repository, err := m.archiveOperator(ctx)
	if err != nil {
		return nil, err
	}
	report := &ArchiveReport{Kind: "archive", Status: "preview", Issue: n}
	tomb, err := s.RestoreArchive(ctx, n, repository, apply)
	if tomb != nil {
		report.Refs = append([]contract.ArchiveRef{}, tomb.Manifest.Refs...)
		if tomb.Manifest.Namespace != s.Layout().Name() {
			for i := range report.Refs {
				if report.Refs[i].SHA == tomb.Manifest.StateTip && strings.HasSuffix(report.Refs[i].Destination, "/state") {
					report.Refs[i].Source = s.Layout().StateRef(n)
				} else {
					report.Refs[i].Source = s.Layout().CheckpointRef(n, report.Refs[i].SHA)
				}
			}
		}
	}
	if err == nil && apply {
		report.Status = "completed"
	}
	return report, err
}

func (m *Manager) checkArchiveAdmission(ctx context.Context, n int) error {
	if s, ok := m.store.(interface {
		CheckArchived(context.Context, int) error
	}); ok {
		return s.CheckArchived(ctx, n)
	}
	return nil
}
