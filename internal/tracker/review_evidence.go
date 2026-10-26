package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

const ReviewEvidenceLimit = 128 << 10
const ReviewBodyLimit = 8 << 10

type ReviewEvidence struct {
	Records     []ReviewEvidenceRecord
	Truncated   bool
	Unavailable string
}
type ReviewEvidenceRecord struct {
	ID        int64  `json:"id"`
	URL       string `json:"html_url"`
	Body      string `json:"body"`
	State     string `json:"state"`
	CommitSHA string `json:"commit_id"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Kind string `json:"-"`
}
type ReviewEvidenceReader interface {
	GetReviewEvidence(context.Context, int) (ReviewEvidence, error)
}

func boundReviewText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

// GetReviewEvidence is separate from status metadata: only work downloads bodies.
func (g *GitHubTracker) GetReviewEvidence(ctx context.Context, number int) (ReviewEvidence, error) {
	var out ReviewEvidence
	if number <= 0 {
		return out, fmt.Errorf("review evidence requires a positive PR number")
	}
	budget := ReviewEvidenceLimit / 2
	for _, endpoint := range []string{"reviews", "comments"} {
		for page := 1; page <= 10; page++ {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			path := fmt.Sprintf("repos/%s/%s/pulls/%d/%s?per_page=100&page=%d", g.owner, g.repo, number, endpoint, page)
			resp, err := g.client.RequestWithContext(ctx, "GET", path, nil)
			if err != nil {
				return out, fmt.Errorf("read PR #%d %s evidence: %w", number, endpoint, err)
			}
			data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
			resp.Body.Close()
			if err != nil {
				return out, err
			}
			if len(data) > 1<<20 {
				return out, fmt.Errorf("PR review evidence page exceeds 1 MiB")
			}
			var rows []ReviewEvidenceRecord
			if err = json.Unmarshal(data, &rows); err != nil {
				return out, fmt.Errorf("decode PR %s evidence: %w", endpoint, err)
			}
			if len(rows) > 100 {
				return out, fmt.Errorf("PR review evidence page exceeds 100 records")
			}
			for _, r := range rows {
				r.Kind = endpoint
				fields := []*string{&r.Body, &r.URL, &r.State, &r.CommitSHA, &r.Path, &r.User.Login}
				for _, f := range fields {
					if len(*f) > ReviewBodyLimit {
						*f = boundReviewText(*f, ReviewBodyLimit)
						out.Truncated = true
					}
				}
				cost := len(r.Body) + len(r.URL) + len(r.State) + len(r.CommitSHA) + len(r.Path) + len(r.User.Login) + 128
				if cost > budget {
					out.Truncated = true
					return out, nil
				}
				budget -= cost
				out.Records = append(out.Records, r)
			}
			if len(rows) < 100 {
				break
			}
			if page == 10 {
				out.Truncated = true
			}
		}
	}
	return out, nil
}
