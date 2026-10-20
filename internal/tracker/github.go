package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// ResolveGitHubToken returns the configured GitHub API token in precedence order:
// SWARM_GITHUB_TOKEN -> GH_TOKEN -> GITHUB_TOKEN.
func ResolveGitHubToken() (string, string) {
	for _, key := range []string{"SWARM_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			return val, key
		}
	}
	return "", ""
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

	var client *api.RESTClient
	var err error
	if token, source := ResolveGitHubToken(); token != "" && source == "SWARM_GITHUB_TOKEN" {
		host := strings.TrimSpace(os.Getenv("GH_HOST"))
		if host == "" {
			host = "github.com"
		}
		client, err = api.NewRESTClient(api.ClientOptions{
			Host:      host,
			AuthToken: token,
		})
	} else {
		client, err = api.DefaultRESTClient()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to initialize GitHub REST client (set SWARM_GITHUB_TOKEN, GH_TOKEN, or GITHUB_TOKEN, or run gh auth login): %w", err)
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

func (g *GitHubTracker) CurrentUser(ctx context.Context) (string, error) {
	var resp struct {
		Login string `json:"login"`
	}
	if err := g.client.DoWithContext(ctx, "GET", "user", nil, &resp); err != nil {
		return "", fmt.Errorf("failed to fetch current GitHub user: %w", err)
	}
	return resp.Login, nil
}

func (g *GitHubTracker) Authorize(ctx context.Context, permission string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if permission != "approve-task" && permission != "review-human" {
		return fmt.Errorf("unknown permission %q", permission)
	}
	var me struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if err := g.client.DoWithContext(ctx, "GET", "user", nil, &me); err != nil {
		return fmt.Errorf("github authorization identity: %w", err)
	}
	if permission == "review-human" && strings.EqualFold(me.Type, "bot") {
		return fmt.Errorf("bot account %q cannot provide human review", me.Login)
	}
	var collaborator struct {
		Permission string `json:"permission"`
	}
	path := fmt.Sprintf("repos/%s/%s/collaborators/%s/permission", g.owner, g.repo, url.PathEscape(me.Login))
	if err := g.client.DoWithContext(ctx, "GET", path, nil, &collaborator); err != nil {
		return fmt.Errorf("github authorization permission: %w", err)
	}
	switch strings.ToLower(collaborator.Permission) {
	case "admin", "maintain", "write":
		return nil
	default:
		return fmt.Errorf("github actor %q lacks write permission for %s", me.Login, permission)
	}
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

func (g *GitHubTracker) GetIssue(ctx context.Context, number int) (*Issue, error) {
	var resp ghIssueResponse
	path := fmt.Sprintf("repos/%s/%s/issues/%d", g.owner, g.repo, number)
	if err := g.client.DoWithContext(ctx, "GET", path, nil, &resp); err != nil {
		return nil, fmt.Errorf("github GetIssue #%d: %w", number, err)
	}
	return convertGHIssue(resp), nil
}

func (g *GitHubTracker) CreateIssue(ctx context.Context, title, body string, labels []string) (*Issue, error) {
	payload := map[string]any{
		"title":  title,
		"body":   body,
		"labels": labels,
	}
	raw, _ := json.Marshal(payload)
	var resp ghIssueResponse
	path := fmt.Sprintf("repos/%s/%s/issues", g.owner, g.repo)
	if err := g.client.DoWithContext(ctx, "POST", path, bytes.NewReader(raw), &resp); err != nil {
		return nil, fmt.Errorf("github CreateIssue: %w", err)
	}
	return convertGHIssue(resp), nil
}

func (g *GitHubTracker) UpdateIssueBody(ctx context.Context, number int, body string) (*Issue, error) {
	raw, _ := json.Marshal(map[string]any{"body": body})
	var resp ghIssueResponse
	path := fmt.Sprintf("repos/%s/%s/issues/%d", g.owner, g.repo, number)
	if err := g.client.DoWithContext(ctx, "PATCH", path, bytes.NewReader(raw), &resp); err != nil {
		return nil, fmt.Errorf("github UpdateIssueBody #%d: %w", number, err)
	}
	return convertGHIssue(resp), nil
}

func (g *GitHubTracker) ListIssues(ctx context.Context, filter IssueFilter) ([]*Issue, error) {
	var out []*Issue
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		q := url.Values{}
		state := filter.State
		if state == "" {
			state = "open"
		}
		q.Set("state", state)
		q.Set("per_page", "100")
		q.Set("page", fmt.Sprint(page))
		if len(filter.Labels) > 0 {
			q.Set("labels", strings.Join(filter.Labels, ","))
		}
		path := fmt.Sprintf("repos/%s/%s/issues?%s", g.owner, g.repo, q.Encode())
		var rawList []ghIssueResponse
		if err := g.client.DoWithContext(ctx, "GET", path, nil, &rawList); err != nil {
			return nil, fmt.Errorf("github ListIssues page %d: %w", page, err)
		}
		for _, r := range rawList {
			if r.PullRequest == nil {
				out = append(out, convertGHIssue(r))
			}
		}
		if len(rawList) < 100 {
			break
		}
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

func (g *GitHubTracker) ListComments(ctx context.Context, issueNumber int) ([]*Comment, error) {
	var out []*Comment
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100&page=%d", g.owner, g.repo, issueNumber, page)
		var rawList []ghCommentResponse
		if err := g.client.DoWithContext(ctx, "GET", path, nil, &rawList); err != nil {
			return nil, fmt.Errorf("github ListComments #%d page %d: %w", issueNumber, page, err)
		}
		for _, r := range rawList {
			out = append(out, &Comment{ID: r.ID, IssueNum: issueNumber, Author: r.User.Login, AuthorAssociation: r.AuthorAssociation, Body: r.Body, HTMLURL: r.HTMLURL, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
		}
		if len(rawList) < 100 {
			break
		}
	}
	return out, nil
}

func (g *GitHubTracker) CreateComment(ctx context.Context, issueNumber int, body string) (*Comment, error) {
	raw, _ := json.Marshal(map[string]string{"body": body})
	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments", g.owner, g.repo, issueNumber)
	var r ghCommentResponse
	if err := g.client.DoWithContext(ctx, "POST", path, bytes.NewReader(raw), &r); err != nil {
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

func (g *GitHubTracker) UpdateComment(ctx context.Context, commentID int64, body string) (*Comment, error) {
	raw, _ := json.Marshal(map[string]string{"body": body})
	path := fmt.Sprintf("repos/%s/%s/issues/comments/%d", g.owner, g.repo, commentID)
	var r ghCommentResponse
	if err := g.client.DoWithContext(ctx, "PATCH", path, bytes.NewReader(raw), &r); err != nil {
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

func (g *GitHubTracker) AddLabels(ctx context.Context, issueNumber int, labels []string) error {
	raw, _ := json.Marshal(map[string]any{"labels": labels})
	path := fmt.Sprintf("repos/%s/%s/issues/%d/labels", g.owner, g.repo, issueNumber)
	var resp any
	return g.client.DoWithContext(ctx, "POST", path, bytes.NewReader(raw), &resp)
}

func (g *GitHubTracker) RemoveLabel(ctx context.Context, issueNumber int, label string) error {
	path := fmt.Sprintf("repos/%s/%s/issues/%d/labels/%s", g.owner, g.repo, issueNumber, url.PathEscape(label))
	err := g.client.DoWithContext(ctx, "DELETE", path, nil, nil)
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == 404 {
		return nil // The requested projection is already absent.
	}
	return err
}

func (g *GitHubTracker) CreateDraftPR(ctx context.Context, req DraftPRRequest) (*PullRequest, error) {
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
	if err := g.client.DoWithContext(ctx, "POST", path, bytes.NewReader(raw), &resp); err != nil {
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

func (g *GitHubTracker) UpdateDraftPR(ctx context.Context, prNumber int, title, body string) (*PullRequest, error) {
	payload := map[string]any{
		"body": body,
	}
	if strings.TrimSpace(title) != "" {
		payload["title"] = title
	}
	raw, _ := json.Marshal(payload)
	path := fmt.Sprintf("repos/%s/%s/pulls/%d", g.owner, g.repo, prNumber)
	var resp struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		Draft   bool   `json:"draft"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := g.client.DoWithContext(ctx, "PATCH", path, bytes.NewReader(raw), &resp); err != nil {
		return nil, fmt.Errorf("github UpdateDraftPR #%d: %w", prNumber, err)
	}
	return &PullRequest{
		Number:  resp.Number,
		Title:   resp.Title,
		Body:    resp.Body,
		Head:    resp.Head.Ref,
		Base:    resp.Base.Ref,
		Draft:   resp.Draft,
		HTMLURL: resp.HTMLURL,
	}, nil
}

func (g *GitHubTracker) FindDraftPR(ctx context.Context, head, base string) (*PullRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q := url.Values{"state": []string{"open"}, "head": []string{g.owner + ":" + head}, "base": []string{base}, "per_page": []string{"100"}}
	path := fmt.Sprintf("repos/%s/%s/pulls?%s", g.owner, g.repo, q.Encode())
	var prs []struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		Draft   bool   `json:"draft"`
		HTMLURL string `json:"html_url"`
	}
	if err := g.client.DoWithContext(ctx, "GET", path, nil, &prs); err != nil {
		return nil, fmt.Errorf("github FindDraftPR: %w", err)
	}
	for _, pr := range prs {
		if pr.Draft {
			return &PullRequest{Number: pr.Number, Title: pr.Title, Body: pr.Body, Head: head, Base: base, Draft: true, HTMLURL: pr.HTMLURL}, nil
		}
	}
	return nil, nil
}

func (g *GitHubTracker) GetCheckStatus(ctx context.Context, ref string) ([]CheckRunResult, error) {
	path := fmt.Sprintf("repos/%s/%s/commits/%s/check-runs", g.owner, g.repo, url.PathEscape(ref))
	var resp struct {
		CheckRuns []CheckRunResult `json:"check_runs"`
	}
	if err := g.client.DoWithContext(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.CheckRuns, nil
}

func (g *GitHubTracker) EnsureLabels(ctx context.Context, labels []LabelSpec) error {
	for _, lb := range labels {
		raw, _ := json.Marshal(map[string]string{
			"name":        lb.Name,
			"color":       lb.Color,
			"description": lb.Description,
		})
		path := fmt.Sprintf("repos/%s/%s/labels", g.owner, g.repo)
		var resp any
		if err := g.client.DoWithContext(ctx, "POST", path, bytes.NewReader(raw), &resp); err != nil {
			// Ignore 422 already_exists
			if !strings.Contains(err.Error(), "422") && !strings.Contains(err.Error(), "already_exists") {
				return fmt.Errorf("ensure label %q: %w", lb.Name, err)
			}
		}
	}
	return nil
}
