package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
)

type GitHubTracker struct {
	owner  string
	repo   string
	client *api.RESTClient
}

func NewGitHubTracker(repoOverride string) (*GitHubTracker, error) {
	var owner, name string
	if repoOverride == "" {
		repoOverride = os.Getenv("SWARM_REPO")
	}
	if repoOverride != "" {
		parts := strings.Split(repoOverride, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid repo format %q: expected owner/repo", repoOverride)
		}
		owner, name = parts[0], parts[1]
	} else {
		r, err := repository.Current()
		if err != nil {
			return nil, fmt.Errorf("could not resolve current GitHub repository (use --backend local or --repo owner/repo): %w", err)
		}
		owner, name = r.Owner, r.Name
	}

	client, err := api.DefaultRESTClient()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize GitHub REST client: %w", err)
	}
	return &GitHubTracker{
		owner:  owner,
		repo:   name,
		client: client,
	}, nil
}

func (g *GitHubTracker) BackendName() string {
	return fmt.Sprintf("github (%s/%s)", g.owner, g.repo)
}

func (g *GitHubTracker) CurrentUser(_ context.Context) (string, error) {
	if u := os.Getenv("SWARM_USER"); u != "" {
		return u, nil
	}
	var resp struct {
		Login string `json:"login"`
	}
	if err := g.client.Get("user", &resp); err != nil {
		return "", fmt.Errorf("failed to fetch current GitHub user: %w", err)
	}
	return resp.Login, nil
}

type ghIssueResponse struct {
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	Body              string    `json:"body"`
	State             string    `json:"state"`
	HTMLURL           string    `json:"html_url"`
	AuthorAssociation string    `json:"author_association"`
	CreatedAt         time.Time `json:"created_at"`
	User              struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"`
}

func convertGHIssue(r ghIssueResponse) *Issue {
	var labels []string
	for _, l := range r.Labels {
		labels = append(labels, l.Name)
	}
	return &Issue{
		Number:            r.Number,
		Title:             r.Title,
		Body:              r.Body,
		State:             r.State,
		Labels:            labels,
		Author:            r.User.Login,
		AuthorAssociation: r.AuthorAssociation,
		HTMLURL:           r.HTMLURL,
		CreatedAt:         r.CreatedAt,
	}
}

func (g *GitHubTracker) GetIssue(_ context.Context, number int) (*Issue, error) {
	var resp ghIssueResponse
	path := fmt.Sprintf("repos/%s/%s/issues/%d", g.owner, g.repo, number)
	if err := g.client.Get(path, &resp); err != nil {
		return nil, fmt.Errorf("github GetIssue #%d: %w", number, err)
	}
	return convertGHIssue(resp), nil
}

func (g *GitHubTracker) CreateIssue(_ context.Context, title, body string, labels []string) (*Issue, error) {
	payload := map[string]any{
		"title":  title,
		"body":   body,
		"labels": labels,
	}
	raw, _ := json.Marshal(payload)
	var resp ghIssueResponse
	path := fmt.Sprintf("repos/%s/%s/issues", g.owner, g.repo)
	if err := g.client.Post(path, bytes.NewReader(raw), &resp); err != nil {
		return nil, fmt.Errorf("github CreateIssue: %w", err)
	}
	return convertGHIssue(resp), nil
}

func (g *GitHubTracker) ListIssues(_ context.Context, filter IssueFilter) ([]*Issue, error) {
	q := url.Values{}
	state := filter.State
	if state == "" {
		state = "open"
	}
	q.Set("state", state)
	q.Set("per_page", "100")
	if len(filter.Labels) > 0 {
		q.Set("labels", strings.Join(filter.Labels, ","))
	}
	path := fmt.Sprintf("repos/%s/%s/issues?%s", g.owner, g.repo, q.Encode())
	var rawList []ghIssueResponse
	if err := g.client.Get(path, &rawList); err != nil {
		return nil, fmt.Errorf("github ListIssues: %w", err)
	}
	var out []*Issue
	for _, r := range rawList {
		if r.PullRequest != nil {
			continue
		}
		out = append(out, convertGHIssue(r))
	}
	return out, nil
}

type ghCommentResponse struct {
	ID                int64     `json:"id"`
	Body              string    `json:"body"`
	HTMLURL           string    `json:"html_url"`
	AuthorAssociation string    `json:"author_association"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	User              struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (g *GitHubTracker) ListComments(_ context.Context, issueNumber int) ([]*Comment, error) {
	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100", g.owner, g.repo, issueNumber)
	var rawList []ghCommentResponse
	if err := g.client.Get(path, &rawList); err != nil {
		return nil, fmt.Errorf("github ListComments #%d: %w", issueNumber, err)
	}
	var out []*Comment
	for _, r := range rawList {
		out = append(out, &Comment{
			ID:                r.ID,
			IssueNum:          issueNumber,
			Author:            r.User.Login,
			AuthorAssociation: r.AuthorAssociation,
			Body:              r.Body,
			HTMLURL:           r.HTMLURL,
			CreatedAt:         r.CreatedAt,
			UpdatedAt:         r.UpdatedAt,
		})
	}
	return out, nil
}

func (g *GitHubTracker) CreateComment(_ context.Context, issueNumber int, body string) (*Comment, error) {
	raw, _ := json.Marshal(map[string]string{"body": body})
	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments", g.owner, g.repo, issueNumber)
	var r ghCommentResponse
	if err := g.client.Post(path, bytes.NewReader(raw), &r); err != nil {
		return nil, fmt.Errorf("github CreateComment #%d: %w", issueNumber, err)
	}
	return &Comment{
		ID:                r.ID,
		IssueNum:          issueNumber,
		Author:            r.User.Login,
		AuthorAssociation: r.AuthorAssociation,
		Body:              r.Body,
		HTMLURL:           r.HTMLURL,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}, nil
}

func (g *GitHubTracker) UpdateComment(_ context.Context, commentID int64, body string) (*Comment, error) {
	raw, _ := json.Marshal(map[string]string{"body": body})
	path := fmt.Sprintf("repos/%s/%s/issues/comments/%d", g.owner, g.repo, commentID)
	var r ghCommentResponse
	if err := g.client.Patch(path, bytes.NewReader(raw), &r); err != nil {
		return nil, fmt.Errorf("github UpdateComment %d: %w", commentID, err)
	}
	return &Comment{
		ID:        r.ID,
		Author:    r.User.Login,
		Body:      r.Body,
		HTMLURL:   r.HTMLURL,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}, nil
}

func (g *GitHubTracker) AddLabels(_ context.Context, issueNumber int, labels []string) error {
	raw, _ := json.Marshal(map[string]any{"labels": labels})
	path := fmt.Sprintf("repos/%s/%s/issues/%d/labels", g.owner, g.repo, issueNumber)
	var resp any
	return g.client.Post(path, bytes.NewReader(raw), &resp)
}

func (g *GitHubTracker) RemoveLabel(_ context.Context, issueNumber int, label string) error {
	path := fmt.Sprintf("repos/%s/%s/issues/%d/labels/%s", g.owner, g.repo, issueNumber, url.PathEscape(label))
	return g.client.Delete(path, nil)
}

func (g *GitHubTracker) CreateDraftPR(_ context.Context, req DraftPRRequest) (*PullRequest, error) {
	base := req.Base
	if base == "" {
		base = "main"
	}
	payload := map[string]any{
		"title": req.Title,
		"body":  req.Body,
		"head":  req.Head,
		"base":  base,
		"draft": true,
	}
	raw, _ := json.Marshal(payload)
	path := fmt.Sprintf("repos/%s/%s/pulls", g.owner, g.repo)
	var resp struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		Draft   bool   `json:"draft"`
		HTMLURL string `json:"html_url"`
	}
	if err := g.client.Post(path, bytes.NewReader(raw), &resp); err != nil {
		return nil, fmt.Errorf("github CreateDraftPR: %w", err)
	}
	return &PullRequest{
		Number:  resp.Number,
		Title:   resp.Title,
		Body:    resp.Body,
		Head:    req.Head,
		Base:    base,
		Draft:   resp.Draft,
		HTMLURL: resp.HTMLURL,
	}, nil
}

func (g *GitHubTracker) GetCheckStatus(_ context.Context, ref string) ([]CheckRunResult, error) {
	path := fmt.Sprintf("repos/%s/%s/commits/%s/check-runs", g.owner, g.repo, url.PathEscape(ref))
	var resp struct {
		CheckRuns []CheckRunResult `json:"check_runs"`
	}
	if err := g.client.Get(path, &resp); err != nil {
		return nil, err
	}
	return resp.CheckRuns, nil
}

func (g *GitHubTracker) EnsureLabels(_ context.Context, labels []LabelSpec) error {
	for _, lb := range labels {
		raw, _ := json.Marshal(map[string]string{
			"name":        lb.Name,
			"color":       lb.Color,
			"description": lb.Description,
		})
		path := fmt.Sprintf("repos/%s/%s/labels", g.owner, g.repo)
		var resp any
		if err := g.client.Post(path, bytes.NewReader(raw), &resp); err != nil {
			// Ignore 422 already_exists
			if !strings.Contains(err.Error(), "422") && !strings.Contains(err.Error(), "already_exists") {
				return fmt.Errorf("ensure label %q: %w", lb.Name, err)
			}
		}
	}
	return nil
}
