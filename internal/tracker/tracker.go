package tracker

import (
	"context"
	"strings"
	"time"
)

type Issue struct {
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	Body              string    `json:"body"`
	State             string    `json:"state"` // "open", "closed"
	Type              string    `json:"type,omitempty"`
	Labels            []string  `json:"labels"`
	Assignees         []string  `json:"assignees,omitempty" yaml:"assignees,omitempty"`
	Author            string    `json:"author"`
	AuthorAssociation string    `json:"author_association"` // "OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR", "NONE"
	HTMLURL           string    `json:"html_url"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"` // last activity; zero when the backend does not record it
}

func IsEpicIssueType(issueType string) bool {
	return strings.EqualFold(strings.TrimSpace(issueType), "Epic")
}

func (i *Issue) IsEpic() bool {
	return i != nil && IsEpicIssueType(i.Type)
}

type Comment struct {
	ID                int64     `json:"id"`
	IssueNum          int       `json:"issue_num"`
	Author            string    `json:"author"`
	AuthorAssociation string    `json:"author_association,omitempty"`
	AuthorType        string    `json:"author_type,omitempty"`
	Body              string    `json:"body"`
	HTMLURL           string    `json:"html_url"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type IssueFilter struct {
	State  string
	Labels []string
}

// PullRequestFilter selects pull requests for listing. Results come most
// recently updated first; Limit 0 returns every match.
type PullRequestFilter struct {
	State string // "open" (default), "closed", or "all"
	Limit int
}

type DraftPRRequest struct {
	IssueNumber int    `json:"issue_number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Head        string `json:"head"`
	Base        string `json:"base"`
}

type PullRequest struct {
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	Head           string     `json:"head"`
	HeadRepo       string     `json:"head_repo,omitempty"`
	HeadSHA        string     `json:"head_sha,omitempty"`
	BaseSHA        string     `json:"base_sha,omitempty"`
	State          string     `json:"state"`
	Merged         bool       `json:"merged"`
	MergeCommitSHA string     `json:"merge_commit_sha,omitempty"`
	MergedAt       *time.Time `json:"merged_at,omitempty"`
	// IssueNumber is the tracker's own record of the linked issue (local backend).
	// GitHub has no such field; PR commands treat the body's closing reference
	// as a lookup hint and always verify the link against coordination state.
	IssueNumber int    `json:"issue_number,omitempty"`
	Base        string `json:"base"`
	BaseRepo    string `json:"base_repo,omitempty"`
	Draft       bool   `json:"draft"`
	HTMLURL     string `json:"html_url"`
	// Author and timestamps are filled by ListPullRequests.
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

type PullRequestReview struct {
	Author      string    `json:"author"`
	State       string    `json:"state"`
	CommitSHA   string    `json:"commit_sha"`
	SubmittedAt time.Time `json:"submitted_at"`
	// Current reports whether the review was submitted against the PR's
	// published head; reviews of earlier commits are stale.
	Current bool `json:"current"`
}

type CheckRunResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	// App is the GitHub App that reported the check, when GitHub records one.
	App string `json:"app,omitempty"`
}

type LabelSpec struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func DefaultSwarmLabels() []LabelSpec {
	return []LabelSpec{
		{Name: "swarm:ready", Color: "0e8a16", Description: "Work unit vetted and ready for SwarmOS agents"},
		{Name: "swarm:draft", Color: "cfd3d7", Description: "Structured WorkUnit draft awaiting maintainer approval"},
		{Name: "swarm:community", Color: "c5def5", Description: "Community bug report, idea, or feedback awaiting maintainer triage"},
		{Name: "swarm:needs-scope", Color: "e99695", Description: "Imported or raw issue needing a structured WorkUnit contract"},
		{Name: "swarm:blocked", Color: "b60205", Description: "Waiting on prerequisite depends_on issues, overlapping touchpoints, or policy review"},
		{Name: "swarm:claimed", Color: "1d76db", Description: "Claimed by an active SwarmOS lease"},
		{Name: "swarm:in-progress", Color: "fbca04", Description: "SwarmOS workflow execution in progress"},
		{Name: "swarm:plan-review", Color: "d93f0b", Description: "Plan artifact awaiting review approval"},
		{Name: "swarm:plan-approved", Color: "0e8a16", Description: "Plan artifact approved for implementation"},
		{Name: "swarm:pr-open", Color: "5319e7", Description: "Draft PR submitted for this work unit"},
	}
}

type Tracker interface {
	BackendName() string
	CurrentUser(ctx context.Context) (string, error)
	// Authorize checks the current authenticated actor's repository permission.
	Authorize(ctx context.Context, permission string) error
	CheckIssueType(ctx context.Context, issueType string) error
	GetIssue(ctx context.Context, number int) (*Issue, error)
	CreateIssue(ctx context.Context, title, body string, labels []string) (*Issue, error)
	CreateTypedIssue(ctx context.Context, title, body string, labels []string, issueType string) (*Issue, error)
	UpdateIssueBody(ctx context.Context, number int, body string) (*Issue, error)
	ParentIssueNumber(ctx context.Context, childNumber int) (int, error)
	LinkSubIssue(ctx context.Context, parentNumber, childNumber int) error
	ListIssues(ctx context.Context, filter IssueFilter) ([]*Issue, error)
	ListComments(ctx context.Context, issueNumber int) ([]*Comment, error)
	CreateComment(ctx context.Context, issueNumber int, body string) (*Comment, error)
	UpdateComment(ctx context.Context, commentID int64, body string) (*Comment, error)
	AddLabels(ctx context.Context, issueNumber int, labels []string) error
	RemoveLabel(ctx context.Context, issueNumber int, label string) error
	AddAssignees(ctx context.Context, issueNumber int, logins []string) error
	CreateDraftPR(ctx context.Context, req DraftPRRequest) (*PullRequest, error)
	UpdateDraftPR(ctx context.Context, prNumber int, title, body string) (*PullRequest, error)
	FindDraftPR(ctx context.Context, head, base string) (*PullRequest, error)
	GetPullRequest(ctx context.Context, number int) (*PullRequest, error)
	ListPullRequests(ctx context.Context, filter PullRequestFilter) ([]*PullRequest, error)
	GetCheckStatus(ctx context.Context, ref string) ([]CheckRunResult, error)
	EnsureLabels(ctx context.Context, labels []LabelSpec) error
}
