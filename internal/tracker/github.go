package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/supernovae/swarmos/internal/gitcredential"
)

type GitHubTracker struct {
	owner  string
	repo   string
	client *api.RESTClient
}

// ResolveGitHubToken returns the configured GitHub API token in precedence order:
// SWARM_GITHUB_TOKEN -> SWARM_GITHUB_TOKEN_COMMAND -> GH_TOKEN -> GITHUB_TOKEN.
func ResolveGitHubToken() (string, string, error) {
	return gitcredential.GitHubToken()
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

	token, source, err := ResolveGitHubToken()
	if err != nil {
		return nil, err
	}
	var client *api.RESTClient
	if token != "" && (source == "SWARM_GITHUB_TOKEN" || source == gitcredential.TokenCommandEnv) {
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
		return nil, fmt.Errorf("failed to initialize GitHub REST client (set SWARM_GITHUB_TOKEN_COMMAND, SWARM_GITHUB_TOKEN, or GH_TOKEN, or run gh auth login): %w", err)
	}
	return &GitHubTracker{
		owner:  owner,
		repo:   name,
		client: client,
	}, nil
}

func NewGitHubTrackerWithClient(owner, repo string, client *api.RESTClient) *GitHubTracker {
	return &GitHubTracker{
		owner:  owner,
		repo:   repo,
		client: client,
	}
}

func (g *GitHubTracker) UpstreamRepo() string {
	return g.owner + "/" + g.repo
}

func (g *GitHubTracker) RESTClient() *api.RESTClient {
	return g.client
}

func (g *GitHubTracker) VerifyForkRepository(ctx context.Context, forkSlug, expectedOwner string, requirePush bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	forkSlug = strings.TrimSpace(forkSlug)
	parts := strings.Split(forkSlug, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", fmt.Errorf("invalid fork repository %q: expected owner/repo", forkSlug)
	}
	if expectedOwner != "" && !strings.EqualFold(parts[0], expectedOwner) {
		return "", fmt.Errorf("fork repository %q owner %q does not match authenticated user %q", forkSlug, parts[0], expectedOwner)
	}
	path := fmt.Sprintf("repos/%s/%s", url.PathEscape(parts[0]), url.PathEscape(parts[1]))
	var resp struct {
		FullName string `json:"full_name"`
		Name     string `json:"name"`
		Fork     bool   `json:"fork"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		Source struct {
			FullName string `json:"full_name"`
		} `json:"source"`
		Permissions *struct {
			Push  bool `json:"push"`
			Admin bool `json:"admin"`
		} `json:"permissions,omitempty"`
	}
	if err := g.client.DoWithContext(ctx, "GET", path, nil, &resp); err != nil {
		return "", fmt.Errorf("verify fork repository %q: %w", forkSlug, err)
	}
	if !resp.Fork {
		return "", fmt.Errorf("repository %q is not a GitHub fork", forkSlug)
	}
	upstream := g.UpstreamRepo()
	if !strings.EqualFold(resp.Source.FullName, upstream) {
		return "", fmt.Errorf("fork repository %q source %q does not match upstream %q", forkSlug, resp.Source.FullName, upstream)
	}
	if expectedOwner != "" && !strings.EqualFold(resp.Owner.Login, expectedOwner) {
		return "", fmt.Errorf("fork repository %q owner %q does not match %q", forkSlug, resp.Owner.Login, expectedOwner)
	}
	if requirePush {
		if resp.Permissions == nil || (!resp.Permissions.Push && !resp.Permissions.Admin) {
			return "", fmt.Errorf("authenticated user lacks push permission on fork repository %q", forkSlug)
		}
	}
	canonical := strings.TrimSpace(resp.FullName)
	if canonical == "" && resp.Owner.Login != "" && resp.Name != "" {
		canonical = resp.Owner.Login + "/" + resp.Name
	}
	if canonical == "" {
		canonical = forkSlug
	}
	return canonical, nil
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
	if permission != "approve-task" && permission != "review-human" && permission != "create-epic" {
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
	perm := strings.ToLower(collaborator.Permission)
	if permission == "create-epic" {
		switch perm {
		case "admin", "maintain":
			return nil
		default:
			return fmt.Errorf("github actor %q lacks maintainer/admin permission for %s", me.Login, permission)
		}
	}
	switch perm {
	case "admin", "maintain", "write":
		return nil
	default:
		return fmt.Errorf("github actor %q lacks write permission for %s", me.Login, permission)
	}
}

type ghIssueTypeResponse struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	IsEnabled *bool  `json:"is_enabled,omitempty"`
}

func (g *GitHubTracker) CheckIssueType(ctx context.Context, issueType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !IsEpicIssueType(issueType) {
		return fmt.Errorf("unsupported issue type %q (only \"Epic\" is supported)", issueType)
	}
	path := fmt.Sprintf("repos/%s/%s/issue-types", g.owner, g.repo)
	var types []ghIssueTypeResponse
	if err := g.client.DoWithContext(ctx, "GET", path, nil, &types); err != nil {
		return fmt.Errorf("github issue types unavailable on %s/%s (enable organization issue types including \"Epic\" and verify token permissions): %w", g.owner, g.repo, err)
	}
	for _, t := range types {
		if IsEpicIssueType(t.Name) && (t.IsEnabled == nil || *t.IsEnabled) {
			return nil
		}
	}
	return fmt.Errorf("github issue type \"Epic\" is not enabled on %s/%s; enable the organization \"Epic\" issue type in GitHub settings", g.owner, g.repo)
}

type ghIssueResponse struct {
	ID                int64     `json:"id"`
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	Body              string    `json:"body"`
	State             string    `json:"state"`
	HTMLURL           string    `json:"html_url"`
	AuthorAssociation string    `json:"author_association"`
	CreatedAt         time.Time `json:"created_at"`
	Type              *struct {
		Name string `json:"name"`
	} `json:"type,omitempty"`
	User struct {
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
	var issueType string
	if r.Type != nil {
		issueType = r.Type.Name
	}
	return &Issue{
		Number:            r.Number,
		Title:             r.Title,
		Body:              r.Body,
		State:             r.State,
		Type:              issueType,
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

func (g *GitHubTracker) CreateTypedIssue(ctx context.Context, title, body string, labels []string, issueType string) (*Issue, error) {
	if IsEpicIssueType(issueType) {
		if err := g.Authorize(ctx, "create-epic"); err != nil {
			return nil, err
		}
	}
	if err := g.CheckIssueType(ctx, issueType); err != nil {
		return nil, err
	}
	canonicalType := "Epic"
	payload := map[string]any{
		"title":  title,
		"body":   body,
		"labels": labels,
		"type":   canonicalType,
	}
	raw, _ := json.Marshal(payload)
	var resp ghIssueResponse
	path := fmt.Sprintf("repos/%s/%s/issues", g.owner, g.repo)
	if err := g.client.DoWithContext(ctx, "POST", path, bytes.NewReader(raw), &resp); err != nil {
		return nil, fmt.Errorf("github CreateTypedIssue (%s): %w", canonicalType, err)
	}
	iss := convertGHIssue(resp)
	if !IsEpicIssueType(iss.Type) {
		return nil, fmt.Errorf("github CreateTypedIssue (%s): response did not confirm issue type %q (got %q)", canonicalType, canonicalType, iss.Type)
	}
	return iss, nil
}

func httpErrorStatusCode(err error) int {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr != nil {
		return httpErr.StatusCode
	}
	return 0
}

func (g *GitHubTracker) ParentIssueNumber(ctx context.Context, childNumber int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if childNumber <= 0 {
		return 0, fmt.Errorf("invalid child issue number %d", childNumber)
	}
	var resp ghIssueResponse
	path := fmt.Sprintf("repos/%s/%s/issues/%d/parent", g.owner, g.repo, childNumber)
	if err := g.client.DoWithContext(ctx, "GET", path, nil, &resp); err != nil {
		if httpErrorStatusCode(err) == http.StatusNotFound {
			return 0, nil
		}
		return 0, fmt.Errorf("github ParentIssueNumber #%d: %w", childNumber, err)
	}
	return resp.Number, nil
}

func (g *GitHubTracker) LinkSubIssue(ctx context.Context, parentNumber, childNumber int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if parentNumber <= 0 || childNumber <= 0 {
		return fmt.Errorf("invalid parent #%d or child #%d issue number", parentNumber, childNumber)
	}
	if parentNumber == childNumber {
		return fmt.Errorf("issue #%d cannot be linked as a sub-issue of itself", childNumber)
	}
	if _, err := g.GetIssue(ctx, parentNumber); err != nil {
		return fmt.Errorf("github LinkSubIssue parent #%d: %w", parentNumber, err)
	}
	var childResp ghIssueResponse
	childPath := fmt.Sprintf("repos/%s/%s/issues/%d", g.owner, g.repo, childNumber)
	if err := g.client.DoWithContext(ctx, "GET", childPath, nil, &childResp); err != nil {
		return fmt.Errorf("github LinkSubIssue child #%d: %w", childNumber, err)
	}
	if childResp.ID <= 0 {
		return fmt.Errorf("github LinkSubIssue child #%d missing database id", childNumber)
	}
	existingParent, err := g.ParentIssueNumber(ctx, childNumber)
	if err != nil {
		return err
	}
	if existingParent == parentNumber {
		return nil
	}
	if existingParent != 0 {
		return fmt.Errorf("issue #%d is already linked to parent issue #%d (cannot link to #%d)", childNumber, existingParent, parentNumber)
	}
	raw, _ := json.Marshal(map[string]any{"sub_issue_id": childResp.ID})
	subPath := fmt.Sprintf("repos/%s/%s/issues/%d/sub_issues", g.owner, g.repo, parentNumber)
	var subResp any
	if err := g.client.DoWithContext(ctx, "POST", subPath, bytes.NewReader(raw), &subResp); err != nil {
		return fmt.Errorf("github LinkSubIssue #%d -> #%d: %w", parentNumber, childNumber, err)
	}
	return nil
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
		Type  string `json:"type"`
	} `json:"user"`
}

const maxCommentPages = 10
const maxCommentResponseBytes = 8 << 20

func (g *GitHubTracker) ListComments(ctx context.Context, issueNumber int) ([]*Comment, error) {
	return g.ListCommentsSince(ctx, issueNumber, time.Time{})
}

// ListCommentsSince bounds both the work and memory of a comment scan. Callers
// polling an outstanding request use the server timestamp of that request.
func (g *GitHubTracker) ListCommentsSince(ctx context.Context, issueNumber int, since time.Time) ([]*Comment, error) {
	var out []*Comment
	remaining := int64(maxCommentResponseBytes)
	for page := 1; page <= maxCommentPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100&page=%d", g.owner, g.repo, issueNumber, page)
		if !since.IsZero() {
			path += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
		}
		resp, err := g.client.RequestWithContext(ctx, "GET", path, nil)
		if err != nil {
			return nil, fmt.Errorf("github ListComments #%d page %d: %w", issueNumber, page, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, remaining+1))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if int64(len(data)) > remaining {
			return nil, fmt.Errorf("github comment scan exceeds %d bytes", maxCommentResponseBytes)
		}
		remaining -= int64(len(data))
		var rawList []ghCommentResponse
		if err := json.Unmarshal(data, &rawList); err != nil {
			return nil, fmt.Errorf("decode github comments: %w", err)
		}
		if len(rawList) > 100 {
			return nil, fmt.Errorf("github comment page exceeds 100 records")
		}
		for _, r := range rawList {
			out = append(out, &Comment{ID: r.ID, IssueNum: issueNumber, Author: r.User.Login, AuthorAssociation: r.AuthorAssociation, AuthorType: r.User.Type, Body: r.Body, HTMLURL: r.HTMLURL, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
		}
		if len(rawList) < 100 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("github comment scan exceeds %d pages", maxCommentPages)
}

func (g *GitHubTracker) GetComment(ctx context.Context, commentID int64) (*Comment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if commentID <= 0 {
		return nil, fmt.Errorf("invalid comment id %d", commentID)
	}
	path := fmt.Sprintf("repos/%s/%s/issues/comments/%d", g.owner, g.repo, commentID)
	var r ghCommentResponse
	if err := g.client.DoWithContext(ctx, "GET", path, nil, &r); err != nil {
		return nil, fmt.Errorf("github GetComment %d: %w", commentID, err)
	}
	return &Comment{
		ID:                r.ID,
		Author:            r.User.Login,
		AuthorAssociation: r.AuthorAssociation,
		AuthorType:        r.User.Type,
		Body:              r.Body,
		HTMLURL:           r.HTMLURL,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}, nil
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
		AuthorType:        r.User.Type,
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
		ID:         r.ID,
		Author:     r.User.Login,
		AuthorType: r.User.Type,
		Body:       r.Body,
		HTMLURL:    r.HTMLURL,
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
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
		State   string `json:"state"`
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
		State:   "open",
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
		State   string `json:"state"`
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
		State:   "open",
		Draft:   resp.Draft,
		HTMLURL: resp.HTMLURL,
	}, nil
}

func (g *GitHubTracker) FindDraftPR(ctx context.Context, head, base string) (*PullRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	qualifiedHead := head
	if !strings.Contains(head, ":") {
		qualifiedHead = g.owner + ":" + head
	}
	q := url.Values{"state": []string{"open"}, "head": []string{qualifiedHead}, "base": []string{base}, "per_page": []string{"100"}}
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
			return &PullRequest{Number: pr.Number, Title: pr.Title, Body: pr.Body, Head: head, Base: base, State: "open", Draft: true, HTMLURL: pr.HTMLURL}, nil
		}
	}
	return nil, nil
}

func (g *GitHubTracker) GetPullRequest(ctx context.Context, number int) (*PullRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("invalid pull request number %d", number)
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d", g.owner, g.repo, number)
	var resp struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		Draft   bool   `json:"draft"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	}
	if err := g.client.DoWithContext(ctx, "GET", path, nil, &resp); err != nil {
		return nil, fmt.Errorf("github GetPullRequest #%d: %w", number, err)
	}
	if strings.EqualFold(resp.State, "open") {
		if resp.Base.Ref == "" {
			return nil, fmt.Errorf("github PR #%d has no target branch", number)
		}
		// pulls/N can retain the base SHA from an earlier synchronization.
		// Resolve the branch ref itself before deciding what to rebase onto.
		var target struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		refPath := fmt.Sprintf("repos/%s/%s/git/ref/heads/%s", g.owner, g.repo, escapeRefPath(resp.Base.Ref))
		if err := g.client.DoWithContext(ctx, "GET", refPath, nil, &target); err != nil {
			return nil, fmt.Errorf("github PR #%d target branch %s: %w", number, resp.Base.Ref, err)
		}
		if target.Object.SHA == "" {
			return nil, fmt.Errorf("github PR #%d target branch has no commit", number)
		}
		resp.Base.SHA = target.Object.SHA
	}
	draft := resp.Draft
	if resp.State != "" && !strings.EqualFold(resp.State, "open") {
		draft = false
	}
	return &PullRequest{
		Number:   resp.Number,
		Title:    resp.Title,
		Body:     resp.Body,
		Head:     resp.Head.Ref,
		State:    resp.State,
		Merged:   resp.Merged,
		HeadRepo: resp.Head.Repo.FullName,
		HeadSHA:  resp.Head.SHA,
		Base:     resp.Base.Ref,
		BaseSHA:  resp.Base.SHA,
		BaseRepo: resp.Base.Repo.FullName,
		Draft:    draft,
		HTMLURL:  resp.HTMLURL,
	}, nil
}

// GetPullRequestReviews returns review metadata, not review bodies or comments:
// untrusted PR text must never become coordinator instructions.
func (g *GitHubTracker) GetPullRequestReviews(ctx context.Context, number int) ([]PullRequestReview, error) {
	if number <= 0 {
		return nil, fmt.Errorf("invalid pull request number %d", number)
	}
	var reviews []PullRequestReview
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews?per_page=100&page=%d", g.owner, g.repo, number, page)
		var resp []struct {
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			State       string    `json:"state"`
			CommitSHA   string    `json:"commit_id"`
			SubmittedAt time.Time `json:"submitted_at"`
		}
		if err := g.client.DoWithContext(ctx, "GET", path, nil, &resp); err != nil {
			return nil, fmt.Errorf("github GetPullRequestReviews #%d page %d: %w", number, page, err)
		}
		if len(resp) > 100 {
			return nil, fmt.Errorf("github PR review page exceeds 100 records")
		}
		for _, review := range resp {
			reviews = append(reviews, PullRequestReview{
				Author:      review.User.Login,
				State:       review.State,
				CommitSHA:   review.CommitSHA,
				SubmittedAt: review.SubmittedAt,
			})
		}
		if len(resp) < 100 {
			return reviews, nil
		}
	}
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

// escapeRefPath escapes each segment of a branch name so refs such as
// release/1.0 keep their slashes in REST paths.
func escapeRefPath(ref string) string {
	segments := strings.Split(ref, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
