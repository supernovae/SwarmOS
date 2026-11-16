package fork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
)

func httpStatus(code int) error {
	u, _ := url.Parse("https://api.github.com/repos/o/r/issues/1/comments")
	return fmt.Errorf("github CreateComment #1: %w", &api.HTTPError{StatusCode: code, RequestURL: u})
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// postOutcome decides what one CreateComment call does: store the comment or
// not, and which error to report.
type postOutcome struct {
	store bool
	err   error
}

// commentTracker stores comments in memory. Posts beyond the scripted outcomes
// store and succeed. Listing calls may fail per the scripted read errors, and
// stored comments become visible only after hiddenScans successful scans.
type commentTracker struct {
	tracker.Tracker
	mu          sync.Mutex
	posts       []postOutcome
	bodies      []string
	comments    []*tracker.Comment
	extra       []*tracker.Comment
	readErrs    []error
	reads       int
	hiddenScans int
	createdAt   time.Time
}

func (f *commentTracker) CreateComment(_ context.Context, issue int, body string) (*tracker.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := postOutcome{store: true}
	if n := len(f.bodies); n < len(f.posts) {
		out = f.posts[n]
	}
	f.bodies = append(f.bodies, body)
	var c *tracker.Comment
	if out.store {
		at := f.createdAt
		if at.IsZero() {
			at = time.Now()
		}
		c = &tracker.Comment{ID: 100 + int64(len(f.comments)), IssueNum: issue, Author: "worker", Body: body, CreatedAt: at, UpdatedAt: at}
		f.comments = append(f.comments, c)
	}
	if out.err != nil {
		return nil, out.err
	}
	return c, nil
}

func (f *commentTracker) ListComments(ctx context.Context, _ int) ([]*tracker.Comment, error) {
	return f.list(ctx, time.Time{})
}

func (f *commentTracker) list(ctx context.Context, since time.Time) ([]*tracker.Comment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.reads <= len(f.readErrs) && f.readErrs[f.reads-1] != nil {
		return nil, f.readErrs[f.reads-1]
	}
	var out []*tracker.Comment
	if f.hiddenScans > 0 {
		f.hiddenScans--
	} else {
		out = append(out, f.comments...)
	}
	out = append(out, f.extra...)
	var filtered []*tracker.Comment
	for _, c := range out {
		if since.IsZero() || !c.UpdatedAt.Before(since) {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}

// incrementalTracker enforces the since filter like GitHub does.
type incrementalTracker struct {
	*commentTracker
	sinces []time.Time
}

func (f *incrementalTracker) ListCommentsSince(ctx context.Context, _ int, since time.Time) ([]*tracker.Comment, error) {
	f.mu.Lock()
	f.sinces = append(f.sinces, since)
	f.mu.Unlock()
	return f.list(ctx, since)
}

type countingStore struct {
	coordination.Store
	mu    sync.Mutex
	state *lease.IssueState
	err   error
	raw   []byte
	loads int
}

func (s *countingStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	s.mu.Lock()
	s.loads++
	s.mu.Unlock()
	if s.err != nil {
		return nil, "", s.err
	}
	if s.raw != nil {
		return s.raw, "", nil
	}
	return progressStore{state: *s.state}.Load(ctx, n)
}

func releasedAt(id int64) *countingStore {
	st := lease.IssueState{LastForkRequestID: id}
	st.Lease.Status = "released"
	return &countingStore{state: &st}
}

func releaseRequest() lease.ForkRequest {
	return lease.ForkRequest{Operation: "release", IssueNumber: 1, ClaimID: "c1"}
}

func recordWaits(g *Gateway) *[]time.Duration {
	var waits []time.Duration
	g.wait = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	return &waits
}

func requestNonce(t *testing.T, body string) string {
	t.Helper()
	nonce := requestCommentNonce(body)
	if nonce == "" {
		t.Fatalf("request comment without nonce: %q", body)
	}
	return nonce
}

func TestTransientErrorClassification(t *testing.T) {
	live := context.Background()
	done, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		ctx  context.Context
		err  error
		want bool
	}{
		"500":              {live, httpStatus(500), true},
		"502":              {live, httpStatus(502), true},
		"503":              {live, httpStatus(503), true},
		"404":              {live, httpStatus(404), false},
		"422":              {live, httpStatus(422), false},
		"net timeout":      {live, &url.Error{Op: "Post", URL: "https://api.github.com", Err: timeoutError{}}, true},
		"plain error":      {live, errors.New("decode github comments"), false},
		"canceled":         {live, context.Canceled, false},
		"caller done":      {done, httpStatus(502), false},
		"caller deadline":  {done, context.DeadlineExceeded, false},
		"wrapped canceled": {live, fmt.Errorf("x: %w", context.Canceled), false},
	} {
		if got := isTransientError(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: transient=%v want %v", name, got, tc.want)
		}
	}
}

func TestRequestNonceRoundTrip(t *testing.T) {
	const nonce = "N0NCEN0NCEN0NCEN0NCE"
	req := lease.ForkRequest{Version: 1, Operation: "renew", IssueNumber: 7, ClaimID: "c1"}
	body, err := formatRequestComment(req, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestCommentNonce(body); got != nonce {
		t.Fatalf("nonce = %q in %q", got, body)
	}
	if got, err := ParseRequestComment(body); err != nil || *got != req {
		t.Fatalf("collapsed round trip = %+v, %v", got, err)
	}
	if got := requestCommentNonce(strings.ReplaceAll(body, "\n", "\r\n")); got != nonce {
		t.Fatalf("CRLF nonce = %q", got)
	}
	plain, err := FormatRequestComment(req)
	if err != nil || requestCommentNonce(plain) != "" {
		t.Fatalf("request without nonce: %q, %v", plain, err)
	}
	// The nonce is never part of the JSON payload, so a payload naming it is
	// an unknown field like any other.
	for name, raw := range map[string]string{
		"raw nonce field": RequestMarker + "\n" + `{"version":1,"operation":"renew","issue_number":7,"request_nonce":"x"}`,
		"unknown field":   RequestMarker + "\n" + `{"version":1,"operation":"renew","issue_number":7,"request_token":"x"}`,
	} {
		if _, err := ParseRequestComment(raw); err == nil {
			t.Fatalf("%s: accepted an unknown field", name)
		}
	}
	for _, bad := range []string{"short", "has space in it 0123", "-->injected<!--0123456"} {
		if _, err := formatRequestComment(req, bad); err == nil {
			t.Fatalf("accepted nonce %q", bad)
		}
	}
	// Only the summary line position counts.
	for name, other := range map[string]string{
		"own line": strings.Replace(plain, "\n", "\n<!-- swarm:request-nonce:"+nonce+" -->\n", 1),
		"in json":  strings.Replace(plain, "```json\n", "```json\n<!-- swarm:request-nonce:"+nonce+" -->\n", 1),
		"raw":      RequestMarker + " <!-- swarm:request-nonce:" + nonce + " -->\n" + `{"version":1,"operation":"renew","issue_number":7}`,
	} {
		if got := requestCommentNonce(other); got != "" {
			t.Fatalf("%s: nonce %q", name, got)
		}
	}

	nonces := map[string]bool{}
	for range 2 {
		tr := &commentTracker{}
		g := &Gateway{Tracker: tr, Store: releasedAt(100)}
		if _, err := g.Apply(t.Context(), releaseRequest()); err != nil {
			t.Fatal(err)
		}
		nonces[requestNonce(t, tr.bodies[0])] = true
	}
	if len(nonces) != 2 {
		t.Fatalf("Apply calls reused a nonce: %v", nonces)
	}
}

func TestGatewayPostRecoveryStored(t *testing.T) {
	for name, tc := range map[string]struct {
		postErr     error
		hiddenScans int
		skew        time.Duration
		wantWaits   []time.Duration
	}{
		"502":             {postErr: httpStatus(502)},
		"timeout":         {postErr: &url.Error{Op: "Post", URL: "u", Err: timeoutError{}}},
		"delayed":         {postErr: httpStatus(502), hiddenScans: 2, wantWaits: []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}},
		"server behind":   {postErr: httpStatus(502), skew: -2 * time.Minute},
		"server is ahead": {postErr: httpStatus(503), skew: 2 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			tr := &incrementalTracker{commentTracker: &commentTracker{posts: []postOutcome{{store: true, err: tc.postErr}}, hiddenScans: tc.hiddenScans}}
			if tc.skew != 0 {
				tr.createdAt = time.Now().Add(tc.skew)
			}
			var dispatched int64
			g := &Gateway{Tracker: tr, Store: releasedAt(100), Dispatcher: func(_ context.Context, _ int, c *tracker.Comment) error {
				dispatched = c.ID
				return nil
			}}
			waits := recordWaits(g)
			before := time.Now()
			st, err := g.Apply(t.Context(), releaseRequest())
			if err != nil || st == nil || st.LastForkRequestID != 100 {
				t.Fatalf("state=%+v err=%v", st, err)
			}
			if len(tr.bodies) != 1 || len(tr.comments) != 1 || dispatched != 100 {
				t.Fatalf("posts=%d stored=%d dispatched=%d", len(tr.bodies), len(tr.comments), dispatched)
			}
			if len(*waits) != len(tc.wantWaits) {
				t.Fatalf("waits=%v want %v", *waits, tc.wantWaits)
			}
			for i, w := range tc.wantWaits {
				if (*waits)[i] != w {
					t.Fatalf("waits=%v want %v", *waits, tc.wantWaits)
				}
			}
			// Every reconciliation scan keeps the initial ten-minute lookback.
			scans := 1 + tc.hiddenScans
			first := tr.sinces[0]
			if first.Before(before.Add(-reconcileLookback)) || first.After(before.Add(-reconcileLookback+time.Second)) {
				t.Fatalf("lookback since=%v before=%v", first, before)
			}
			for _, s := range tr.sinces[:scans] {
				if !s.Equal(first) {
					t.Fatalf("scan bound moved: %v", tr.sinces)
				}
			}
		})
	}
}

func TestGatewayPostRecoveryAbsent(t *testing.T) {
	t.Run("re-posts once", func(t *testing.T) {
		tr := &commentTracker{posts: []postOutcome{{err: httpStatus(502)}}}
		g := &Gateway{Tracker: tr, Store: releasedAt(100)}
		waits := recordWaits(g)
		if _, err := g.Apply(t.Context(), releaseRequest()); err != nil {
			t.Fatal(err)
		}
		if len(tr.bodies) != 2 || tr.bodies[0] != tr.bodies[1] || len(tr.comments) != 1 {
			t.Fatalf("posts=%d identical=%v stored=%d", len(tr.bodies), tr.bodies[0] == tr.bodies[1], len(tr.comments))
		}
		if len(*waits) != 2 || tr.reads < 3 {
			t.Fatalf("expected three complete scans before re-posting: waits=%v reads=%d", *waits, tr.reads)
		}
	})
	t.Run("second post stored then 502", func(t *testing.T) {
		tr := &commentTracker{posts: []postOutcome{{err: httpStatus(502)}, {store: true, err: httpStatus(502)}}}
		g := &Gateway{Tracker: tr, Store: releasedAt(100)}
		recordWaits(g)
		if _, err := g.Apply(t.Context(), releaseRequest()); err != nil {
			t.Fatal(err)
		}
		if len(tr.bodies) != 2 || len(tr.comments) != 1 {
			t.Fatalf("posts=%d stored=%d", len(tr.bodies), len(tr.comments))
		}
	})
	t.Run("second post absent", func(t *testing.T) {
		tr := &commentTracker{posts: []postOutcome{{err: httpStatus(502)}, {err: httpStatus(502)}}}
		g := &Gateway{Tracker: tr, Store: releasedAt(100)}
		recordWaits(g)
		_, err := g.Apply(t.Context(), releaseRequest())
		nonce := requestNonce(t, tr.bodies[0])
		if err == nil || !strings.Contains(err.Error(), "outcome unknown") || !strings.Contains(err.Error(), nonce) || httpStatusOf(err) != 502 {
			t.Fatalf("err=%v", err)
		}
		if len(tr.bodies) != 2 || len(tr.comments) != 0 {
			t.Fatalf("posts=%d stored=%d", len(tr.bodies), len(tr.comments))
		}
	})
	t.Run("non-transient post error", func(t *testing.T) {
		tr := &commentTracker{posts: []postOutcome{{err: httpStatus(422)}}}
		g := &Gateway{Tracker: tr, Store: releasedAt(100)}
		_, err := g.Apply(t.Context(), releaseRequest())
		if err == nil || !strings.HasPrefix(err.Error(), "post fork request comment: ") || len(tr.bodies) != 1 || tr.reads != 0 {
			t.Fatalf("posts=%d reads=%d err=%v", len(tr.bodies), tr.reads, err)
		}
	})
}

func httpStatusOf(err error) int {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}
	return 0
}

func TestGatewayPostRecoveryCandidates(t *testing.T) {
	now := time.Now()
	// candidate builds a comment from the worker's own request, altered.
	const nonce = "N0NCEN0NCEN0NCEN0NCE"
	type alter func(req *lease.ForkRequest, nonce *string, c *tracker.Comment)
	build := func(req lease.ForkRequest, id int64, change alter) *tracker.Comment {
		c := &tracker.Comment{ID: id, IssueNum: req.IssueNumber, CreatedAt: now, UpdatedAt: now}
		n := nonce
		change(&req, &n, c)
		if c.Body == "" {
			c.Body, _ = formatRequestComment(req, n)
		}
		return c
	}
	nonMatching := map[string]alter{
		"invalid id":    func(_ *lease.ForkRequest, _ *string, c *tracker.Comment) { c.ID = 0 },
		"wrong nonce":   func(_ *lease.ForkRequest, n *string, _ *tracker.Comment) { *n = "OTHEROTHEROTHEROTHER" },
		"no nonce":      func(_ *lease.ForkRequest, n *string, _ *tracker.Comment) { *n = "" },
		"wrong issue":   func(_ *lease.ForkRequest, _ *string, c *tracker.Comment) { c.IssueNum = 2 },
		"payload issue": func(r *lease.ForkRequest, _ *string, _ *tracker.Comment) { r.IssueNumber = 2 },
		"operation":     func(r *lease.ForkRequest, _ *string, _ *tracker.Comment) { r.Operation = "renew" },
		"fork repo":     func(r *lease.ForkRequest, _ *string, _ *tracker.Comment) { r.ForkRepo = "attacker/repo" },
		"claim":         func(r *lease.ForkRequest, _ *string, _ *tracker.Comment) { r.ClaimID = "other" },
		"edited":        func(_ *lease.ForkRequest, _ *string, c *tracker.Comment) { c.UpdatedAt = now.Add(time.Second) },
		"malformed":     func(_ *lease.ForkRequest, _ *string, c *tracker.Comment) { c.Body = RequestMarker + "\n{not json" },
		"oversized": func(r *lease.ForkRequest, n *string, c *tracker.Comment) {
			body, _ := formatRequestComment(*r, *n)
			c.Body = body + strings.Repeat(" ", MaxForkCommentBytes)
		},
	}
	req := lease.ForkRequest{Version: 1, Operation: "release", IssueNumber: 1, ClaimID: "c1"}
	want := mustMarshal(t, req)
	for name, change := range nonMatching {
		t.Run(name, func(t *testing.T) {
			got, err := matchRequestComment(t.Context(), []*tracker.Comment{nil, build(req, 100, change)}, req, nonce, want)
			if err != nil || got != nil {
				t.Fatalf("matched %+v err=%v", got, err)
			}
		})
	}
	exact := func(*lease.ForkRequest, *string, *tracker.Comment) {}
	if got, err := matchRequestComment(t.Context(), []*tracker.Comment{build(req, 100, exact)}, req, nonce, want); err != nil || got == nil || got.ID != 100 {
		t.Fatalf("exact match = %+v, %v", got, err)
	}
	if _, err := matchRequestComment(t.Context(), []*tracker.Comment{build(req, 100, exact), build(req, 101, exact)}, req, nonce, want); err == nil || !strings.Contains(err.Error(), "multiple comments") {
		t.Fatalf("ambiguous match err=%v", err)
	}
	many := make([]*tracker.Comment, 1001)
	if _, err := matchRequestComment(t.Context(), many, req, nonce, want); err == nil {
		t.Fatal("record budget not enforced")
	}
	big := strings.Repeat("x", 5<<20)
	if _, err := matchRequestComment(t.Context(), []*tracker.Comment{{ID: 1, Body: big}, {ID: 2, Body: big}}, req, nonce, want); err == nil {
		t.Fatal("byte budget not enforced")
	}

	t.Run("ambiguity or failed scan never re-posts", func(t *testing.T) {
		for name, setup := range map[string]func(*commentTracker){
			"two matches": func(tr *commentTracker) { tr.posts[0].store = true },
			"over budget": func(tr *commentTracker) { tr.extra = make([]*tracker.Comment, 1001) },
			"read 4xx":    func(tr *commentTracker) { tr.readErrs = []error{httpStatus(404)} },
		} {
			t.Run(name, func(t *testing.T) {
				tr := &commentTracker{posts: []postOutcome{{err: httpStatus(502)}}}
				setup(tr)
				g := &Gateway{Tracker: tr, Store: releasedAt(100)}
				recordWaits(g)
				if name == "two matches" {
					// Duplicate the stored comment under a second ID once it exists.
					g.Tracker = &dupTracker{commentTracker: tr}
				}
				_, err := g.Apply(t.Context(), releaseRequest())
				if err == nil || !strings.Contains(err.Error(), "outcome unknown") || len(tr.bodies) != 1 {
					t.Fatalf("posts=%d err=%v", len(tr.bodies), err)
				}
			})
		}
	})
}

// dupTracker lists every stored comment twice under distinct IDs.
type dupTracker struct{ *commentTracker }

func (d *dupTracker) ListComments(ctx context.Context, n int) ([]*tracker.Comment, error) {
	list, err := d.commentTracker.ListComments(ctx, n)
	var out []*tracker.Comment
	for _, c := range list {
		if c == nil {
			continue
		}
		cp := *c
		cp.ID += 50
		out = append(out, c, &cp)
	}
	return out, err
}

func mustMarshal(t *testing.T, req lease.ForkRequest) []byte {
	t.Helper()
	body, err := FormatRequestComment(req)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequestComment(body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestGatewayReadRetry(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		for _, failure := range []error{httpStatus(502), httpStatus(404), timeoutError{}} {
			ct := &commentTracker{readErrs: []error{failure}}
			var tr tracker.Tracker = ct
			if incremental {
				tr = &incrementalTracker{commentTracker: ct}
			}
			g := &Gateway{Tracker: tr}
			waits := recordWaits(g)
			_, err := g.listComments(t.Context(), 1, time.Time{})
			if !errors.Is(err, failure) || ct.reads != 1 || len(*waits) != 0 {
				t.Fatalf("reads=%d waits=%v err=%v", ct.reads, *waits, err)
			}
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ct := &commentTracker{}
	_, err := (&Gateway{Tracker: ct}).listComments(ctx, 1, time.Time{})
	if !errors.Is(err, context.Canceled) || ct.reads != 0 {
		t.Fatalf("reads=%d err=%v", ct.reads, err)
	}
	t.Run("real wait honors deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := (&Gateway{}).sleep(ctx, time.Minute); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
			t.Fatalf("wait: %v", err)
		}
	})
}

func TestGatewayFallbackLoadErrorsKeepPolling(t *testing.T) {
	gitErr := fmt.Errorf("git fetch upstream: %w", &exec.ExitError{})
	for name, store := range map[string]*countingStore{
		"not found": {err: coordination.ErrNotFound},
		"git error": {err: gitErr},
		"malformed": {raw: []byte("{not json")},
	} {
		t.Run(name, func(t *testing.T) {
			tr := &commentTracker{}
			g := &Gateway{Tracker: tr, Store: store, PollInterval: time.Millisecond, PollTimeout: 50 * time.Millisecond}
			waits := recordWaits(g)
			done := make(chan error, 1)
			go func() {
				_, err := g.Apply(t.Context(), releaseRequest())
				done <- err
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Apply did not stop at its polling deadline")
			}
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "request 100") {
				t.Fatalf("err=%v", err)
			}
			store.mu.Lock()
			loads := store.loads
			store.mu.Unlock()
			if tr.reads < 2 || loads < 2 || len(tr.bodies) != 1 || len(*waits) != 0 {
				t.Fatalf("reads=%d loads=%d posts=%d waits=%v", tr.reads, loads, len(tr.bodies), *waits)
			}
		})
	}
}

func TestGatewayOldCoordinatorRejection(t *testing.T) {
	now := time.Now()
	parseErr := `invalid fork request JSON: json: unknown field "future_field"`
	type alter func(r *ForkResult, c *tracker.Comment)
	run := func(t *testing.T, change alter) (bool, int, error) {
		res := ForkResult{Version: 1, RequestID: 100, IssueNumber: 1, Success: false, Error: parseErr}
		c := &tracker.Comment{ID: 101, Author: ActionsBotLogin, AuthorType: "Bot", CreatedAt: now, UpdatedAt: now}
		change(&res, c)
		c.Body, _ = FormatResultComment(res)
		tr := &commentTracker{extra: []*tracker.Comment{c}}
		store := &countingStore{err: coordination.ErrNotFound}
		g := &Gateway{Tracker: tr, Store: store, PollInterval: time.Millisecond, PollTimeout: 30 * time.Millisecond}
		rejected := false
		g.OnProgress = func(e ProgressEvent) { rejected = rejected || e.Status == "rejected" }
		_, err := g.Apply(t.Context(), releaseRequest())
		if len(tr.bodies) != 1 {
			t.Fatalf("posted %d times", len(tr.bodies))
		}
		return rejected, store.loads, err
	}
	rejected, loads, err := run(t, func(*ForkResult, *tracker.Comment) {})
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !rejected || loads != 0 {
		t.Fatalf("rejected=%v loads=%d err=%v", rejected, loads, err)
	}
	for _, want := range []string{"request 100", parseErr, "update its SwarmOS Fork Coordinator workflow"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q: %v", want, err)
		}
	}
	for name, change := range map[string]alter{
		"untrusted author":   func(_ *ForkResult, c *tracker.Comment) { c.Author = "attacker" },
		"edited":             func(_ *ForkResult, c *tracker.Comment) { c.UpdatedAt = now.Add(time.Second) },
		"wrong issue":        func(r *ForkResult, _ *tracker.Comment) { r.IssueNumber = 2 },
		"wrong request":      func(r *ForkResult, _ *tracker.Comment) { r.RequestID = 99 },
		"older comment":      func(_ *ForkResult, c *tracker.Comment) { c.ID = 99 },
		"mismatched op":      func(r *ForkResult, _ *tracker.Comment) { r.Operation = "renew" },
		"empty-op success":   func(r *ForkResult, _ *tracker.Comment) { r.Success = true },
		"mismatched success": func(r *ForkResult, _ *tracker.Comment) { r.Operation = "acquire"; r.Success = true },
	} {
		t.Run(name, func(t *testing.T) {
			rejected, _, err := run(t, change)
			if !errors.Is(err, context.DeadlineExceeded) || rejected {
				t.Fatalf("result was not ignored: rejected=%v err=%v", rejected, err)
			}
		})
	}
}

type budgetTransport func(*http.Request) (*http.Response, error)

func (f budgetTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayPostRetryBudget(t *testing.T) {
	t.Setenv("SWARM_GITHUB_TOKEN", "dummy-token")
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				old := http.DefaultTransport
				defer func() { http.DefaultTransport = old }()
				posts, gets, notices := 0, 0, 0
				var delays time.Duration
				var bodies []string
				http.DefaultTransport = budgetTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method != "POST" {
						gets++
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`[]`)), Request: r}, nil
					}
					posts++
					b, _ := io.ReadAll(r.Body)
					r.Body.Close()
					bodies = append(bodies, string(b))
					if success && posts == 4 {
						return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":123,"body":"ok"}`)), Request: r}, nil
					}
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}
				})
				tr, err := tracker.NewGitHubTracker("o/r")
				if err != nil {
					t.Fatal(err)
				}
				g := &Gateway{Tracker: tr}
				waits := recordWaits(g)
				ctx := gitcredential.WithNetworkRetry(t.Context(), "swarm --fork issue release 462", func(_ string, _, _ int, d time.Duration) { notices++; delays += d })
				_, err = g.postRequest(ctx, releaseRequest(), "nonce", "payload", time.Now())
				if success {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var exhausted *gitcredential.RetryExhaustedError
					var timeout net.Error
					if !errors.As(err, &exhausted) || !errors.As(err, &timeout) || exhausted.Attempts != 4 || exhausted.Operation != "GitHub REST POST" || !strings.Contains(err.Error(), "swarm --fork issue release 462") {
						t.Fatalf("terminal error: %v", err)
					}
					if isTransientError(ctx, fmt.Errorf("wrapped: %w", &url.Error{Op: "Post", URL: "u", Err: err})) {
						t.Fatal("exhaustion treated as transient")
					}
				}
				if posts != 4 || gets != 0 || notices != 3 || delays > time.Minute || len(*waits) != 0 {
					t.Fatalf("posts=%d gets=%d notices=%d delays=%v waits=%v", posts, gets, notices, delays, *waits)
				}
				for _, b := range bodies {
					if b != `{"body":"payload"}` {
						t.Fatalf("body=%q", b)
					}
				}
				t.Logf("posts=%d gets=%d notices=%d delay=%v", posts, gets, notices, delays)
			})
		})
	}
}

func TestGatewayPostRetryBudgetEarlyExhaustion(t *testing.T) {
	ctx := gitcredential.WithNetworkRetry(t.Context(), "swarm --fork issue release 462", nil)
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}
	var policy gitcredential.RetryPolicy
	again, err := policy.Next(ctx, gitcredential.RetryObservation{Operation: "GET", Err: cause, Status: 429, Header: http.Header{"Retry-After": []string{"61"}}})
	if again || err != nil {
		t.Fatalf("policy: %v %v", again, err)
	}
	terminal := policy.Error(ctx, "GitHub REST POST", cause)
	for _, failure := range []error{terminal, fmt.Errorf("wrapped: %w", terminal), &url.Error{Op: "Post", URL: "u", Err: terminal}} {
		ct := &commentTracker{posts: []postOutcome{{err: failure}}}
		g := &Gateway{Tracker: ct}
		waits := recordWaits(g)
		_, err = g.postRequest(ctx, releaseRequest(), "nonce", "payload", time.Now())
		var exhausted *gitcredential.RetryExhaustedError
		var dial *net.OpError
		if !errors.As(err, &exhausted) || exhausted.Attempts != 1 || exhausted.Operation != "GitHub REST POST" || !errors.As(err, &dial) || !dial.Timeout() || len(ct.bodies) != 1 || ct.reads != 0 || len(*waits) != 0 {
			t.Fatalf("posts=%d reads=%d waits=%v err=%v", len(ct.bodies), ct.reads, *waits, err)
		}
	}
}

type heartbeatGatewayTimer struct {
	ch    chan time.Time
	delay time.Duration
}

func (t *heartbeatGatewayTimer) C() <-chan time.Time { return t.ch }
func (t *heartbeatGatewayTimer) Stop() bool          { return true }

type observedHeartbeatGateway struct {
	gateway *Gateway
	calls   atomic.Int64
	errors  chan error
}

func (g *observedHeartbeatGateway) Apply(ctx context.Context, req lease.ForkRequest) (*lease.IssueState, error) {
	g.calls.Add(1)
	st, err := g.gateway.Apply(ctx, req)
	g.errors <- err
	return st, err
}

func TestHeartbeatGatewayTransport(t *testing.T) {
	for _, scenario := range []string{"post timeout", "post 503", "scan failure", "fence rejection", "max rejection", "committed renewal"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".swarm", "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			config := "version: 1\ndefault_agent: mock\ndefault_gate: none\nlease:\n  duration: 4h\n  renewable: true\n  max_renewals: 3\nsecurity:\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  mock:\n    binary: mock\n"
			if err := os.WriteFile(filepath.Join(root, ".swarm", "config.yaml"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".swarm", "workflows", "feature.yaml"), []byte("name: feature\nsteps:\n  - name: implement\n    role: developer\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base"}} {
				cmd := gitfixture.Command(t, "", args...)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
			}
			tr := tracker.NewLocalTracker(root).WithUser("maintainer", "OWNER")
			wu := &contract.WorkUnit{Objective: "Recover heartbeat", Constraints: []string{"Keep fencing"}, Acceptance: []string{"Survive transport"}, Workflow: "feature", Gate: contract.GateNone}
			issue, err := tr.CreateIssue(t.Context(), "heartbeat", contract.FormatIssueMarkdown(wu), nil)
			if err != nil {
				t.Fatal(err)
			}
			coord := lease.NewManager(tr, contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
			store := coordination.NewFileStore(root)
			coord.SetStore(root, store)
			base := time.Now()
			var elapsed atomic.Int64
			now := func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
			coord.SetNowFunc(now)
			if _, err = coord.ApproveTask(t.Context(), issue.Number, "maintainer"); err != nil {
				t.Fatal(err)
			}
			st, err := coord.Acquire(t.Context(), issue.Number, "maintainer", "mock", "implement", 4*time.Hour, 3)
			if err != nil {
				t.Fatal(err)
			}
			ct := &commentTracker{}
			var want error
			switch scenario {
			case "post timeout":
				want = &url.Error{Op: "Post", URL: "u", Err: timeoutError{}}
				ct.posts = []postOutcome{{err: want}, {err: want}}
			case "post 503":
				want = httpStatus(503)
				ct.posts = []postOutcome{{err: want}, {err: want}}
			case "scan failure", "committed renewal":
				want = httpStatus(502)
				ct.readErrs = []error{want}
			case "fence rejection", "max rejection":
				want = lease.ErrClaimFence
				if scenario == "max rejection" {
					want = lease.ErrMaxRenewals
				}
				body, err := FormatResultComment(ForkResult{Version: 1, Operation: "renew", RequestID: 100, IssueNumber: issue.Number, Success: false, Error: want.Error()})
				if err != nil {
					t.Fatal(err)
				}
				ct.extra = []*tracker.Comment{{ID: 101, Author: ActionsBotLogin, AuthorType: "Bot", Body: body, CreatedAt: base, UpdatedAt: base}}
			}
			gateway := &Gateway{Tracker: ct, Store: store}
			recordWaits(gateway)
			if scenario == "committed renewal" {
				gateway.Dispatcher = func(ctx context.Context, _ int, _ *tracker.Comment) error {
					_, err := coord.Renew(ctx, issue.Number, "maintainer", 4*time.Hour)
					return err
				}
			}
			observed := &observedHeartbeatGateway{gateway: gateway, errors: make(chan error, 10)}
			worker := lease.NewManager(tr, contract.SecurityConfig{RequireReadyLabel: true, ReadyLabel: "swarm:ready"})
			worker.SetStore(root, store)
			worker.SetNowFunc(now)
			worker.BindClaim(issue.Number, st.Lease.ClaimID)
			worker.SetWorkerGateway(observed)
			ticks := make(chan *heartbeatGatewayTimer, 10)
			worker.SetHeartbeatTimerFunc(func(d time.Duration) lease.HeartbeatTimer {
				timer := &heartbeatGatewayTimer{make(chan time.Time, 1), d}
				ticks <- timer
				return timer
			})
			receive := func() *heartbeatGatewayTimer {
				t.Helper()
				select {
				case timer := <-ticks:
					return timer
				case <-time.After(5 * time.Second):
					t.Fatal("no timer")
					return nil
				}
			}
			h := worker.StartHeartbeat(t.Context(), issue.Number, "maintainer", 4*time.Hour)
			defer h.Stop()
			timer := receive()
			elapsed.Store(int64(8 * time.Hour / 3))
			timer.ch <- now()
			var returned error
			select {
			case returned = <-observed.errors:
			case <-time.After(5 * time.Second):
				t.Fatal("Gateway.Apply did not finish")
			}
			if !errors.Is(returned, want) {
				t.Fatalf("gateway lost cause: %v", returned)
			}
			if scenario == "fence rejection" || scenario == "max rejection" {
				select {
				case <-h.Context().Done():
				case <-time.After(5 * time.Second):
					t.Fatal("fatal gateway error did not cancel")
				}
				if !errors.Is(h.Err(), want) {
					t.Fatalf("fatal cause: %v", h.Err())
				}
				select {
				case <-ticks:
					t.Fatal("fatal gateway retry")
				default:
				}
				return
			}
			timer = receive()
			if timer.delay != time.Second || h.Context().Err() != nil {
				t.Fatalf("gateway transport canceled: %v delay=%v", h.Err(), timer.delay)
			}
			if scenario == "committed renewal" {
				elapsed.Add(int64(time.Second))
				timer.ch <- now()
				receive()
				current, err := worker.GetCurrentState(t.Context(), issue.Number)
				if err != nil || current.Lease.Renewals != 1 || observed.calls.Load() != 1 {
					t.Fatalf("recovery duplicated committed renewal: %+v %v calls=%d", current, err, observed.calls.Load())
				}
			}
		})
	}
}
