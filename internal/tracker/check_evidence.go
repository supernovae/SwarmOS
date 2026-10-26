package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

const CILogLimit = 32 << 10
const CIAnnotationLimit = 64 << 10

type CheckEvidence struct {
	Check       CheckRunResult
	Annotations []string
	LogTail     string
	Unavailable string
	Truncated   bool
	// CodeScanning is PR-wide; the coordinator attaches it to one entry.
	CodeScanning *CodeScanningEvidence
}

type CheckEvidenceReader interface {
	GetCheckEvidence(context.Context, CheckRunResult) (CheckEvidence, error)
}

func CurrentCIFailure(c CheckRunResult, head string) bool {
	return head != "" && c.HeadSHA == head && c.Status == "completed" && (c.Conclusion == "failure" || c.Conclusion == "cancelled")
}

// Only GitHub's HTTPS log storage may receive an API redirect. The API
// transport does not attach its token to these storage domains.
func ciRedirect(req *http.Request, via []*http.Request) error {
	host := strings.ToLower(req.URL.Hostname())
	if len(via) > 3 || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Port() != "" || !(strings.HasSuffix(host, ".blob.core.windows.net") || strings.HasSuffix(host, ".actions.githubusercontent.com")) {
		return errors.New("CI redirect destination rejected")
	}
	req.Header.Del("Authorization")
	req.Header.Del("Cookie")
	return nil
}

// Injected fixture clients already supply their own transport policy.
func (g *GitHubTracker) evidenceClient() *api.RESTClient {
	if g.ciClient != nil {
		return g.ciClient
	}
	return g.client
}

func (g *GitHubTracker) ciJSON(ctx context.Context, path string, dest any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := g.evidenceClient().RequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("CI metadata request unavailable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return errors.New("CI metadata read failed")
	}
	if len(data) > 2<<20 {
		return errors.New("CI metadata response exceeds 2 MiB")
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return errors.New("CI metadata response invalid")
	}
	return nil
}

// readCITail uses fixed storage regardless of the log's size.
func readCITail(r io.Reader, limit int) (string, bool, error) {
	if limit <= 0 {
		return "", false, nil
	}
	ring := make([]byte, limit)
	buf := make([]byte, 8192)
	pos, total := 0, 0
	truncated := false
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			ring[pos] = b
			pos = (pos + 1) % limit
			if total < limit {
				total++
			} else {
				truncated = true
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, err
		}
	}
	if total < limit {
		return string(ring[:total]), false, nil
	}
	return string(append(ring[pos:], ring[:pos]...)), truncated, nil
}

func (g *GitHubTracker) GetCheckEvidence(ctx context.Context, check CheckRunResult) (CheckEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	e := CheckEvidence{Check: check, Unavailable: "Job log unavailable for this check provider."}
	base := fmt.Sprintf("repos/%s/%s", g.owner, g.repo)
	remaining := CIAnnotationLimit
	for page := 1; remaining > 0; page++ {
		var annotations []struct {
			Path      string `json:"path"`
			Message   string `json:"message"`
			Title     string `json:"title"`
			StartLine int    `json:"start_line"`
		}
		err := g.ciJSON(ctx, fmt.Sprintf("%s/check-runs/%d/annotations?per_page=100&page=%d", base, check.ID, page), &annotations)
		if err != nil {
			if ctx.Err() != nil {
				return e, ctx.Err()
			}
			e.Annotations = append(e.Annotations, "Annotations unavailable.")
			break
		}
		for _, a := range annotations {
			text := fmt.Sprintf("%s:%d %s\n%s", a.Path, a.StartLine, a.Title, a.Message)
			cap := 4096
			if cap > remaining {
				cap = remaining
			}
			if len(text) > cap {
				text = text[:cap]
				e.Truncated = true
			}
			if cap > 0 {
				e.Annotations = append(e.Annotations, text)
				remaining -= len(text)
			}
		}
		if len(annotations) < 100 {
			break
		}
		if remaining == 0 {
			e.Truncated = true
		}
	}
	if check.App != "github-actions" || check.CheckSuite.ID <= 0 {
		return e, nil
	}
	e.Unavailable = "Job log unavailable (missing permission, expired log, or cancelled job)."
	// Query the suite's workflow runs, then match the job's numeric check URL
	// and head. Names and contributor-controlled details URLs are never used.
	for runPage := 1; ; runPage++ {
		var runs struct {
			Runs []struct {
				ID      int64  `json:"id"`
				HeadSHA string `json:"head_sha"`
			} `json:"workflow_runs"`
		}
		if err := g.ciJSON(ctx, fmt.Sprintf("%s/actions/runs?check_suite_id=%d&per_page=100&page=%d", base, check.CheckSuite.ID, runPage), &runs); err != nil {
			if ctx.Err() != nil {
				return e, ctx.Err()
			}
			return e, nil
		}
		for _, run := range runs.Runs {
			if run.HeadSHA != check.HeadSHA {
				continue
			}
			for page := 1; ; page++ {
				var jobs struct {
					Jobs []struct {
						ID          int64  `json:"id"`
						HeadSHA     string `json:"head_sha"`
						CheckRunURL string `json:"check_run_url"`
					} `json:"jobs"`
				}
				if err := g.ciJSON(ctx, fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100&page=%d", base, run.ID, page), &jobs); err != nil {
					if ctx.Err() != nil {
						return e, ctx.Err()
					}
					return e, nil
				}
				for _, job := range jobs.Jobs {
					u, err := url.Parse(job.CheckRunURL)
					if err != nil || job.HeadSHA != check.HeadSHA || u.Path != "/"+base+"/check-runs/"+strconv.FormatInt(check.ID, 10) {
						continue
					}
					resp, err := g.evidenceClient().RequestWithContext(ctx, "GET", fmt.Sprintf("%s/actions/jobs/%d/logs", base, job.ID), nil)
					if err != nil {
						if ctx.Err() != nil {
							return e, ctx.Err()
						}
						return e, nil
					}
					tail, truncated, err := readCITail(resp.Body, CILogLimit)
					resp.Body.Close()
					if err != nil {
						if ctx.Err() != nil {
							return e, ctx.Err()
						}
						return e, nil
					}
					e.LogTail = tail
					e.Truncated = e.Truncated || truncated
					e.Unavailable = ""
					return e, nil
				}
				if len(jobs.Jobs) < 100 {
					break
				}
			}
		}
		if len(runs.Runs) < 100 {
			break
		}
	}
	return e, nil
}
