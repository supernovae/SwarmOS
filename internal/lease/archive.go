package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
	Kind          string                `json:"kind"`
	Status        string                `json:"status"`
	Issue         int                   `json:"issue"`
	Checks        []ArchiveCheck        `json:"checks"`
	Refs          []contract.ArchiveRef `json:"refs"`
	StateTip      string                `json:"state_tip,omitempty"`
	CommitterTime *time.Time            `json:"committer_time,omitempty"`
	Cutoff        *time.Time            `json:"cutoff,omitempty"`
	AfterDays     int                   `json:"after_days,omitempty"`
	Eligible      bool                  `json:"eligible"`
	Error         string                `json:"error,omitempty"`
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

var errArchiveIneligible = errors.New("archive refused")

func archiveEligible(report *ArchiveReport) error {
	var reasons []string
	for _, c := range report.Checks {
		if !c.Eligible {
			reasons = append(reasons, c.Name+": "+c.Reason)
		}
	}
	if len(reasons) > 0 {
		return fmt.Errorf("%w: %s", errArchiveIneligible, strings.Join(reasons, "; "))
	}
	return nil
}
func (m *Manager) ArchiveIssue(ctx context.Context, n int, destination string, apply bool) (*ArchiveReport, error) {
	return m.archiveIssue(ctx, n, destination, apply, nil, 0)
}
func (m *Manager) archiveIssue(ctx context.Context, n int, destination string, apply bool, cutoff *time.Time, days int) (*ArchiveReport, error) {
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
	if apply {
		if _, _, err = s.Load(ctx, n); err != nil {
			return nil, err
		}
	}
	report, st, err := m.archiveEligibility(ctx, s, n)
	if err != nil {
		return report, err
	}
	if cutoff != nil {
		if err = archiveAge(ctx, s, report, cutoff, days); err != nil {
			return report, err
		}
	}
	if err = archiveEligible(report); err != nil {
		return report, err
	}
	report.Eligible = true
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
	manifest := &contract.ArchiveManifest{Version: 1, Repository: repository, Issue: n, Namespace: s.Layout().Name(), Destination: destination, Refs: report.Refs, StateTip: tip, PROutcome: outcome, Actor: actor, ArchivedAt: m.nowFn().UTC(), Cutoff: cutoff}
	err = s.Archive(ctx, manifest, func(ctx context.Context) error {
		r, _, e := m.archiveEligibility(ctx, s, n)
		if e != nil {
			return e
		}
		if cutoff != nil {
			if e = archiveAge(ctx, s, r, cutoff, days); e != nil {
				return e
			}
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

func archiveAge(ctx context.Context, s *coordination.GitStore, r *ArchiveReport, cutoff *time.Time, days int) error {
	for _, ref := range r.Refs {
		if ref.Source == s.Layout().StateRef(r.Issue) {
			r.StateTip = ref.SHA
		}
	}
	stamp, err := s.ArchiveCommitTime(ctx, r.StateTip)
	if err != nil {
		return err
	}
	r.CommitterTime, r.Cutoff, r.AfterDays = &stamp, cutoff, days
	r.Checks = append(r.Checks, ArchiveCheck{"age", stamp.Before(*cutoff), "state-tip committer time must be strictly before cutoff"})
	return nil
}

type BulkArchiveTotals struct {
	Candidates int `json:"candidates"`
	Eligible   int `json:"eligible"`
	Previewed  int `json:"previewed"`
	Archived   int `json:"archived"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
}
type BulkArchiveReport struct {
	Kind      string            `json:"kind"`
	Status    string            `json:"status"`
	Cutoff    *time.Time        `json:"cutoff,omitempty"`
	AfterDays int               `json:"after_days,omitempty"`
	Issues    []*ArchiveReport  `json:"issues"`
	Totals    BulkArchiveTotals `json:"totals"`
	Error     string            `json:"error,omitempty"`
}

const ArchiveKeepReason = "archive disabled by strategy: keep; opt in with archive.strategy: repo on the upstream default branch"

// ArchiveAllEligible runs independent verified transactions, continuing after failures.
// Candidates = previewed + archived + skipped + failed; eligible also includes failed transfers.
func (m *Manager) ArchiveAllEligible(ctx context.Context, policy *contract.ArchiveConfig, destination string, apply bool) (*BulkArchiveReport, error) {
	out := &BulkArchiveReport{Kind: "archive", Status: "preview", Issues: []*ArchiveReport{}}
	s, _, _, err := m.archiveOperator(ctx)
	if err != nil {
		return out, err
	}
	if err = contract.ValidateArchiveConfig(policy); err != nil {
		return out, err
	}
	if policy.EffectiveStrategy() == "keep" && apply {
		return out, fmt.Errorf("%s", ArchiveKeepReason)
	}
	if destination == "" && policy.EffectiveStrategy() == "repo" {
		destination = "https://github.com/" + policy.Repo + ".git"
	}
	if destination != "" {
		if err = contract.ValidateArchiveDestination(destination); err != nil {
			return out, err
		}
	}
	if policy.EffectiveStrategy() == "repo" {
		out.AfterDays = *policy.AfterDays
		cutoff := m.nowFn().UTC().Add(-time.Duration(out.AfterDays) * 24 * time.Hour)
		out.Cutoff = &cutoff
	}
	candidates, err := s.ArchiveCandidates(ctx)
	if err != nil {
		return out, err
	}
	out.Totals.Candidates = len(candidates)
	var failures []error
	for _, candidate := range candidates {
		r := &ArchiveReport{Kind: "archive", Issue: candidate.Issue, StateTip: candidate.StateTip}
		var e error
		if policy.EffectiveStrategy() == "keep" {
			r.Status = "skipped"
			r.Checks = []ArchiveCheck{{"policy", false, ArchiveKeepReason}}
		} else {
			r, e = m.archiveIssue(ctx, candidate.Issue, destination, apply, out.Cutoff, out.AfterDays)
			if r == nil {
				r = &ArchiveReport{Kind: "archive", Issue: candidate.Issue}
			}
			if e != nil {
				// Only explicit pre-transfer safety refusals are harmless skips.
				if !r.Eligible && errors.Is(e, errArchiveIneligible) {
					r.Status = "skipped"
				} else {
					r.Status = "failed"
					r.Error = e.Error()
					failures = append(failures, fmt.Errorf("issue #%d: %w", candidate.Issue, e))
				}
			} else if apply {
				r.Status = "archived"
			} else {
				r.Status = "preview"
			}
		}
		if r.Eligible {
			out.Totals.Eligible++
		}
		switch r.Status {
		case "skipped":
			out.Totals.Skipped++
		case "failed":
			out.Totals.Failed++
		case "archived":
			out.Totals.Archived++
		case "preview":
			out.Totals.Previewed++
		}
		out.Issues = append(out.Issues, r)
	}
	if len(failures) > 0 {
		out.Status = "failed"
		return out, errors.Join(failures...)
	}
	if apply {
		out.Status = "completed"
	}
	return out, nil
}

// ArchivePolicyIssue applies trusted opt-in policy to the explicit-issue CLI.
func (m *Manager) ArchivePolicyIssue(ctx context.Context, n int, policy *contract.ArchiveConfig, destination string, apply bool) (*ArchiveReport, error) {
	if _, _, _, err := m.archiveOperator(ctx); err != nil {
		return nil, err
	}
	if err := contract.ValidateArchiveConfig(policy); err != nil {
		return nil, err
	}
	if policy.EffectiveStrategy() == "keep" {
		if apply {
			return nil, fmt.Errorf("%s", ArchiveKeepReason)
		}
		return &ArchiveReport{Kind: "archive", Status: "preview", Issue: n, Checks: []ArchiveCheck{{"policy", false, ArchiveKeepReason}}}, nil
	}
	if destination == "" {
		destination = "https://github.com/" + policy.Repo + ".git"
	}
	return m.ArchiveIssue(ctx, n, destination, apply)
}
