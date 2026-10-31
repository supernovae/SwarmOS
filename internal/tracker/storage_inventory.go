package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cli/go-gh/v2/pkg/api"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/safefs"
)

// InventoryBudget belongs to the whole report, not to a backend or page.
// Reserve must reject an admission before allocating the requested storage.
type InventoryBudget struct {
	Records   int
	Pages     int
	Bytes     int64
	Reserve   func(int64) error
	Workspace func(int64) error
}

// PullRequestVisitor is optional: existing Tracker implementations need not
// provide it. Errors mean the traversal is incomplete, including after visits.
// Visitors must not use a partial traversal to establish eligibility.
type PullRequestVisitor interface {
	VisitPullRequests(context.Context, InventoryBudget, func(*PullRequest) error) error
}

var ErrInventoryLimit = errors.New("tracker inventory limit reached")

// GitHub bodies allow 65,536 characters; JSON escaping and UTF-8 can multiply
// their wire size. Keep a hard per-string bound as well as the charged input cap.
const inventoryQuotedBytesLimit = 512 << 10

// inventoryReader bounds JSON before decoding, including strings and nesting.
// Decoder lookahead and token allocations use reusable page workspace when
// provided. Retained projections are charged separately from transport input.
type inventoryReader struct {
	ctx                context.Context
	src                io.Reader
	budget             InventoryBudget
	buf                [4096]byte
	depth, quotedBytes int
	readBytes          int64
	quoted, escape     bool
}

func (r *inventoryReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.budget.Bytes <= 0 {
		return 0, ErrInventoryLimit
	}
	nmax := min(len(p), len(r.buf), int(r.budget.Bytes))
	n, err := r.src.Read(r.buf[:nmax])
	if n > 0 {
		r.readBytes += int64(n)
		reserve := r.budget.Reserve
		charge := int64(n) * 64
		if r.budget.Workspace != nil {
			reserve = r.budget.Workspace
			charge = r.readBytes * 4
		}
		if e := reserve(charge); e != nil {
			return 0, e
		}
		r.budget.Bytes -= int64(n)
		for _, c := range r.buf[:n] {
			if r.quoted {
				r.quotedBytes++
				if r.quotedBytes > inventoryQuotedBytesLimit {
					return 0, ErrInventoryLimit
				}
				if r.escape {
					r.escape = false
				} else if c == '\\' {
					r.escape = true
				} else if c == '"' {
					r.quoted = false
				}
			} else {
				switch c {
				case '"':
					r.quoted = true
					r.quotedBytes = 0
				case '{', '[':
					r.depth++
				case '}', ']':
					r.depth--
				}
				if r.depth > 32 || r.depth < 0 {
					return 0, ErrInventoryLimit
				}
			}
		}
		copy(p, r.buf[:n])
	}
	return n, err
}
func inventoryDecoder(ctx context.Context, src io.Reader, b InventoryBudget) (*json.Decoder, error) {
	if b.Records < 1 || b.Pages < 1 || b.Bytes < 1 || b.Reserve == nil {
		return nil, ErrInventoryLimit
	}
	if err := b.Reserve(16384); err != nil {
		return nil, err
	}
	return json.NewDecoder(&inventoryReader{ctx: ctx, src: src, budget: b}), nil
}
func expectJSON(d *json.Decoder, want json.Delim) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if t != want {
		return fmt.Errorf("unexpected inventory JSON structure")
	}
	return nil
}
func skipJSON(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := t.(json.Delim); ok {
		if delim != '{' && delim != '[' {
			return fmt.Errorf("invalid JSON value")
		}
		for d.More() {
			if err := skipJSON(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	return nil
}
func endJSON(d *json.Decoder) error {
	_, err := d.Token()
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("trailing inventory JSON")
	}
	return err
}

// VisitPullRequests opens only the existing state file, without a lock, store
// initialization or Git ref refresh. Other local-state values are streamed.
func (l *LocalTracker) VisitPullRequests(ctx context.Context, b InventoryBudget, visit func(*PullRequest) error) error {
	path, err := filepath.Abs(l.statePath)
	if err != nil {
		return err
	}
	root, err := safefs.Open(filepath.VolumeName(path) + string(filepath.Separator))
	if err != nil {
		return err
	}
	defer root.Close()
	rel, err := filepath.Rel(rootPath(path), path)
	if err != nil {
		return err
	}
	f, err := root.OpenRegular(rel)
	if err != nil {
		return err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return err
	}
	d, err := inventoryDecoder(ctx, f, b)
	if err != nil {
		return err
	}
	if err = expectJSON(d, '{'); err != nil {
		return err
	}
	found, count := false, 0
	seen := map[int]bool{}
	for d.More() {
		if err = ctx.Err(); err != nil {
			return err
		}
		key, e := d.Token()
		if e != nil {
			return e
		}
		if key != "pull_requests" {
			if err = skipJSON(d); err != nil {
				return err
			}
			continue
		}
		if found {
			return fmt.Errorf("duplicate PR map")
		}
		found = true
		if err = expectJSON(d, '{'); err != nil {
			return err
		}
		for d.More() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if count >= b.Records {
				return ErrInventoryLimit
			}
			key, e := d.Token()
			if e != nil {
				return e
			}
			text, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid PR key")
			}
			number, e := strconv.Atoi(text)
			if e != nil || number < 1 {
				return fmt.Errorf("invalid PR number")
			}
			var pr PullRequest
			if err = d.Decode(&pr); err != nil {
				return err
			}
			if seen[number] {
				return fmt.Errorf("duplicate PR identity")
			}
			seen[number] = true
			if pr.Number != number {
				return fmt.Errorf("PR identity mismatch")
			}
			if err = b.Reserve(4096); err != nil {
				return err
			}
			count++
			if err = visit(&pr); err != nil {
				return err
			}
		}
		if err = expectJSON(d, '}'); err != nil {
			return err
		}
	}
	if err = expectJSON(d, '}'); err != nil {
		return err
	}
	if err = endJSON(d); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("PR map unavailable")
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("tracker changed during observation")
	}
	return nil
}
func rootPath(path string) string { return filepath.VolumeName(path) + string(filepath.Separator) }

func (g *GitHubTracker) VisitPullRequests(ctx context.Context, b InventoryBudget, visit func(*PullRequest) error) error {
	if b.Records < 1 || b.Pages < 1 || b.Bytes < 1 || b.Reserve == nil {
		return ErrInventoryLimit
	}
	if g.inventoryClient == nil {
		return fmt.Errorf("bounded PR inventory unavailable")
	}
	count := 0
	for page := 1; page <= b.Pages; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		q := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := b.Reserve(65536); err != nil {
			return err
		}
		response, err := g.inventoryClient.RequestWithContext(ctx, "GET", fmt.Sprintf("repos/%s/%s/pulls?%s", g.owner, g.repo, q.Encode()), nil)
		if err != nil {
			return fmt.Errorf("PR inventory unavailable: %w", err)
		}
		// Retain no transport diagnostics, response bodies or credential-bearing URLs.
		if response.StatusCode != 200 {
			response.Body.Close()
			return fmt.Errorf("PR inventory HTTP status %d", response.StatusCode)
		}
		if err := b.Reserve(16384); err != nil {
			response.Body.Close()
			return err
		}
		r := &inventoryReader{ctx: ctx, src: response.Body, budget: b}
		d := json.NewDecoder(r)
		pageCount := 0
		read := func() error {
			if err := expectJSON(d, '['); err != nil {
				return err
			}
			for d.More() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if count >= b.Records || pageCount >= 100 {
					return ErrInventoryLimit
				}
				var raw struct {
					Number         int        `json:"number"`
					State          string     `json:"state"`
					MergedAt       *time.Time `json:"merged_at"`
					MergeCommitSHA string     `json:"merge_commit_sha"`
					UpdatedAt      time.Time  `json:"updated_at"`
					Head           struct {
						Ref, SHA string
						Repo     struct {
							FullName string `json:"full_name"`
						}
					} `json:"head"`
					Base struct {
						Ref  string
						Repo struct {
							FullName string `json:"full_name"`
						}
					} `json:"base"`
				}
				if err := d.Decode(&raw); err != nil {
					return err
				}
				if raw.Number < 1 || !strings.Contains("|open|closed|", "|"+raw.State+"|") {
					return fmt.Errorf("invalid PR inventory record")
				}
				// The transport workspace is reusable; strings retained across pages
				// belong to this projection and must remain charged independently.
				retained := len(raw.State) + len(raw.MergeCommitSHA) + len(raw.Head.Ref) + len(raw.Head.SHA) + len(raw.Head.Repo.FullName) + len(raw.Base.Ref) + len(raw.Base.Repo.FullName)
				if err := b.Reserve(int64(4096 + 2*retained)); err != nil {
					return err
				}
				count++
				pageCount++
				if err := visit(&PullRequest{Number: raw.Number, State: raw.State, Merged: raw.MergedAt != nil, MergedAt: raw.MergedAt, MergeCommitSHA: raw.MergeCommitSHA, UpdatedAt: raw.UpdatedAt, Head: raw.Head.Ref, HeadSHA: raw.Head.SHA, HeadRepo: raw.Head.Repo.FullName, Base: raw.Base.Ref, BaseRepo: raw.Base.Repo.FullName}); err != nil {
					return err
				}
			}
			if err := expectJSON(d, ']'); err != nil {
				return err
			}
			return endJSON(d)
		}
		err = read()
		response.Body.Close()
		// One shared input-byte budget across pages, including decoder lookahead.
		b.Bytes = r.budget.Bytes
		if err != nil {
			return err
		}
		if pageCount < 100 {
			return nil
		}
	}
	return ErrInventoryLimit
}

// A separate client refuses redirects and discards error bodies before go-gh
// can decode them. The ordinary client's error handling uses io.ReadAll.
type inventoryTransport struct{ base http.RoundTripper }

func (t inventoryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(""))
		resp.Header = resp.Header.Clone()
		resp.Header.Set("Content-Type", "text/plain")
	}
	return resp, err
}
func newInventoryRESTClient(options api.ClientOptions) (*api.RESTClient, error) {
	transport := options.Transport
	if transport == nil {
		transport = http.DefaultTransport
		if base, ok := transport.(*http.Transport); ok {
			clone := base.Clone()
			clone.MaxResponseHeaderBytes = 32768
			transport = clone
		}
	}
	options.Transport = inventoryTransport{base: transport}
	options.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	options.EnableCache = false
	options.LogIgnoreEnv = true
	options.Log = io.Discard
	options.LogVerboseHTTP = false
	return api.NewRESTClient(options)
}

// StateRefVisitor observes authoritative remote ledger object identities. It
// performs no fetch; inventory reads only locally available pinned objects.
type StateRefVisitor interface {
	VisitStateRefs(context.Context, InventoryBudget, func(int, string) error) error
}

func (g *GitHubTracker) VisitStateRefs(ctx context.Context, b InventoryBudget, visit func(int, string) error) error {
	if g.inventoryClient == nil || b.Records < 1 || b.Pages < 2 || b.Reserve == nil || b.Bytes < 1 {
		return ErrInventoryLimit
	}
	count := 0
	seen := map[int]bool{}
	for _, prefix := range []string{"heads/swarm-state/", "swarm/state/"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.Reserve(65536); err != nil {
			return err
		}
		response, err := g.inventoryClient.RequestWithContext(ctx, "GET", fmt.Sprintf("repos/%s/%s/git/matching-refs/%s", g.owner, g.repo, prefix), nil)
		if err != nil {
			return fmt.Errorf("state ref inventory unavailable")
		}
		d, err := inventoryDecoder(ctx, response.Body, b)
		if err != nil {
			response.Body.Close()
			return err
		}
		read := func() error {
			if err := expectJSON(d, '['); err != nil {
				return err
			}
			for d.More() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if count >= b.Records {
					return ErrInventoryLimit
				}
				var ref struct {
					Ref    string                     `json:"ref"`
					Object struct{ SHA, Type string } `json:"object"`
				}
				if err := d.Decode(&ref); err != nil {
					return err
				}
				text, ok := strings.CutPrefix(ref.Ref, "refs/"+prefix+"issue-")
				number, err := strconv.Atoi(text)
				if !ok || err != nil || number < 1 || ref.Object.Type != "commit" || !inventoryObjectSHA(ref.Object.SHA) {
					return fmt.Errorf("invalid ledger ref identity")
				}
				if seen[number] {
					return fmt.Errorf("ledger namespace conflict")
				}
				if err = b.Reserve(1024); err != nil {
					return err
				}
				seen[number] = true
				count++
				if err = visit(number, ref.Object.SHA); err != nil {
					return err
				}
			}
			if err := expectJSON(d, ']'); err != nil {
				return err
			}
			return endJSON(d)
		}
		err = read()
		response.Body.Close()
		if err != nil {
			return err
		}
		// Decoder's input is independently capped per namespace. The report's
		// shared reserve accounts all bytes before parsing either response.
	}
	return nil
}
func inventoryObjectSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}
