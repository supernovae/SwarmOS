package tracker

import (
	"context"
	"time"
)

type Issue struct {
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	Body              string    `json:"body"`
	State             string    `json:"state"` // "open", "closed"
	Labels            []string  `json:"labels"`
	Author            string    `json:"author"`
	AuthorAssociation string    `json:"author_association"` // "OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR", "NONE"
	HTMLURL           string    `json:"html_url"`
	CreatedAt         time.Time `json:"created_at"`
}

type Comment struct {
	ID                int64     `json:"id"`
	IssueNum          int       `json:"issue_num"`
	Author            string    `json:"author"`
	AuthorAssociation string    `json:"author_association,omitempty"`
	Body              string    `json:"body"`
	HTMLURL           string    `json:"html_url"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type IssueFilter struct {
	State  string
	Labels []string
}

type DraftPRRequest struct {
	IssueNumber int    `json:"issue_number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Head        string `json:"head"`
	Base        string `json:"base"`
}

type PullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Head    string `json:"head"`
	Base    string `json:"base"`
	Draft   bool   `json:"draft"`
	HTMLURL string `json:"html_url"`
}

type CheckRunResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type LabelSpec struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func DefaultSwarmLabels() []LabelSpec {
	return []LabelSpec{
		{Name: "swarm:ready", Color: "0e8a16", Description: "Work unit vetted and ready for SwarmOS agents"},
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
	GetIssue(ctx context.Context, number int) (*Issue, error)
	CreateIssue(ctx context.Context, title, body string, labels []string) (*Issue, error)
	ListIssues(ctx context.Context, filter IssueFilter) ([]*Issue, error)
	ListComments(ctx context.Context, issueNumber int) ([]*Comment, error)
	CreateComment(ctx context.Context, issueNumber int, body string) (*Comment, error)
	UpdateComment(ctx context.Context, commentID int64, body string) (*Comment, error)
	AddLabels(ctx context.Context, issueNumber int, labels []string) error
	RemoveLabel(ctx context.Context, issueNumber int, label string) error
	CreateDraftPR(ctx context.Context, req DraftPRRequest) (*PullRequest, error)
	GetCheckStatus(ctx context.Context, ref string) ([]CheckRunResult, error)
	EnsureLabels(ctx context.Context, labels []LabelSpec) error
}
