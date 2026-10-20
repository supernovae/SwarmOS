package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
	"time"
)

type localState struct {
	NextIssueNum   int                  `json:"next_issue_num"`
	NextCommentID  int64                `json:"next_comment_id"`
	NextPRNum      int                  `json:"next_pr_num"`
	Issues         map[int]*Issue       `json:"issues"`
	Comments       map[int][]*Comment   `json:"comments"`
	PullRequests   map[int]*PullRequest `json:"pull_requests"`
	DefinedLabels  map[string]LabelSpec `json:"defined_labels"`
}

type LocalTracker struct {
	statePath string
	user      string
	assoc     string
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

func (l *LocalTracker) withLock(mutate bool, fn func(st *localState) error) error {
	dir := filepath.Dir(l.statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lockPath := l.statePath + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock failed: %w", err)
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	st := &localState{
		NextIssueNum:  1,
		NextCommentID: 1000,
		NextPRNum:     1,
		Issues:        make(map[int]*Issue),
		Comments:      make(map[int][]*Comment),
		PullRequests:  make(map[int]*PullRequest),
		DefinedLabels: make(map[string]LabelSpec),
	}

	if data, err := os.ReadFile(l.statePath); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, st); err != nil {
			return fmt.Errorf("corrupt local tracker state: %w", err)
		}
		if st.Issues == nil {
			st.Issues = make(map[int]*Issue)
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
		return os.WriteFile(l.statePath, out, 0o644)
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
			ID:        cid,
			IssueNum:  issueNumber,
			Author:    l.user,
			Body:      body,
			HTMLURL:   fmt.Sprintf("local://issues/%d#comment-%d", issueNumber, cid),
			CreatedAt: now,
			UpdatedAt: now,
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
