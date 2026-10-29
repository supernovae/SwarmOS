package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	f := newCLIFixture(t)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'candidate\n' > feature.txt")
	issue := f.create(t, "agent")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
	requests := 0
	client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "fixture", Transport: descriptionRoundTrip(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		if r.Method == "POST" || r.Method == "PATCH" {
			var req struct{ Body string }
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(req.Body) || len(req.Body) >= 60000 {
				rec.WriteHeader(422)
				_, _ = rec.Write([]byte(`{"message":"body too long"}`))
			} else {
				requests++
				_ = json.NewEncoder(rec).Encode(map[string]any{"number": 1, "body": req.Body, "html_url": "https://example.invalid/pull/1"})
			}
		} else {
			t.Fatalf("unexpected API request: %s %s", r.Method, r.URL)
		}
		return rec.Result(), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	tr := &strictDescriptionTracker{Tracker: f.tracker, github: tracker.NewGitHubTrackerWithClient("fixture", "repo", client)}
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
	human := "Human é🙂 prefix\n\n" + original.Body + "\n\nHuman footer\n\n"
	if _, err := tr.UpdateDraftPR(t.Context(), original.Number, original.Title, human); err != nil {
		t.Fatal(err)
	}
	st.Artifacts = []contract.Artifact{
		{Type: contract.ArtifactDesign, Verdict: "PASS", Body: strings.Repeat("计划🙂<!-- swarm:rebase:end -->\n", 10000)},
		{Type: contract.ArtifactValidation, Verdict: "FAIL", Body: "| Check | Command | Status | Duration |\n| --- | --- | --- | --- |\n| tests | go test | fail | 1s |\n\n### Output: tests\n" + strings.Repeat("huge log é\n", 10000)},
	}
	st.LastRebase = &lease.RebaseEvidence{Status: "completed", Verified: true, Plans: []lease.RebasePlanReport{{Commit: "head", Digest: "digest", Body: strings.Repeat("micro-plan 计划\n", 10000)}}}
	before, _ := json.Marshal(st)
	store := coordination.NewFileStore(f.root)
	_, version, err := store.Load(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwap(t.Context(), issue, version, before); err != nil {
		t.Fatal(err)
	}
	service := &prprojection.Service{Root: f.root, Tracker: tr, Manager: f.manager}
	for i := 0; i < 2; i++ {
		if _, err := service.Submit(t.Context(), original.Number); err != nil {
			t.Fatal(err)
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
	if !strings.Contains(result.Body, "tests: fail") || requests < 4 {
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
