package fork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
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
	req, err := ParseRequestComment(body)
	if err != nil || req.RequestNonce == "" {
		t.Fatalf("request comment without nonce: %v", err)
	}
	return req.RequestNonce
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
	req := lease.ForkRequest{Version: 1, Operation: "renew", IssueNumber: 7, ClaimID: "c1", RequestNonce: "N0NCE"}
	body, err := FormatRequestComment(req)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseRequestComment(body); err != nil || got.RequestNonce != "N0NCE" {
		t.Fatalf("collapsed round trip = %+v, %v", got, err)
	}
	legacy := RequestMarker + "\n" + `{"version":1,"operation":"renew","issue_number":7,"request_nonce":"N0NCE"}`
	if got, err := ParseRequestComment(legacy); err != nil || got.RequestNonce != "N0NCE" {
		t.Fatalf("raw layout = %+v, %v", got, err)
	}
	old := RequestMarker + "\n" + `{"version":1,"operation":"renew","issue_number":7}`
	if got, err := ParseRequestComment(old); err != nil || got.RequestNonce != "" {
		t.Fatalf("request without nonce = %+v, %v", got, err)
	}
	unknown := RequestMarker + "\n" + `{"version":1,"operation":"renew","issue_number":7,"request_token":"x"}`
	if _, err := ParseRequestComment(unknown); err == nil {
		t.Fatal("accepted an unknown field")
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
	type alter func(req *lease.ForkRequest, c *tracker.Comment)
	build := func(req lease.ForkRequest, id int64, change alter) *tracker.Comment {
		c := &tracker.Comment{ID: id, IssueNum: req.IssueNumber, CreatedAt: now, UpdatedAt: now}
		change(&req, c)
		if c.Body == "" {
			c.Body, _ = FormatRequestComment(req)
		}
		return c
	}
	nonMatching := map[string]alter{
		"invalid id":    func(_ *lease.ForkRequest, c *tracker.Comment) { c.ID = 0 },
		"wrong nonce":   func(r *lease.ForkRequest, _ *tracker.Comment) { r.RequestNonce = "OTHER" },
		"no nonce":      func(r *lease.ForkRequest, _ *tracker.Comment) { r.RequestNonce = "" },
		"wrong issue":   func(_ *lease.ForkRequest, c *tracker.Comment) { c.IssueNum = 2 },
		"payload issue": func(r *lease.ForkRequest, _ *tracker.Comment) { r.IssueNumber = 2 },
		"operation":     func(r *lease.ForkRequest, _ *tracker.Comment) { r.Operation = "renew" },
		"fork repo":     func(r *lease.ForkRequest, _ *tracker.Comment) { r.ForkRepo = "attacker/repo" },
		"claim":         func(r *lease.ForkRequest, _ *tracker.Comment) { r.ClaimID = "other" },
		"edited":        func(_ *lease.ForkRequest, c *tracker.Comment) { c.UpdatedAt = now.Add(time.Second) },
		"malformed":     func(_ *lease.ForkRequest, c *tracker.Comment) { c.Body = RequestMarker + "\n{not json" },
		"oversized": func(r *lease.ForkRequest, c *tracker.Comment) {
			body, _ := FormatRequestComment(*r)
			c.Body = body + strings.Repeat(" ", MaxForkCommentBytes)
		},
	}
	req := lease.ForkRequest{Version: 1, Operation: "release", IssueNumber: 1, ClaimID: "c1", RequestNonce: "N0NCE"}
	want := mustMarshal(t, req)
	for name, change := range nonMatching {
		t.Run(name, func(t *testing.T) {
			got, err := matchRequestComment(t.Context(), []*tracker.Comment{nil, build(req, 100, change)}, req, want)
			if err != nil || got != nil {
				t.Fatalf("matched %+v err=%v", got, err)
			}
		})
	}
	exact := func(*lease.ForkRequest, *tracker.Comment) {}
	if got, err := matchRequestComment(t.Context(), []*tracker.Comment{build(req, 100, exact)}, req, want); err != nil || got == nil || got.ID != 100 {
		t.Fatalf("exact match = %+v, %v", got, err)
	}
	if _, err := matchRequestComment(t.Context(), []*tracker.Comment{build(req, 100, exact), build(req, 101, exact)}, req, want); err == nil || !strings.Contains(err.Error(), "multiple comments") {
		t.Fatalf("ambiguous match err=%v", err)
	}
	many := make([]*tracker.Comment, 1001)
	if _, err := matchRequestComment(t.Context(), many, req, want); err == nil {
		t.Fatal("record budget not enforced")
	}
	big := strings.Repeat("x", 5<<20)
	if _, err := matchRequestComment(t.Context(), []*tracker.Comment{{ID: 1, Body: big}, {ID: 2, Body: big}}, req, want); err == nil {
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
	schedule := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}
	for _, incremental := range []bool{false, true} {
		for _, phase := range []string{"reconcile", "poll"} {
			name := fmt.Sprintf("%s/incremental=%v", phase, incremental)
			// A recovered reconciliation is followed by one outcome poll.
			pollReads := 0
			if phase == "reconcile" {
				pollReads = 1
			}
			newGateway := func(readErrs []error) (*Gateway, *commentTracker, *[]time.Duration) {
				ct := &commentTracker{readErrs: readErrs}
				if phase == "reconcile" {
					ct.posts = []postOutcome{{store: true, err: httpStatus(502)}}
				}
				var tr tracker.Tracker = ct
				if incremental {
					tr = &incrementalTracker{commentTracker: ct}
				}
				g := &Gateway{Tracker: tr, Store: releasedAt(100)}
				return g, ct, recordWaits(g)
			}
			t.Run(name+"/recovers", func(t *testing.T) {
				for _, code := range []int{500, 502, 503} {
					g, tr, waits := newGateway([]error{httpStatus(code), httpStatus(code)})
					if _, err := g.Apply(t.Context(), releaseRequest()); err != nil {
						t.Fatal(err)
					}
					if tr.reads != 3+pollReads || len(tr.bodies) != 1 || len(*waits) != 2 || (*waits)[0] != schedule[0] || (*waits)[1] != schedule[1] {
						t.Fatalf("reads=%d posts=%d waits=%v", tr.reads, len(tr.bodies), *waits)
					}
				}
			})
			t.Run(name+"/persistent", func(t *testing.T) {
				g, tr, waits := newGateway([]error{httpStatus(502), httpStatus(502), httpStatus(502), httpStatus(502)})
				_, err := g.Apply(t.Context(), releaseRequest())
				if err == nil || tr.reads != 3 || len(tr.bodies) != 1 || len(*waits) != 2 || httpStatusOf(err) != 502 {
					t.Fatalf("reads=%d posts=%d waits=%v err=%v", tr.reads, len(tr.bodies), *waits, err)
				}
				want := "request 100"
				if phase == "reconcile" {
					want = "nonce " + requestNonce(t, tr.bodies[0])
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error does not identify the request (%s): %v", want, err)
				}
			})
			t.Run(name+"/4xx not retried", func(t *testing.T) {
				g, tr, waits := newGateway([]error{httpStatus(404)})
				if _, err := g.Apply(t.Context(), releaseRequest()); err == nil || tr.reads != 1 || len(*waits) != 0 || len(tr.bodies) != 1 {
					t.Fatalf("reads=%d waits=%v err=%v", tr.reads, *waits, err)
				}
			})
			t.Run(name+"/timeout retried", func(t *testing.T) {
				g, tr, _ := newGateway([]error{&url.Error{Op: "Get", URL: "u", Err: timeoutError{}}})
				if _, err := g.Apply(t.Context(), releaseRequest()); err != nil || tr.reads != 2+pollReads {
					t.Fatalf("reads=%d err=%v", tr.reads, err)
				}
			})
		}
	}
	t.Run("cancel during wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		tr := &commentTracker{readErrs: []error{httpStatus(502), httpStatus(502)}}
		g := &Gateway{Tracker: tr, Store: releasedAt(100)}
		g.wait = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
		_, err := g.Apply(ctx, releaseRequest())
		if !errors.Is(err, context.Canceled) || tr.reads != 1 {
			t.Fatalf("reads=%d err=%v", tr.reads, err)
		}
	})
	t.Run("deadline already expired", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		tr := &commentTracker{}
		g := &Gateway{Tracker: tr, Store: releasedAt(100)}
		comments, err := g.listComments(ctx, 1, time.Time{})
		if comments != nil || !errors.Is(err, context.Canceled) || tr.reads != 0 {
			t.Fatalf("reads=%d err=%v", tr.reads, err)
		}
	})
	t.Run("real wait honors deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		g := &Gateway{}
		start := time.Now()
		if err := g.sleep(ctx, time.Minute); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
			t.Fatalf("sleep err=%v", err)
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
	parseErr := `invalid fork request JSON: json: unknown field "request_nonce"`
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
