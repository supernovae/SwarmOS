package tracker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/safefs"
	"strings"
)

type localState struct {
	ChildOrder    map[int][]int        `json:"child_order,omitempty"`
	NextIssueNum  int                  `json:"next_issue_num"`
	NextCommentID int64                `json:"next_comment_id"`
	NextPRNum     int                  `json:"next_pr_num"`
	Issues        map[int]*Issue       `json:"issues"`
	Parents       map[int]int          `json:"parents"`
	Comments      map[int][]*Comment   `json:"comments"`
	PullRequests  map[int]*PullRequest `json:"pull_requests"`
	DefinedLabels map[string]LabelSpec `json:"defined_labels"`
}

type LocalTracker struct {
	repoRoot         string
	statePath        string
	user             string
	assoc            string
	hookAfterComment func(issueNumber int, c *Comment)
}

func NewLocalTracker(repoRoot string) *LocalTracker {
	user := os.Getenv("SWARM_USER")
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		user = "local-dev"
	}
	return &LocalTracker{
		repoRoot:  repoRoot,
		statePath: filepath.Join(repoRoot, ".swarm", "local-tracker", "state.json"),
		user:      user,
		assoc:     "OWNER",
	}
}

func (l *LocalTracker) WithUser(user, association string) *LocalTracker {
	cp := *l
	cp.user = user
	if association != "" {
		cp.assoc = association
	}
	return &cp
}

func (l *LocalTracker) SetAfterCreateCommentHook(fn func(issueNumber int, c *Comment)) {
	l.hookAfterComment = fn
}

func (l *LocalTracker) BackendName() string {
	return "local"
}

func (l *LocalTracker) CurrentUser(_ context.Context) (string, error) {
	return l.user, nil
}

func (l *LocalTracker) Authorize(ctx context.Context, permission string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch permission {
	case "approve-task", "review-human", "edit-task":
		if l.assoc == "OWNER" || l.assoc == "MEMBER" || l.assoc == "COLLABORATOR" {
			return nil
		}
		return fmt.Errorf("local actor %q is not authorized for %s", l.user, permission)
	case "create-epic":
		if l.assoc == "OWNER" {
			return nil
		}
		return fmt.Errorf("local actor %q (association %q) is not authorized for %s; maintainer OWNER required", l.user, l.assoc, permission)
	default:
		return fmt.Errorf("unknown permission %q", permission)
	}
}

func (l *LocalTracker) CheckIssueType(ctx context.Context, issueType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !IsEpicIssueType(issueType) {
		return fmt.Errorf("unsupported issue type %q in local tracker (only \"Epic\" is supported)", issueType)
	}
	return nil
}

func (l *LocalTracker) withLock(mutate bool, fn func(st *localState) error) error {
	base, err := safefs.Open(l.repoRoot)
	if err != nil {
		return err
	}
	defer base.Close()
	state, err := base.Sub(filepath.Join(".swarm", "local-tracker"), true, 0700)
	if err != nil {
		return err
	}
	defer state.Close()
	lockFile, err := state.OpenLock("state.json.lock")
	if err != nil {
		return err
	}
	defer lockFile.Close()

	if err := filelock.LockExclusive(lockFile); err != nil {
		return fmt.Errorf("filelock failed: %w", err)
	}
	defer func() {
		_ = filelock.Unlock(lockFile)
	}()

	st := &localState{
		NextIssueNum:  1,
		NextCommentID: 1000,
		NextPRNum:     1,
		Issues:        make(map[int]*Issue),
		Parents:       make(map[int]int),
		Comments:      make(map[int][]*Comment),
		PullRequests:  make(map[int]*PullRequest),
		DefinedLabels: make(map[string]LabelSpec),
	}

	data, readErr := state.ReadFile("state.json", 64<<20)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, st); err != nil {
			return fmt.Errorf("corrupt local tracker state: %w", err)
		}
		if st.Issues == nil {
			st.Issues = make(map[int]*Issue)
		}
		if st.Parents == nil {
			st.Parents = make(map[int]int)
		}
		if st.Comments == nil {
			st.Comments = make(map[int][]*Comment)
		}
		if st.PullRequests == nil {
			st.PullRequests = make(map[int]*PullRequest)
		}
		if st.DefinedLabels == nil {
			st.DefinedLabels = make(map[string]LabelSpec)
		}
	}
	if st.ChildOrder == nil {
		st.ChildOrder = make(map[int][]int)
	}
	if err := fn(st); err != nil {
		return err
	}

	if mutate {
		out, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return err
		}
		return state.WriteFile("state.json", out, 0o600)
	}
	return nil
}

func (l *LocalTracker) GetIssue(_ context.Context, number int) (*Issue, error) {
	var res *Issue
	err := l.withLock(false, func(st *localState) error {
		iss, ok := st.Issues[number]
		if !ok {
			return fmt.Errorf("issue #%d not found in local tracker", number)
		}
		cp := *iss
		cp.Labels = append([]string(nil), iss.Labels...)
		res = &cp
		return nil
	})
	return res, err
}

func (l *LocalTracker) CreateIssue(_ context.Context, title, body string, labels []string) (*Issue, error) {
	var created *Issue
	err := l.withLock(true, func(st *localState) error {
		num := st.NextIssueNum
		st.NextIssueNum++
		now := time.Now().UTC()
		iss := &Issue{
			Number:            num,
			Title:             title,
			Body:              body,
			State:             "open",
			Labels:            uniqueStrings(labels),
			Author:            l.user,
			AuthorAssociation: l.assoc,
			HTMLURL:           fmt.Sprintf("local://issues/%d", num),
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		st.Issues[num] = iss
		cp := *iss
		created = &cp
		return nil
	})
	return created, err
}

func (l *LocalTracker) CreateTypedIssue(ctx context.Context, title, body string, labels []string, issueType string) (*Issue, error) {
	if IsEpicIssueType(issueType) {
		if err := l.Authorize(ctx, "create-epic"); err != nil {
			return nil, err
		}
	}
	if err := l.CheckIssueType(ctx, issueType); err != nil {
		return nil, err
	}
	canonicalType := "Epic"
	var created *Issue
	err := l.withLock(true, func(st *localState) error {
		num := st.NextIssueNum
		st.NextIssueNum++
		now := time.Now().UTC()
		iss := &Issue{
			Number:            num,
			Title:             title,
			Body:              body,
			State:             "open",
			Type:              canonicalType,
			Labels:            uniqueStrings(labels),
			Author:            l.user,
			AuthorAssociation: l.assoc,
			HTMLURL:           fmt.Sprintf("local://issues/%d", num),
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		st.Issues[num] = iss
		cp := *iss
		created = &cp
		return nil
	})
	return created, err
}

func (l *LocalTracker) ParentIssueNumber(ctx context.Context, childNumber int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if childNumber <= 0 {
		return 0, fmt.Errorf("invalid child issue number %d", childNumber)
	}
	var parent int
	err := l.withLock(false, func(st *localState) error {
		if _, ok := st.Issues[childNumber]; !ok {
			return fmt.Errorf("issue #%d not found in local tracker", childNumber)
		}
		parent = st.Parents[childNumber]
		return nil
	})
	return parent, err
}

func localChildNumbers(st *localState, parent int) []int {
	order := append([]int(nil), st.ChildOrder[parent]...)
	seen := make(map[int]bool)
	for _, n := range order {
		seen[n] = true
	}
	var missing []int
	for child, p := range st.Parents {
		if p == parent && !seen[child] {
			missing = append(missing, child)
		}
	}
	sort.Ints(missing)
	return append(order, missing...)
}

func (l *LocalTracker) ListSubIssues(ctx context.Context, parentNumber int) ([]*Issue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []*Issue
	err := l.withLock(false, func(st *localState) error {
		if st.Issues[parentNumber] == nil {
			return fmt.Errorf("parent issue #%d not found in local tracker", parentNumber)
		}
		for _, n := range localChildNumbers(st, parentNumber) {
			issue := st.Issues[n]
			if issue == nil {
				return fmt.Errorf("sub-issue #%d missing", n)
			}
			cp := *issue
			cp.Labels = append([]string(nil), issue.Labels...)
			out = append(out, &cp)
		}
		return nil
	})
	return out, err
}

func (l *LocalTracker) LinkSubIssue(ctx context.Context, parentNumber, childNumber int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if parentNumber <= 0 || childNumber <= 0 {
		return fmt.Errorf("invalid parent #%d or child #%d issue number", parentNumber, childNumber)
	}
	if parentNumber == childNumber {
		return fmt.Errorf("issue #%d cannot be linked as a sub-issue of itself", childNumber)
	}
	return l.withLock(true, func(st *localState) error {
		if _, ok := st.Issues[parentNumber]; !ok {
			return fmt.Errorf("parent issue #%d not found in local tracker", parentNumber)
		}
		if _, ok := st.Issues[childNumber]; !ok {
			return fmt.Errorf("child issue #%d not found in local tracker", childNumber)
		}
		if existing := st.Parents[childNumber]; existing != 0 {
			if existing == parentNumber {
				return nil
			}
			return fmt.Errorf("issue #%d is already linked to parent issue #%d (cannot link to #%d)", childNumber, existing, parentNumber)
		}
		for cur := st.Parents[parentNumber]; cur != 0; cur = st.Parents[cur] {
			if cur == childNumber {
				return fmt.Errorf("linking issue #%d under #%d would create a parent cycle", childNumber, parentNumber)
			}
		}
		st.ChildOrder[parentNumber] = append(localChildNumbers(st, parentNumber), childNumber)
		st.Parents[childNumber] = parentNumber
		return nil
	})
}

func (l *LocalTracker) UpdateIssueBody(_ context.Context, number int, body string) (*Issue, error) {
	var updated *Issue
	err := l.withLock(true, func(st *localState) error {
		iss, ok := st.Issues[number]
		if !ok {
			return fmt.Errorf("issue #%d not found in local tracker", number)
		}
		iss.Body = body
		iss.UpdatedAt = time.Now().UTC()
		cp := *iss
		cp.Labels = append([]string(nil), iss.Labels...)
		updated = &cp
		return nil
	})
	return updated, err
}

func (l *LocalTracker) UpdateIssueTitle(_ context.Context, number int, title string) (*Issue, error) {
	var updated *Issue
	err := l.withLock(true, func(st *localState) error {
		iss, ok := st.Issues[number]
		if !ok {
			return fmt.Errorf("issue #%d not found in local tracker", number)
		}
		iss.Title = title
		iss.UpdatedAt = time.Now().UTC()
		cp := *iss
		cp.Labels = append([]string(nil), iss.Labels...)
		updated = &cp
		return nil
	})
	return updated, err
}

func (l *LocalTracker) ListIssues(_ context.Context, filter IssueFilter) ([]*Issue, error) {
	var list []*Issue
	err := l.withLock(false, func(st *localState) error {
		for _, iss := range st.Issues {
			if filter.State != "" && filter.State != "all" && iss.State != filter.State {
				continue
			}
			matchLabels := true
			for _, reqLabel := range filter.Labels {
				if !slices.Contains(iss.Labels, reqLabel) {
					matchLabels = false
					break
				}
			}
			if !matchLabels {
				continue
			}
			cp := *iss
			cp.Labels = append([]string(nil), iss.Labels...)
			list = append(list, &cp)
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].Number < list[j].Number
		})
		return nil
	})
	return list, err
}

func (l *LocalTracker) ListComments(_ context.Context, issueNumber int) ([]*Comment, error) {
	var list []*Comment
	err := l.withLock(false, func(st *localState) error {
		if _, ok := st.Issues[issueNumber]; !ok {
			return fmt.Errorf("issue #%d not found", issueNumber)
		}
		for _, c := range st.Comments[issueNumber] {
			cp := *c
			list = append(list, &cp)
		}
		sort.Slice(list, func(i, j int) bool {
			return list[i].ID < list[j].ID
		})
		return nil
	})
	return list, err
}

func (l *LocalTracker) CreateComment(_ context.Context, issueNumber int, body string) (*Comment, error) {
	var created *Comment
	err := l.withLock(true, func(st *localState) error {
		iss, ok := st.Issues[issueNumber]
		if !ok {
			return fmt.Errorf("issue #%d not found", issueNumber)
		}
		cid := st.NextCommentID
		st.NextCommentID++
		now := time.Now().UTC()
		iss.UpdatedAt = now
		c := &Comment{
			ID:                cid,
			IssueNum:          issueNumber,
			Author:            l.user,
			AuthorAssociation: l.assoc,
			Body:              body,
			HTMLURL:           fmt.Sprintf("local://issues/%d#comment-%d", issueNumber, cid),
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		st.Comments[issueNumber] = append(st.Comments[issueNumber], c)
		cp := *c
		created = &cp
		return nil
	})
	if err == nil && l.hookAfterComment != nil {
		l.hookAfterComment(issueNumber, created)
	}
	return created, err
}

func (l *LocalTracker) UpdateComment(_ context.Context, commentID int64, body string) (*Comment, error) {
	var updated *Comment
	err := l.withLock(true, func(st *localState) error {
		for issueNum, comments := range st.Comments {
			for idx, c := range comments {
				if c.ID == commentID {
					c.Body = body
					c.UpdatedAt = time.Now().UTC()
					st.Comments[issueNum][idx] = c
					cp := *c
					updated = &cp
					return nil
				}
			}
		}
		return fmt.Errorf("comment %d not found", commentID)
	})
	return updated, err
}

func (l *LocalTracker) AddLabels(_ context.Context, issueNumber int, labels []string) error {
	return l.withLock(true, func(st *localState) error {
		iss, ok := st.Issues[issueNumber]
		if !ok {
			return fmt.Errorf("issue #%d not found", issueNumber)
		}
		iss.Labels = uniqueStrings(append(iss.Labels, labels...))
		return nil
	})
}

func (l *LocalTracker) AddAssignees(_ context.Context, issueNumber int, logins []string) error {
	return l.withLock(true, func(st *localState) error {
		iss, ok := st.Issues[issueNumber]
		if !ok {
			return fmt.Errorf("issue #%d not found", issueNumber)
		}
		iss.Assignees = uniqueStrings(append(iss.Assignees, logins...))
		return nil
	})
}

func (l *LocalTracker) RemoveLabel(_ context.Context, issueNumber int, label string) error {
	return l.withLock(true, func(st *localState) error {
		iss, ok := st.Issues[issueNumber]
		if !ok {
			return fmt.Errorf("issue #%d not found", issueNumber)
		}
		var kept []string
		for _, existing := range iss.Labels {
			if existing != label {
				kept = append(kept, existing)
			}
		}
		iss.Labels = kept
		return nil
	})
}

func (l *LocalTracker) CreateDraftPR(ctx context.Context, req DraftPRRequest) (*PullRequest, error) {
	var created *PullRequest
	err := l.withLock(true, func(st *localState) error {
		num := st.NextPRNum
		st.NextPRNum++
		pr := &PullRequest{
			Number:      num,
			IssueNumber: req.IssueNumber,
			Title:       req.Title,
			Body:        req.Body,
			Head:        req.Head,
			Base:        req.Base,
			State:       "open",
			Draft:       true,
			HTMLURL:     fmt.Sprintf("local://pulls/%d", num),
			Author:      l.user,
			CreatedAt:   time.Now().UTC(),
		}
		pr.UpdatedAt = pr.CreatedAt
		l.refreshPRRefs(pr)
		if pr.HeadSHA != "" {
			if err := l.updatePRRef(ctx, num, pr.HeadSHA, ""); err != nil {
				return fmt.Errorf("initialize local PR head: %w", err)
			}
		}
		st.PullRequests[num] = pr
		cp := *pr
		created = &cp
		return nil
	})
	return created, err
}

func (l *LocalTracker) UpdateDraftPR(_ context.Context, prNumber int, title, body string) (*PullRequest, error) {
	var updated *PullRequest
	err := l.withLock(true, func(st *localState) error {
		pr, ok := st.PullRequests[prNumber]
		if !ok {
			return fmt.Errorf("pull request #%d not found in local tracker", prNumber)
		}
		if title != "" {
			pr.Title = title
		}
		pr.Body = body
		pr.UpdatedAt = time.Now().UTC()
		cp := *pr
		updated = &cp
		return nil
	})
	return updated, err
}

func (l *LocalTracker) FindDraftPR(ctx context.Context, head, base string) (*PullRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var found *PullRequest
	err := l.withLock(false, func(st *localState) error {
		for _, pr := range st.PullRequests {
			if pr.Draft && pr.Head == head && pr.Base == base {
				cp := *pr
				l.refreshPRRefs(&cp)
				found = &cp
				return nil
			}
		}
		return nil
	})
	return found, err
}

func (l *LocalTracker) GetPullRequest(ctx context.Context, number int) (*PullRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var found *PullRequest
	err := l.withLock(false, func(st *localState) error {
		pr, ok := st.PullRequests[number]
		if !ok {
			return fmt.Errorf("pull request #%d not found in local tracker", number)
		}
		cp := *pr
		l.refreshPRRefs(&cp)
		found = &cp
		return nil
	})
	return found, err
}

func (l *LocalTracker) ListPullRequests(ctx context.Context, filter PullRequestFilter) ([]*PullRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state := filter.State
	if state == "" {
		state = "open"
	}
	var out []*PullRequest
	err := l.withLock(false, func(st *localState) error {
		for _, pr := range st.PullRequests {
			if state != "all" && pr.State != state {
				continue
			}
			cp := *pr
			l.refreshPRRefs(&cp)
			out = append(out, &cp)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].Number > out[j].Number
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, err
}

// refreshPRRefs reads only explicit branch refs. Missing refs retain persisted
// metadata for fixtures without a Git repository, never substituting HEAD.
func (l *LocalTracker) refreshPRRefs(pr *PullRequest) {
	if sha := l.prRefSHA(pr.Number); sha != "" {
		pr.HeadSHA = sha
	} else if sha := l.branchSHA(pr.Head); sha != "" {
		pr.HeadSHA = sha
	}
	if sha := l.branchSHA(pr.Base); sha != "" {
		pr.BaseSHA = sha
	}
}

// PublishPRHead advances only the local PR's dedicated Git ref with a
// compare-and-swap. The checked-out issue branch/worktree remains untouched.
func (l *LocalTracker) PublishPRHead(ctx context.Context, number int, expectedHead, head string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if number <= 0 || !validLocalSHA(expectedHead) || !validLocalSHA(head) {
		return fmt.Errorf("PR number and full expected/new commit SHAs are required")
	}
	return l.withLock(true, func(st *localState) error {
		pr, ok := st.PullRequests[number]
		if !ok {
			return fmt.Errorf("pull request #%d not found in local tracker", number)
		}
		if pr.State != "open" {
			return fmt.Errorf("pull request #%d is not open", number)
		}
		if l.prRefSHA(number) != expectedHead {
			return fmt.Errorf("local PR #%d head changed from expected %s", number, expectedHead)
		}
		cmd, _, err := gitcredential.Command(ctx, l.repoRoot, "rev-parse", "--verify", head+"^{commit}")
		if err != nil {
			return err
		}
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("PR head commit %s unavailable: %w", head, err)
		}
		if strings.TrimSpace(string(out)) != head {
			return fmt.Errorf("PR head commit %s resolved to %s", head, strings.TrimSpace(string(out)))
		}
		if err := l.updatePRRef(ctx, number, head, expectedHead); err != nil {
			return fmt.Errorf("publish local PR head: %w", err)
		}
		pr.HeadSHA = head
		return nil
	})
}

func validLocalSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}

func (l *LocalTracker) updatePRRef(ctx context.Context, number int, head, old string) error {
	cmd, redact, err := gitcredential.Command(ctx, l.repoRoot, "update-ref", fmt.Sprintf("refs/swarm/pr-head/%d", number), head, old)
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, redact(strings.TrimSpace(string(out))))
	}
	return nil
}

func (l *LocalTracker) prRefSHA(number int) string {
	if number <= 0 {
		return ""
	}
	return l.refSHA(fmt.Sprintf("refs/swarm/pr-head/%d", number))
}

func (l *LocalTracker) branchSHA(branch string) string {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.Contains(branch, ":") {
		return ""
	}
	ref := "refs/heads/" + branch
	check, _, err := gitcredential.Command(context.Background(), l.repoRoot, "check-ref-format", ref)
	if err != nil || check.Run() != nil {
		return ""
	}
	return l.refSHA(ref)
}

func (l *LocalTracker) refSHA(ref string) string {
	cmd, _, err := gitcredential.Command(context.Background(), l.repoRoot, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return ""
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (l *LocalTracker) GetPullRequestReviews(ctx context.Context, number int) ([]PullRequestReview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("invalid pull request number %d", number)
	}
	return nil, nil
}

func (l *LocalTracker) GetCheckStatus(_ context.Context, _ string) ([]CheckRunResult, error) {
	return nil, nil
}

func (l *LocalTracker) EnsureLabels(_ context.Context, labels []LabelSpec) error {
	return l.withLock(true, func(st *localState) error {
		for _, lb := range labels {
			st.DefinedLabels[lb.Name] = lb
		}
		return nil
	})
}

func uniqueStrings(in []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
