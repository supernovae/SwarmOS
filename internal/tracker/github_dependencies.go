package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// BlockedByReference is a display-only native relationship, never dependency authority.
type BlockedByReference struct {
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// IssueReference parses a repository-qualified issue URL without guessing identity.
func IssueReference(raw string) (BlockedByReference, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return BlockedByReference{}, fmt.Errorf("invalid issue URL %q", raw)
	}
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(p) != 4 || p[0] == "" || p[1] == "" || p[2] != "issues" {
		return BlockedByReference{}, fmt.Errorf("invalid issue URL %q", raw)
	}
	n, err := strconv.Atoi(p[3])
	if err != nil || n <= 0 {
		return BlockedByReference{}, fmt.Errorf("invalid issue URL %q", raw)
	}
	return BlockedByReference{Owner: p[0], Repo: p[1], Number: n, HTMLURL: raw}, nil
}

func (r BlockedByReference) String() string {
	return fmt.Sprintf("%s/%s#%d", strings.ToLower(r.Owner), strings.ToLower(r.Repo), r.Number)
}

type nativeDependency struct {
	ID            int64  `json:"id"`
	Number        int    `json:"number"`
	HTMLURL       string `json:"html_url"`
	RepositoryURL string `json:"repository_url"`
}

func (g *GitHubTracker) dependencyPath(n int) string {
	return fmt.Sprintf("repos/%s/%s/issues/%d/dependencies/blocked_by", g.owner, g.repo, n)
}

func (g *GitHubTracker) blockedByRows(ctx context.Context, n int) ([]nativeDependency, error) {
	if n <= 0 {
		return nil, fmt.Errorf("invalid issue number %d", n)
	}
	var out []nativeDependency
	for page := 1; page <= 10000; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var rows []nativeDependency
		if err := g.client.DoWithContext(ctx, "GET", fmt.Sprintf("%s?per_page=100&page=%d", g.dependencyPath(n), page), nil, &rows); err != nil {
			return nil, fmt.Errorf("list blocked-by #%d page %d: %w", n, page, err)
		}
		out = append(out, rows...)
		if len(rows) < 100 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("blocked-by #%d pagination limit exceeded", n)
}

func (g *GitHubTracker) ListBlockedBy(ctx context.Context, n int) ([]BlockedByReference, error) {
	rows, err := g.blockedByRows(ctx, n)
	if err != nil {
		return nil, err
	}
	out := make([]BlockedByReference, 0, len(rows))
	for _, row := range rows {
		ref, err := IssueReference(row.HTMLURL)
		if err != nil && row.RepositoryURL != "" {
			u, e := url.Parse(row.RepositoryURL)
			if e == nil && u.Scheme == "https" && u.Host != "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil {
				p := strings.Split(strings.Trim(u.Path, "/"), "/")
				if len(p) >= 3 && p[len(p)-3] == "repos" && p[len(p)-2] != "" && p[len(p)-1] != "" && row.Number > 0 {
					ref = BlockedByReference{Owner: p[len(p)-2], Repo: p[len(p)-1], Number: row.Number, HTMLURL: row.HTMLURL}
					err = nil
				}
			}
		}
		if err != nil || ref.Number != row.Number || row.ID <= 0 {
			return nil, fmt.Errorf("blocked-by #%d contains incomplete issue identity", n)
		}
		out = append(out, ref)
	}
	return out, nil
}

func (g *GitHubTracker) nativeIssueID(ctx context.Context, n int) (int64, error) {
	if n <= 0 {
		return 0, fmt.Errorf("invalid issue number %d", n)
	}
	var row ghIssueResponse
	path := fmt.Sprintf("repos/%s/%s/issues/%d", g.owner, g.repo, n)
	if err := g.client.DoWithContext(ctx, "GET", path, nil, &row); err != nil {
		return 0, err
	}
	if row.ID <= 0 {
		return 0, fmt.Errorf("issue #%d missing database id", n)
	}
	return row.ID, nil
}

func (g *GitHubTracker) LinkBlockedBy(ctx context.Context, n, prerequisite int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n <= 0 || prerequisite <= 0 || n == prerequisite {
		return fmt.Errorf("invalid blocked-by relationship #%d -> #%d", n, prerequisite)
	}
	id, err := g.nativeIssueID(ctx, prerequisite)
	if err != nil {
		return fmt.Errorf("resolve prerequisite #%d: %w", prerequisite, err)
	}
	contains := func() (bool, error) {
		rows, e := g.blockedByRows(ctx, n)
		if e != nil {
			return false, e
		}
		for _, row := range rows {
			if row.ID == id {
				return true, nil
			}
		}
		return false, nil
	}
	exists, err := contains()
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"issue_id": id})
	var result any
	err = g.client.DoWithContext(ctx, "POST", g.dependencyPath(n), bytes.NewReader(raw), &result)
	if err == nil {
		return nil
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && (httpErr.StatusCode == 409 || httpErr.StatusCode == 422) {
		if found, readErr := contains(); readErr == nil && found {
			return nil
		}
	}
	return fmt.Errorf("link blocked-by #%d -> #%d: %w", n, prerequisite, err)
}

func (g *GitHubTracker) OrderSubIssues(ctx context.Context, parent int, children []int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if parent <= 0 {
		return fmt.Errorf("invalid parent issue number %d", parent)
	}
	seen := map[int]bool{}
	ordered := make([]int, 0, len(children))
	for _, n := range children {
		if n <= 0 || n == parent {
			return fmt.Errorf("invalid child issue #%d", n)
		}
		if !seen[n] {
			ordered = append(ordered, n)
			seen[n] = true
		}
	}
	if len(ordered) < 2 {
		return nil
	}
	member := map[int64]bool{}
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if page > 10000 {
			return fmt.Errorf("sub-issue priority pagination limit exceeded")
		}
		var siblings []ghIssueResponse
		path := fmt.Sprintf("repos/%s/%s/issues/%d/sub_issues?per_page=100&page=%d", g.owner, g.repo, parent, page)
		if err := g.client.DoWithContext(ctx, "GET", path, nil, &siblings); err != nil {
			return fmt.Errorf("priority membership under #%d: %w", parent, err)
		}
		for _, s := range siblings {
			if s.ID <= 0 {
				return fmt.Errorf("priority membership missing database id")
			}
			member[s.ID] = true
		}
		if len(siblings) < 100 {
			break
		}
	}
	ids := make([]int64, len(ordered))
	for i, n := range ordered {
		id, err := g.nativeIssueID(ctx, n)
		if err != nil {
			return err
		}
		if !member[id] {
			return fmt.Errorf("priority child #%d missing from parent #%d", n, parent)
		}
		ids[i] = id
	}
	path := fmt.Sprintf("repos/%s/%s/issues/%d/sub_issues/priority", g.owner, g.repo, parent)
	for i, id := range ids {
		payload := map[string]any{"sub_issue_id": id}
		if i == 0 {
			payload["before_id"] = ids[1]
		} else {
			payload["after_id"] = ids[i-1]
		}
		raw, _ := json.Marshal(payload)
		var result any
		if err := g.client.DoWithContext(ctx, "PATCH", path, bytes.NewReader(raw), &result); err != nil {
			return fmt.Errorf("order sub-issue #%d under #%d: %w", ordered[i], parent, err)
		}
	}
	return nil
}
