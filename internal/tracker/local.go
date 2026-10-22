package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/safefs"
)

type localState struct {
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
	case "approve-task", "review-human":
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
		if _, ok := st.Issues[issueNumber]; !ok {
			return fmt.Errorf("issue #%d not found", issueNumber)
		}
		cid := st.NextCommentID
		st.NextCommentID++
		now := time.Now().UTC()
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

func (l *LocalTracker) CreateDraftPR(_ context.Context, req DraftPRRequest) (*PullRequest, error) {
	var created *PullRequest
	err := l.withLock(true, func(st *localState) error {
		num := st.NextPRNum
		st.NextPRNum++
		pr := &PullRequest{
			Number:  num,
			Title:   req.Title,
			Body:    req.Body,
			Head:    req.Head,
			Base:    req.Base,
			Draft:   true,
			HTMLURL: fmt.Sprintf("local://pulls/%d", num),
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
		found = &cp
		return nil
	})
	return found, err
}

func (l *LocalTracker) GetCheckStatus(_ context.Context, _ string) ([]CheckRunResult, error) {
	return []CheckRunResult{
		{Name: "local-validate", Status: "completed", Conclusion: "success"},
	}, nil
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
