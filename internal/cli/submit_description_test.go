package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"

	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	prprojection "github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/tracker"
)

type descriptionRoundTrip func(*http.Request) (*http.Response, error)

func (f descriptionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type strictDescriptionTracker struct {
	tracker.Tracker
	github *tracker.GitHubTracker
}

func (s *strictDescriptionTracker) CreateDraftPR(ctx context.Context, r tracker.DraftPRRequest) (*tracker.PullRequest, error) {
	if _, err := s.github.CreateDraftPR(ctx, r); err != nil {
		return nil, err
	}
	return s.Tracker.CreateDraftPR(ctx, r)
}
func (s *strictDescriptionTracker) UpdateDraftPR(ctx context.Context, n int, title, body string) (*tracker.PullRequest, error) {
	if _, err := s.github.UpdateDraftPR(ctx, n, title, body); err != nil {
		return nil, err
	}
	return s.Tracker.UpdateDraftPR(ctx, n, title, body)
}

func TestSubmitDescriptionStrictComposedAPI(t *testing.T) {
	t.Setenv("SWARM_MOCK_PLAN_APPEND", strings.Repeat("oversized plan 计划🙂\n", 3000))
	f := newCLIFixture(t)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'candidate\n' > feature.txt")
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
	// Make the issue-generated source exceed the API limit before actual submit.
	store := coordination.NewFileStore(f.root)
	save := func(st *lease.IssueState) {
		t.Helper()
		raw, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		_, version, err := store.Load(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CompareAndSwap(t.Context(), issue, version, raw); err != nil {
			t.Fatal(err)
		}
	}
	seeded := f.state(t, issue)
	for i := range seeded.Artifacts {
		if seeded.Artifacts[i].Type == contract.ArtifactImplementation {
			seeded.Artifacts[i].Body += strings.Repeat("oversized issue evidence 计划🙂\n", 10000)
		}
	}
	save(seeded)
	humanExpected := false
	betweenExpected := false
	between := "\nHuman inter-section 中文 <!-- ordinary marker -->\n\n"
	betweenRebase := "\nHuman review/rebase é🙂\n\n"
	requests := 0
	rejected := 0
	var bodies []string
	client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "fixture", Transport: descriptionRoundTrip(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		if r.Method == "POST" || r.Method == "PATCH" {
			var req struct{ Body string }
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(req.Body) || len(req.Body) >= 60000 {
				rejected++
				rec.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = rec.Write([]byte(`{"message":"body too long"}`))
			} else {
				requests++
				if r.Method == "POST" && (!strings.Contains(req.Body, "oversized plan") || !strings.Contains(req.Body, "Inline details omitted")) {
					t.Fatal("POST did not project oversized issue evidence")
				}
				if humanExpected && (!strings.HasPrefix(req.Body, "Human é🙂 prefix\n\n") || !strings.Contains(req.Body, "\n\nHuman footer\n\n")) {
					t.Fatal("request changed human spans")
				}
				if betweenExpected && (!strings.Contains(req.Body, between) || !strings.Contains(req.Body, betweenRebase)) {
					t.Fatal("request changed inter-section bytes")
				}
				bodies = append(bodies, req.Body)
				for _, name := range []string{"issue-description", "review-evidence", "rebase"} {
					start := strings.Count(req.Body, "<!-- swarm:"+name+":start -->")
					end := strings.Count(req.Body, "<!-- swarm:"+name+":end -->")
					if start != end || start > 1 {
						t.Fatalf("invalid envelope %s: %d/%d", name, start, end)
					}
				}
				if !strings.Contains(req.Body, "<!-- swarm:issue-description:start -->") {
					t.Fatal("missing issue envelope")
				}
				_ = json.NewEncoder(rec).Encode(map[string]any{"number": 1, "body": req.Body, "html_url": "https://example.invalid/pull/1"})
			}
		} else {
			t.Fatalf("unexpected API request: %s %s", r.Method, r.URL)
		}
		response := rec.Result()
		response.Request = r
		return response, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	tr := &strictDescriptionTracker{Tracker: f.tracker, github: tracker.NewGitHubTrackerWithClient("fixture", "repo", client)}
	_, negativeErr := tr.github.CreateDraftPR(t.Context(), tracker.DraftPRRequest{Body: strings.Repeat("x", 60000)})
	var apiErr *api.HTTPError
	if !errors.As(negativeErr, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity || rejected != 1 {
		t.Fatalf("negative control: %v, rejections %d", negativeErr, rejected)
	}
	cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: tr})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"issue", "submit", n})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err, out.String())
	}
	st := f.state(t, issue)
	original, err := tr.GetPullRequest(t.Context(), st.PRNumber)
	if err != nil {
		t.Fatal(err)
	}
	humanExpected = true
	t.Setenv("SWARM_MOCK_PLAN_APPEND", "")
	human := "Human é🙂 prefix\n\n" + original.Body + "\n\nHuman footer\n\n"
	if _, err := tr.UpdateDraftPR(t.Context(), original.Number, original.Title, human); err != nil {
		t.Fatal(err)
	}
	p := strconv.Itoa(original.Number)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'revision\\n' > revision.txt")
	worked := readPROutcome(t, f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false", "--json"))
	st = f.state(t, issue)
	if st.PRRevision == nil || st.PRRevision.RemoteHeadSHA == st.PRRevision.LocalHeadSHA || worked.HeadSHA == original.HeadSHA {
		t.Fatal("revision is not pending at a distinct head")
	}
	if err := f.manager.CheckClaim(t.Context(), issue, "other-worker"); !errors.Is(err, lease.ErrClaimFence) {
		t.Fatalf("claim guard: %v", err)
	}
	// Publish with the real local tracker's expected-head guard, then reconcile
	// through Service.Submit (the supported published-pending revision path).
	if err := f.tracker.PublishPRHead(t.Context(), original.Number, "stale-head", worked.HeadSHA); err == nil {
		t.Fatal("expected-head guard accepted stale head")
	}
	if err := f.tracker.PublishPRHead(t.Context(), original.Number, original.HeadSHA, worked.HeadSHA); err != nil {
		t.Fatal(err)
	}
	service := &prprojection.Service{Root: f.root, Tracker: tr, Manager: f.manager, LookupEnv: f.env.lookup()}
	if _, err := service.Submit(t.Context(), original.Number); err != nil {
		t.Fatal(err)
	}
	if done := f.state(t, issue); done.PRRevision != nil || done.HeadSHA != worked.HeadSHA {
		t.Fatal("active revision not completed")
	}
	freshnessWrite(t, f.root, "upstream.txt", "advance base\n")
	freshnessGit(t, f.root, "add", "upstream.txt")
	freshnessGit(t, f.root, "commit", "-m", "advance approved base")
	f.must(t, "issue", "approve", n)
	rebased := readPROutcome(t, f.must(t, "pr", "rebase", p, "--json"))
	if rebased.HeadSHA == worked.HeadSHA || rebased.State.LastRebase == nil {
		t.Fatal("real rebase did not run")
	}
	st = f.state(t, issue)
	// Retain actual rebase evidence, enlarging only its optional micro-plan.
	st.LastRebase.Plans = []lease.RebasePlanReport{{Commit: rebased.HeadSHA, Digest: "digest", Body: strings.Repeat("micro-plan 计划\n", 10000)}}
	save(st)
	if err := f.tracker.PublishPRHead(t.Context(), original.Number, worked.HeadSHA, rebased.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Submit(t.Context(), original.Number); err != nil {
		t.Fatal(err)
	}
	st = f.state(t, issue)
	st.Artifacts = []contract.Artifact{
		{Type: contract.ArtifactDesign, Verdict: "PASS", Body: strings.Repeat("计划🙂<!-- swarm:rebase:end -->\n", 10000)},
		{Type: contract.ArtifactValidation, Verdict: "FAIL", Body: "| Check | Command | Status | Duration |\n| --- | --- | --- | --- |\n| tests | go test | fail | 1s |\n\n### Output: tests\n" + strings.Repeat("huge log é\n", 10000)},
	}
	save(st)
	current, err := tr.GetPullRequest(t.Context(), original.Number)
	if err != nil {
		t.Fatal(err)
	}
	betweenExpected = true
	current.Body = strings.Replace(current.Body, "<!-- swarm:review-evidence:start -->", between+"<!-- swarm:review-evidence:start -->", 1)
	current.Body = strings.Replace(current.Body, "<!-- swarm:rebase:start -->", betweenRebase+"<!-- swarm:rebase:start -->", 1)
	if _, err := tr.UpdateDraftPR(t.Context(), original.Number, current.Title, current.Body); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(st)
	var stable string
	for i := 0; i < 3; i++ {
		if _, err := service.Submit(t.Context(), original.Number); err != nil {
			t.Fatal(err)
		}
		got, _ := tr.GetPullRequest(t.Context(), original.Number)
		if i > 0 && got.Body != stable {
			t.Fatal("repeated projection changed bytes")
		}
		stable = got.Body
		if !strings.Contains(stable, between) {
			t.Fatal("inter-section bytes changed")
		}
	}
	after, _ := json.Marshal(f.state(t, issue))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("projection mutated durable evidence")
	}

	result, _ := tr.GetPullRequest(t.Context(), original.Number)
	if !strings.HasPrefix(result.Body, "Human é🙂 prefix\n\n") || !strings.Contains(result.Body, "\n\nHuman footer\n\n") {
		t.Fatal("outside bytes changed")
	}

	for _, name := range []string{"issue-description", "review-evidence", "rebase"} {
		if strings.Count(result.Body, "<!-- swarm:"+name+":start -->") != 1 {
			t.Fatal("section missing or duplicated", name)
		}
	}
	if rejected != 1 {
		t.Fatal("unexpected lifecycle API rejection", rejected)
	}
	if !strings.Contains(result.Body, "tests: fail") || requests < 8 || len(bodies) != requests {
		t.Fatal("outcomes or requests missing", requests)
	}
}

func TestFormatDraftDescriptionOversized(t *testing.T) {
	unit := &contract.WorkUnit{IssueNumber: 303, Objective: strings.Repeat("目的🙂", 20000), Constraints: []string{strings.Repeat("constraint ", 10000)}, Acceptance: []string{strings.Repeat("criterion ", 10000)}}
	st := &lease.IssueState{}
	for _, typ := range []contract.ArtifactType{contract.ArtifactDesign, contract.ArtifactTestPlan, contract.ArtifactReview, contract.ArtifactImplementation, contract.ArtifactValidation} {
		st.Artifacts = append(st.Artifacts, contract.Artifact{Type: typ, Verdict: "PASS", Summary: strings.Repeat("summary ", 10000), Body: strings.Repeat("日志🙂<!-- swarm:rebase:start -->", 10000), Metadata: map[string]string{"content_sha256": "full-digest"}})
	}
	body := formatDraftPRBody(unit, st, "refs/swarm/state/issue-303")
	if len(body) == 0 || len(body) > lease.PRDescriptionLimit || !utf8.ValidString(body) || !strings.Contains(body, "Closes #303") {
		t.Fatal("invalid bounded description", len(body))
	}
}

type descriptionFailureTracker struct {
	tracker.Tracker
	mode             string
	failed           bool
	creates, updates int
}

func (s *descriptionFailureTracker) CreateDraftPR(ctx context.Context, r tracker.DraftPRRequest) (*tracker.PullRequest, error) {
	s.creates++
	pr, err := s.Tracker.CreateDraftPR(ctx, r)
	if err == nil && s.mode == "lost-create" && !s.failed {
		s.failed = true
		return nil, fmt.Errorf("lost create response")
	}
	return pr, err
}
func (s *descriptionFailureTracker) UpdateDraftPR(ctx context.Context, n int, title, body string) (*tracker.PullRequest, error) {
	s.updates++
	if !s.failed {
		s.failed = true
		if s.mode == "nil-update" {
			return nil, nil
		}
		if s.mode == "failed-update" {
			return nil, fmt.Errorf("injected update failure")
		}
	}
	return s.Tracker.UpdateDraftPR(ctx, n, title, body)
}
func TestIssueDescriptionPublicationRetry(t *testing.T) {
	for _, mode := range []string{"lost-create", "nil-update", "failed-update", "outside-excess"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIFixture(t)
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'candidate\n' > feature.txt")
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
			st := f.state(t, issue)
			var existing *tracker.PullRequest
			if mode != "lost-create" {
				prose := "maintainer prose é\n\n"
				if mode == "outside-excess" {
					prose = strings.Repeat("x", 60000)
				}
				var err error
				existing, err = f.tracker.CreateDraftPR(t.Context(), tracker.DraftPRRequest{IssueNumber: issue, Title: "Existing", Head: st.Branch, Base: "main", Body: prose})
				if err != nil {
					t.Fatal(err)
				}
			}
			tr := &descriptionFailureTracker{Tracker: f.tracker, mode: mode}
			run := func() error {
				cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: tr})
				var output bytes.Buffer
				cmd.SetOut(&output)
				cmd.SetErr(&output)
				cmd.SetArgs([]string{"issue", "submit", n, "--json"})
				err := cmd.ExecuteContext(t.Context())
				if err != nil && strings.Contains(output.String(), `"status":"submitted"`) {
					t.Fatal("success output after failed publication")
				}
				return err
			}
			if err := run(); err == nil {
				t.Fatal("injected publication failure was ignored")
			}
			failed := f.state(t, issue)
			if failed.Lease.Status == "completed" || failed.PRNumber != 0 {
				t.Fatal("failed description was recorded as submitted")
			}
			if mode == "outside-excess" {
				if tr.updates != 0 {
					t.Fatal("PATCH sent for irreducible body")
				}
				preserved, _ := tr.GetPullRequest(t.Context(), existing.Number)
				if preserved.Body != strings.Repeat("x", 60000) {
					t.Fatal("outside body changed")
				}
				return
			}
			if err := run(); err != nil {
				t.Fatal("retry failed", err)
			}
			done := f.state(t, issue)
			if done.Lease.Status != "completed" || done.PRNumber != 1 {
				t.Fatal("retry lost published PR identity", done)
			}
			if mode == "lost-create" && tr.creates != 1 {
				t.Fatal("retry created a second PR")
			}
			if existing != nil {
				updated, _ := tr.GetPullRequest(t.Context(), done.PRNumber)
				if !strings.HasPrefix(updated.Body, "maintainer prose é\n\n") {
					t.Fatal("retry overwrote maintainer text")
				}
			}
		})
	}
}
