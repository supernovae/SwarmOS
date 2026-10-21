package fork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

type fakeGitHubState struct {
	mu          sync.Mutex
	currentUser string
	nextID      int64
	nextPR      int
	issues      map[int]*tracker.Issue
	comments    map[int64]*tracker.Comment
	issueComm   map[int][]int64
	prs         map[int]*tracker.PullRequest
	forks       map[string]map[string]any
}

func newFakeGitHubState() *fakeGitHubState {
	return &fakeGitHubState{
		currentUser: "maintainer",
		nextID:      500,
		nextPR:      10,
		issues:      make(map[int]*tracker.Issue),
		comments:    make(map[int64]*tracker.Comment),
		issueComm:   make(map[int][]int64),
		prs:         make(map[int]*tracker.PullRequest),
		forks: map[string]map[string]any{
			"alice/swarm": {
				"full_name":   "alice/swarm",
				"name":        "swarm",
				"fork":        true,
				"owner":       map[string]any{"login": "alice"},
				"source":      map[string]any{"full_name": "octo/swarm"},
				"permissions": map[string]any{"push": true},
			},
			"bob/swarm": {
				"full_name":   "bob/swarm",
				"name":        "swarm",
				"fork":        true,
				"owner":       map[string]any{"login": "bob"},
				"source":      map[string]any{"full_name": "octo/swarm"},
				"permissions": map[string]any{"push": true},
			},
		},
	}
}

func (s *fakeGitHubState) trackerForUser(t *testing.T, login, userType string) *tracker.GitHubTracker {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		writeJSON := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			writeJSON(map[string]any{"login": login, "type": userType})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/collaborators/"):
			perm := "read"
			if login == "maintainer" {
				perm = "admin"
			}
			writeJSON(map[string]any{"permission": perm})

		case r.Method == http.MethodGet && (r.URL.Path == "/repos/alice/swarm" || r.URL.Path == "/repos/bob/swarm"):
			slug := strings.TrimPrefix(r.URL.Path, "/repos/")
			info, ok := s.forks[slug]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(info)

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/"):
			idStr := strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/")
			id, _ := strconv.ParseInt(idStr, 10, 64)
			c, ok := s.comments[id]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(map[string]any{
				"id":         c.ID,
				"body":       c.Body,
				"created_at": c.CreatedAt,
				"updated_at": c.UpdatedAt,
				"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
			})

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/"):
			idStr := strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/issues/comments/")
			id, _ := strconv.ParseInt(idStr, 10, 64)
			c, ok := s.comments[id]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			c.Body = payload.Body
			c.UpdatedAt = c.CreatedAt.Add(time.Second)
			writeJSON(map[string]any{
				"id":         c.ID,
				"body":       c.Body,
				"created_at": c.CreatedAt,
				"updated_at": c.UpdatedAt,
				"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
			})

		case strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/issues/"):
			rest := strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/issues/")
			parts := strings.Split(rest, "/")
			issueNum, _ := strconv.Atoi(parts[0])
			iss, ok := s.issues[issueNum]
			if !ok {
				http.Error(w, "issue not found", http.StatusNotFound)
				return
			}
			if len(parts) == 1 && r.Method == http.MethodGet {
				var labels []map[string]string
				for _, l := range iss.Labels {
					labels = append(labels, map[string]string{"name": l})
				}
				writeJSON(map[string]any{
					"id":     issueNum,
					"number": iss.Number,
					"title":  iss.Title,
					"body":   iss.Body,
					"state":  iss.State,
					"user":   map[string]any{"login": iss.Author},
					"labels": labels,
				})
				return
			}
			if len(parts) == 2 && parts[1] == "comments" {
				if r.Method == http.MethodGet {
					var list []map[string]any
					for _, cid := range s.issueComm[issueNum] {
						c := s.comments[cid]
						list = append(list, map[string]any{
							"id":         c.ID,
							"body":       c.Body,
							"created_at": c.CreatedAt,
							"updated_at": c.UpdatedAt,
							"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
						})
					}
					writeJSON(list)
					return
				}
				if r.Method == http.MethodPost {
					var payload struct {
						Body string `json:"body"`
					}
					_ = json.NewDecoder(r.Body).Decode(&payload)
					s.nextID++
					now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
					c := &tracker.Comment{
						ID:         s.nextID,
						IssueNum:   issueNum,
						Author:     login,
						AuthorType: userType,
						Body:       payload.Body,
						CreatedAt:  now,
						UpdatedAt:  now,
					}
					s.comments[c.ID] = c
					s.issueComm[issueNum] = append(s.issueComm[issueNum], c.ID)
					writeJSON(map[string]any{
						"id":         c.ID,
						"body":       c.Body,
						"created_at": c.CreatedAt,
						"updated_at": c.UpdatedAt,
						"user":       map[string]any{"login": c.Author, "type": c.AuthorType},
					})
					return
				}
			}
			if len(parts) == 2 && parts[1] == "labels" && r.Method == http.MethodPost {
				var payload struct {
					Labels []string `json:"labels"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				for _, lb := range payload.Labels {
					if !slices.Contains(iss.Labels, lb) {
						iss.Labels = append(iss.Labels, lb)
					}
				}
				writeJSON(map[string]any{})
				return
			}
			if len(parts) == 3 && parts[1] == "labels" && r.Method == http.MethodDelete {
				target := parts[2]
				var kept []string
				for _, lb := range iss.Labels {
					if lb != target {
						kept = append(kept, lb)
					}
				}
				iss.Labels = kept
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Error(w, "unhandled issue route", http.StatusNotFound)

		case strings.HasPrefix(r.URL.Path, "/repos/octo/swarm/pulls/"):
			prNum, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/octo/swarm/pulls/"))
			pr, ok := s.prs[prNum]
			if !ok {
				http.Error(w, "pr not found", http.StatusNotFound)
				return
			}
			writeJSON(map[string]any{
				"number":   pr.Number,
				"title":    pr.Title,
				"body":     pr.Body,
				"state":    "open",
				"draft":    pr.Draft,
				"html_url": pr.HTMLURL,
				"head": map[string]any{
					"ref":  pr.Head,
					"sha":  pr.HeadSHA,
					"repo": map[string]any{"full_name": pr.HeadRepo},
				},
				"base": map[string]any{
					"ref":  pr.Base,
					"repo": map[string]any{"full_name": pr.BaseRepo},
				},
			})

		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.NewRESTClient(api.ClientOptions{
		Host:      "github.com",
		AuthToken: "test-token",
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			cloned := req.Clone(req.Context())
			cloned.URL.Scheme = srvURL.Scheme
			cloned.URL.Host = srvURL.Host
			cloned.Host = srvURL.Host
			return http.DefaultTransport.RoundTrip(cloned)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tracker.NewGitHubTrackerWithClient("octo", "swarm", client)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestForkCoordinatorSecurityAndLifecycle(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	originBare := filepath.Join(tmp, "origin.git")
	aliceForkBare := filepath.Join(tmp, "alice", "swarm.git")
	bobForkBare := filepath.Join(tmp, "bob", "swarm.git")
	git(tmp, "init", "--bare", "--initial-branch=main", originBare)

	upstreamCheckout := filepath.Join(tmp, "upstream")
	git(tmp, "clone", originBare, upstreamCheckout)
	git(upstreamCheckout, "config", "user.name", "Maintainer")
	git(upstreamCheckout, "config", "user.email", "maintainer@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: upstreamCheckout}); err != nil {
		t.Fatal(err)
	}
	git(upstreamCheckout, "add", ".")
	git(upstreamCheckout, "commit", "-m", "initial approved policy")
	git(upstreamCheckout, "push", "origin", "main")
	git(originBare, "symbolic-ref", "HEAD", "refs/heads/main")
	baseSHA := git(upstreamCheckout, "rev-parse", "HEAD")

	// Create bare forks for alice and bob
	if err := os.MkdirAll(filepath.Dir(aliceForkBare), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bobForkBare), 0o755); err != nil {
		t.Fatal(err)
	}
	git(tmp, "clone", "--bare", originBare, aliceForkBare)
	git(tmp, "clone", "--bare", originBare, bobForkBare)

	ghState := newFakeGitHubState()
	wuBody := "```yaml swarm-workunit\nid: ISSUE-42\nobjective: Implement fork feature\nconstraints: [Follow policy]\nacceptance: [Tests pass]\nworkflow: feature\ngate: agent\n```"
	ghState.issues[42] = &tracker.Issue{Number: 42, Title: "Fork feature 42", Body: wuBody, State: "open", Author: "maintainer"}
	ghState.issues[43] = &tracker.Issue{Number: 43, Title: "Fork feature 43", Body: strings.ReplaceAll(wuBody, "ISSUE-42", "ISSUE-43"), State: "open", Author: "maintainer"}

	maintTracker := ghState.trackerForUser(t, "maintainer", "User")
	cfg, err := harness.LoadConfigAt(upstreamCheckout, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	maintMgr := lease.NewManager(maintTracker, cfg.Security)
	if err := maintMgr.ConfigureStore(upstreamCheckout); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	maintMgr.SetNowFunc(func() time.Time { return now })
	if _, err := maintMgr.ApproveTask(ctx, 42, "maintainer"); err != nil {
		t.Fatalf("ApproveTask(42): %v", err)
	}
	if _, err := maintMgr.ApproveTask(ctx, 43, "maintainer"); err != nil {
		t.Fatalf("ApproveTask(43): %v", err)
	}

	botTracker := ghState.trackerForUser(t, ActionsBotLogin, "Bot")
	coord := &Coordinator{
		RepoRoot:     upstreamCheckout,
		UpstreamRepo: "octo/swarm",
		Tracker:      botTracker,
		NowFn:        func() time.Time { return now },
		ForkGitURLFunc: func(forkRepo string) string {
			switch forkRepo {
			case "alice/swarm":
				return aliceForkBare
			case "bob/swarm":
				return bobForkBare
			default:
				return filepath.Join(tmp, "missing.git")
			}
		},
	}

	makeEvent := func(issueNum int, c *tracker.Comment) IssueCommentEvent {
		var ev IssueCommentEvent
		ev.Action = "created"
		ev.Repository.FullName = "octo/swarm"
		ev.Issue.Number = issueNum
		ev.Issue.State = "open"
		ev.Comment.ID = c.ID
		ev.Comment.Body = c.Body
		ev.Comment.CreatedAt = c.CreatedAt
		ev.Comment.UpdatedAt = c.UpdatedAt
		ev.Comment.User.Login = c.Author
		ev.Comment.User.Type = c.AuthorType
		return ev
	}

	aliceTracker := ghState.trackerForUser(t, "alice", "User")
	bobTracker := ghState.trackerForUser(t, "bob", "User")

	// 1. Rejects oversized request payload on client before posting
	if _, err := FormatRequestComment(lease.ForkRequest{
		Version:     1,
		Operation:   "update-artifact",
		IssueNumber: 42,
		ClaimID:     "c1",
		Artifact:    &contract.Artifact{Body: strings.Repeat("x", MaxForkCommentBytes+100)},
	}); err == nil {
		t.Fatal("expected oversized fork request payload to fail before posting")
	}

	// 2. Rejects edited comment body and unknown/forged JSON fields (owner/base_sha/policy_digest)
	forgedComment, err := aliceTracker.CreateComment(ctx, 42, RequestMarker+"\n"+`{"version":1,"operation":"acquire","issue_number":42,"fork_repo":"alice/swarm","owner":"maintainer","base_sha":"deadbeef"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.HandleEvent(ctx, makeEvent(42, forgedComment)); err == nil {
		t.Fatal("expected request smuggling owner/base_sha fields to be rejected")
	}

	editedComment, err := aliceTracker.CreateComment(ctx, 42, RequestMarker+"\n"+`{"version":1,"operation":"acquire","issue_number":42,"fork_repo":"alice/swarm"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = aliceTracker.UpdateComment(ctx, editedComment.ID, editedComment.Body)
	if _, err := coord.HandleEvent(ctx, makeEvent(42, editedComment)); err == nil {
		t.Fatal("expected edited request comment to be rejected")
	}

	// 3. Alice acquires #42 via Gateway + Coordinator
	aliceClone := filepath.Join(tmp, "alice-clone")
	git(tmp, "clone", originBare, aliceClone)
	git(aliceClone, "remote", "add", "fork", aliceForkBare)
	// Block upstream push from alice's clone to prove zero upstream ref push
	git(aliceClone, "remote", "set-url", "--push", "origin", filepath.Join(tmp, "denied-upstream.git"))

	aliceStore := coordination.NewGitStore(aliceClone)
	aliceGateway := &Gateway{
		Tracker:      aliceTracker,
		Store:        aliceStore,
		ForkRepo:     "alice/swarm",
		PollInterval: 5 * time.Millisecond,
		PollTimeout:  2 * time.Second,
		Dispatcher: func(ctx context.Context, issueNumber int, c *tracker.Comment) error {
			// Inject a forged non-bot result comment first to ensure Gateway ignores it
			_, _ = bobTracker.CreateComment(ctx, issueNumber, ResultMarker+"\n"+fmt.Sprintf(`{"version":1,"request_id":%d,"issue_number":%d,"operation":"acquire","success":false,"error":"spoofed by bob"}`, c.ID, issueNumber))
			_, err := coord.HandleEvent(ctx, makeEvent(issueNumber, c))
			return err
		},
	}

	st42, err := aliceGateway.Apply(ctx, lease.ForkRequest{
		Version:     1,
		Operation:   "acquire",
		IssueNumber: 42,
		ForkRepo:    "alice/swarm",
		Agent:       "claude",
		Phase:       "all",
		Duration:    "24h", // Should be capped at approved policy 4h
	})
	if err != nil {
		t.Fatalf("alice acquire #42 failed: %v", err)
	}
	if st42.Lease.Owner != "alice" || st42.ForkRepo != "alice/swarm" || st42.Lease.ExpiresAt.Sub(now) != 4*time.Hour {
		t.Fatalf("unexpected alice lease on #42: %+v (fork=%q)", st42.Lease, st42.ForkRepo)
	}
	aliceClaimID := st42.Lease.ClaimID

	// 4. Replay of the same acquire comment ID must fail with ErrReplay
	lastCommentID := st42.LastForkRequestID
	ghState.mu.Lock()
	origReqComment := ghState.comments[lastCommentID]
	ghState.mu.Unlock()
	if _, err := coord.HandleEvent(ctx, makeEvent(42, origReqComment)); !errors.Is(err, coordination.ErrReplay) {
		t.Fatalf("expected replay of comment %d to fail with ErrReplay, got %v", lastCommentID, err)
	}

	// 5. Per-contributor cap (max_active_claims=1): alice cannot claim #43 while #42 is active
	if _, err := aliceGateway.Apply(ctx, lease.ForkRequest{
		Version:     1,
		Operation:   "acquire",
		IssueNumber: 43,
		ForkRepo:    "alice/swarm",
		Agent:       "claude",
		Phase:       "all",
	}); !errors.Is(err, coordination.ErrForkClaimLimit) {
		t.Fatalf("expected ErrForkClaimLimit when alice claims #43 while #42 is active, got %v", err)
	}

	// 6. Rejects human review artifact via update-artifact
	if _, err := aliceGateway.Apply(ctx, lease.ForkRequest{
		Version:     1,
		Operation:   "update-artifact",
		IssueNumber: 42,
		ClaimID:     aliceClaimID,
		Artifact: &contract.Artifact{
			Type:     contract.ArtifactReview,
			Producer: "human:alice",
			Verdict:  "APPROVED",
			Summary:  "spoofed human review",
			Body:     "VERDICT: APPROVED\n",
			Metadata: map[string]string{"kind": "human", "actor": "alice"},
		},
	}); err == nil {
		t.Fatal("expected human review artifact from fork contributor to be rejected")
	}

	// 7. Alice records design, test-plan, agent review, implementation, and checkpoint on #42
	for _, art := range []contract.Artifact{
		{Type: contract.ArtifactDesign, Producer: "claude:planner", Summary: "Plan", Body: "# Plan\n"},
		{Type: contract.ArtifactTestPlan, Producer: "claude:planner", Summary: "Test Plan", Body: "# Test Plan\n"},
		{Type: contract.ArtifactReview, Producer: "claude:reviewer", Verdict: "APPROVED", Summary: "Approved", Body: "VERDICT: APPROVED\n", Metadata: map[string]string{"kind": "agent", "actor": "alice"}},
		{Type: contract.ArtifactImplementation, Producer: "claude:developer", Verdict: "PASS", Summary: "Impl", Body: "# Impl\n"},
	} {
		artCopy := art
		if _, err := aliceGateway.Apply(ctx, lease.ForkRequest{
			Version:     1,
			Operation:   "update-artifact",
			IssueNumber: 42,
			ClaimID:     aliceClaimID,
			Artifact:    &artCopy,
		}); err != nil {
			t.Fatalf("alice update-artifact %s failed: %v", art.Type, err)
		}
	}

	aliceBranch := fmt.Sprintf("swarm/issue-42-%s", aliceClaimID)
	git(aliceClone, "checkout", "-b", aliceBranch, baseSHA)
	if err := os.WriteFile(filepath.Join(aliceClone, "feature.txt"), []byte("alice work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(aliceClone, "config", "user.name", "Alice")
	git(aliceClone, "config", "user.email", "alice@example.invalid")
	git(aliceClone, "add", "feature.txt")
	git(aliceClone, "commit", "-m", "alice checkpoint")
	aliceHead := git(aliceClone, "rev-parse", "HEAD")

	// Partial failure: recording checkpoint before pushing branch to fork fails closed
	if _, err := aliceGateway.Apply(ctx, lease.ForkRequest{
		Version:     1,
		Operation:   "record-checkpoint",
		IssueNumber: 42,
		ClaimID:     aliceClaimID,
		Branch:      aliceBranch,
		HeadSHA:     aliceHead,
	}); err == nil {
		t.Fatal("expected record-checkpoint before fork branch push to fail")
	}

	// Push to alice's fork remote and retry record-checkpoint
	git(aliceClone, "push", "fork", aliceHead+":refs/heads/"+aliceBranch)
	st42, err = aliceGateway.Apply(ctx, lease.ForkRequest{
		Version:     1,
		Operation:   "record-checkpoint",
		IssueNumber: 42,
		ClaimID:     aliceClaimID,
		Branch:      aliceBranch,
		HeadSHA:     aliceHead,
	})
	if err != nil {
		t.Fatalf("alice record-checkpoint after fork push failed: %v", err)
	}
	if st42.Branch != aliceBranch || st42.HeadSHA != aliceHead {
		t.Fatalf("unexpected checkpoint state: branch=%q head=%q", st42.Branch, st42.HeadSHA)
	}

	// 8. Expiry and takeover by bob: clears alice's Branch/HeadSHA and implementation artifact while keeping plan/test/review
	now = now.Add(5 * time.Hour)
	bobGateway := &Gateway{
		Tracker:      bobTracker,
		Store:        aliceStore,
		ForkRepo:     "bob/swarm",
		PollInterval: 5 * time.Millisecond,
		PollTimeout:  2 * time.Second,
		Dispatcher: func(ctx context.Context, issueNumber int, c *tracker.Comment) error {
			_, err := coord.HandleEvent(ctx, makeEvent(issueNumber, c))
			return err
		},
	}
	stBob, err := bobGateway.Apply(ctx, lease.ForkRequest{
		Version:     1,
		Operation:   "acquire",
		IssueNumber: 42,
		ForkRepo:    "bob/swarm",
		Agent:       "claude",
		Phase:       "implement",
	})
	if err != nil {
		t.Fatalf("bob takeover acquire #42 failed: %v", err)
	}
	if stBob.Lease.Owner != "bob" || stBob.Lease.ClaimID == aliceClaimID || stBob.ForkRepo != "bob/swarm" {
		t.Fatalf("bob takeover state invalid: %+v", stBob)
	}
	if stBob.Branch != "" || stBob.HeadSHA != "" {
		t.Fatalf("takeover must clear prior fork branch/head, got branch=%q head=%q", stBob.Branch, stBob.HeadSHA)
	}
	for _, a := range stBob.Artifacts {
		if a.Type == contract.ArtifactImplementation || a.Type == contract.ArtifactValidation {
			t.Fatalf("takeover must clear head-bound %s artifact", a.Type)
		}
	}
	if len(stBob.Artifacts) != 3 {
		t.Fatalf("takeover must retain design/test-plan/review artifacts, got %d", len(stBob.Artifacts))
	}

	// Alice's stale claim token is now fenced on renew, checkpoint, and submission
	if _, err := aliceGateway.Apply(ctx, lease.ForkRequest{
		Version:     1,
		Operation:   "renew",
		IssueNumber: 42,
		ClaimID:     aliceClaimID,
		Duration:    "1h",
	}); !errors.Is(err, lease.ErrClaimFence) {
		t.Fatalf("expected stale alice renew to fail with ErrClaimFence, got %v", err)
	}

	// Bob checkpoints to bob/swarm and submits a verified Draft PR
	bobBranch := fmt.Sprintf("swarm/issue-42-%s", stBob.Lease.ClaimID)
	git(aliceClone, "remote", "add", "bob-fork", bobForkBare)
	git(aliceClone, "push", "bob-fork", aliceHead+":refs/heads/"+bobBranch)
	if _, err := bobGateway.Apply(ctx, lease.ForkRequest{
		Version:     1,
		Operation:   "record-checkpoint",
		IssueNumber: 42,
		ClaimID:     stBob.Lease.ClaimID,
		Branch:      bobBranch,
		HeadSHA:     aliceHead,
	}); err != nil {
		t.Fatalf("bob record-checkpoint failed: %v", err)
	}

	ghState.mu.Lock()
	ghState.prs[55] = &tracker.PullRequest{
		Number:   55,
		Title:    "Fork feature 42 (#42)",
		Head:     bobBranch,
		HeadRepo: "bob/swarm",
		HeadSHA:  aliceHead,
		Base:     "main",
		BaseRepo: "octo/swarm",
		Draft:    true,
		HTMLURL:  "https://github.com/octo/swarm/pull/55",
	}
	ghState.mu.Unlock()

	stSubmitted, err := bobGateway.Apply(ctx, lease.ForkRequest{
		Version:     1,
		Operation:   "record-submission",
		IssueNumber: 42,
		ClaimID:     stBob.Lease.ClaimID,
		PRNumber:    55,
	})
	if err != nil {
		t.Fatalf("bob record-submission failed: %v", err)
	}
	if stSubmitted.Lease.Status != "completed" || stSubmitted.PRNumber != 55 || stSubmitted.PRURL != "https://github.com/octo/swarm/pull/55" {
		t.Fatalf("unexpected submitted state: %+v", stSubmitted)
	}
}
