package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/fork"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

// upstreamRemoteFixture is a GitHub project octo/swarm with personal forks,
// served by a fake GitHub API and the real fork coordinator.
type upstreamRemoteFixture struct {
	t            *testing.T
	tmp          string
	projectBare  string
	aliceBare    string
	upstreamRoot string
	nodeConfig   string
	env          *fixtureEnv
	baseSHA      string
	forkMainSHA  string

	mu       sync.Mutex
	now      time.Time
	comments map[int64]*tracker.Comment
	prs      map[int]*tracker.PullRequest
	issues   map[int]*tracker.Issue
	posted   int

	maintainer tracker.Tracker
	coord      *fork.Coordinator
}

func (f *upstreamRemoteFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newUpstreamRemoteFixture(t *testing.T) *upstreamRemoteFixture {
	t.Helper()
	f := &upstreamRemoteFixture{
		t: t, tmp: t.TempDir(), env: newFixtureEnv(t),
		now:      time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC),
		comments: map[int64]*tracker.Comment{},
		prs:      map[int]*tracker.PullRequest{},
	}
	f.nodeConfig = filepath.Join(t.TempDir(), "node.yaml")
	f.env.set("SWARM_NODE_CONFIG", f.nodeConfig)
	f.projectBare = filepath.Join(f.tmp, "octo", "swarm.git")
	f.aliceBare = filepath.Join(f.tmp, "alice", "swarm.git")
	for _, dir := range []string{f.projectBare, f.aliceBare} {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.git(f.tmp, "init", "--bare", "--initial-branch=main", f.projectBare)
	f.upstreamRoot = filepath.Join(f.tmp, "maintainer")
	f.git(f.tmp, "clone", f.projectBare, f.upstreamRoot)
	f.git(f.upstreamRoot, "config", "user.name", "Maintainer")
	f.git(f.upstreamRoot, "config", "user.email", "maintainer@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: f.upstreamRoot, SwarmVersion: "a1b2c3d4e5f60123456789abcdef0123456789ab", ForkCoordinator: true}); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(f.upstreamRoot, ".swarm", "config.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(cfg, []byte("\n  mock:\n    binary: git\n    harness: mock\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := "required_checks:\n  - name: git-available\n    command: git --version\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(f.upstreamRoot, ".swarm", "validation", "required-checks.yaml"), []byte(checks), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(f.upstreamRoot, "add", ".")
	f.git(f.upstreamRoot, "commit", "-m", "approved upstream baseline")
	f.git(f.upstreamRoot, "push", "origin", "main")
	f.git(f.projectBare, "symbolic-ref", "HEAD", "refs/heads/main")
	f.baseSHA = f.git(f.upstreamRoot, "rev-parse", "HEAD")

	// The fork advertises a different default branch carrying a fork-only
	// commit, so a base taken from origin would be detectably wrong.
	f.git(f.tmp, "clone", "--bare", f.projectBare, f.aliceBare)
	scratch := filepath.Join(f.tmp, "fork-scratch")
	f.git(f.tmp, "clone", f.aliceBare, scratch)
	f.git(scratch, "config", "user.name", "Alice")
	f.git(scratch, "config", "user.email", "alice@example.invalid")
	f.git(scratch, "switch", "-c", "fork-default")
	if err := os.WriteFile(filepath.Join(scratch, "fork-only.txt"), []byte("fork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(scratch, "add", "fork-only.txt")
	f.git(scratch, "commit", "-m", "fork-only default branch")
	f.git(scratch, "push", "origin", "fork-default")
	f.git(f.aliceBare, "symbolic-ref", "HEAD", "refs/heads/fork-default")
	f.forkMainSHA = f.git(scratch, "rev-parse", "HEAD")

	f.maintainer = f.githubTracker("octo/swarm", "maintainer", "User")
	bot := f.githubTracker("octo/swarm", fork.ActionsBotLogin, "Bot")
	f.coord = &fork.Coordinator{
		RepoRoot: f.upstreamRoot, UpstreamRepo: "octo/swarm", Tracker: bot,
		NowFn: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
		ForkGitURLFunc: func(forkRepo string) string {
			if forkRepo == "alice/swarm" {
				return f.aliceBare
			}
			return filepath.Join(f.tmp, "missing.git")
		},
	}
	wuBody := "```yaml swarm-workunit\nid: ISSUE-42\nobjective: Exercise the origin=fork layout\nconstraints: [No upstream push access]\nacceptance: [Cross-repo Draft PR opened]\nworkflow: feature\ngate: agent\nallow_harness_changes: true\n```"
	f.issues = map[int]*tracker.Issue{42: {Number: 42, Title: "Upstream remote task", Body: wuBody, State: "open", Author: "maintainer"}}
	if out, err := f.run(RootOptions{RepoRoot: f.upstreamRoot, Backend: "github", trackerOverride: f.maintainer}, "issue", "approve", "42"); err != nil {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	return f
}

// aliceClone clones alice's fork as origin and adds the project as upstream
// with pushes denied, like gh repo clone <fork>.
func (f *upstreamRemoteFixture) aliceClone(name string) string {
	root := filepath.Join(f.tmp, "clones", name)
	f.git(f.tmp, "clone", f.aliceBare, root)
	f.git(root, "config", "user.name", "Alice")
	f.git(root, "config", "user.email", "alice@example.invalid")
	f.git(root, "remote", "add", "upstream", f.projectBare)
	f.git(root, "remote", "set-url", "--push", "upstream", filepath.Join(f.tmp, "no-upstream-push.git"))
	return root
}

// as runs commands as login with the real repository identity resolution.
func (f *upstreamRemoteFixture) as(login, root string) RootOptions {
	return RootOptions{
		RepoRoot: root, Backend: "github",
		githubTrackerFunc: func(slug string) (tracker.Tracker, error) {
			return f.githubTracker(slug, login, "User"), nil
		},
		remoteSlugFunc: func(root, remote string) string {
			slug, err := workspace.ResolveForkRemoteSlug(root, remote, remote)
			if err != nil {
				return ""
			}
			return slug
		},
	}
}

func (f *upstreamRemoteFixture) run(opts RootOptions, args ...string) (string, error) {
	opts.forkPollInterval = 5 * time.Millisecond
	opts.forkPollTimeout = 30 * time.Second
	opts.forkDispatcher = f.dispatch
	opts.nowFn = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
	opts.lookupEnv = f.env.lookup()
	cmd := newRootCmdWithOptions(&opts)
	var out, diagnostics bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String() + diagnostics.String(), err
}

func (f *upstreamRemoteFixture) dispatch(ctx context.Context, issueNumber int, c *tracker.Comment) error {
	var ev fork.IssueCommentEvent
	ev.Action = "created"
	ev.Repository.FullName = "octo/swarm"
	ev.Issue.Number = issueNumber
	ev.Issue.State = "open"
	ev.Comment.ID = c.ID
	ev.Comment.Body = c.Body
	ev.Comment.CreatedAt = c.CreatedAt
	ev.Comment.UpdatedAt = c.UpdatedAt
	ev.Comment.User.Login = c.Author
	ev.Comment.User.Type = c.AuthorType
	_, err := f.coord.HandleEvent(ctx, ev)
	return err
}

func (f *upstreamRemoteFixture) state() *lease.IssueState {
	f.t.Helper()
	raw, _, err := coordination.NewGitStore(f.upstreamRoot).Load(context.Background(), 42)
	if err != nil {
		f.t.Fatal(err)
	}
	var st lease.IssueState
	if err := json.Unmarshal(raw, &st); err != nil {
		f.t.Fatal(err)
	}
	return &st
}

// refs snapshots every ref of the project and alice's fork.
func (f *upstreamRemoteFixture) refs() string {
	return f.git(f.projectBare, "for-each-ref") + "\n--\n" + f.git(f.aliceBare, "for-each-ref")
}

func (f *upstreamRemoteFixture) postedComments() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posted
}

// githubTracker serves slug's API as login. Forks are owned by their owner;
// carol's fork denies her push access.
func (f *upstreamRemoteFixture) githubTracker(slug, login, userType string) tracker.Tracker {
	prefix := "/repos/" + slug + "/"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		comment := func(c *tracker.Comment) map[string]any {
			return map[string]any{"id": c.ID, "body": c.Body, "created_at": c.CreatedAt, "updated_at": c.UpdatedAt, "user": map[string]any{"login": c.Author, "type": c.AuthorType}}
		}
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/user":
			reply(map[string]any{"login": login, "type": userType})
		case r.Method == http.MethodGet && strings.HasPrefix(path, prefix+"collaborators/"):
			perm := "read"
			if login == "maintainer" {
				perm = "admin"
			}
			reply(map[string]any{"permission": perm})
		case r.Method == http.MethodGet && strings.Count(path, "/") == 3 && strings.HasSuffix(path, "/swarm") && path != "/repos/octo/swarm":
			owner := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")[0]
			reply(map[string]any{"full_name": owner + "/swarm", "name": "swarm", "fork": true,
				"owner": map[string]any{"login": owner}, "source": map[string]any{"full_name": "octo/swarm"},
				"permissions": map[string]any{"push": login == owner && owner != "carol"}})
		case !strings.HasPrefix(path, prefix):
			http.Error(w, "unexpected "+r.Method+" "+path, http.StatusNotFound)
		case strings.HasPrefix(path, prefix+"issues/comments/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(path, prefix+"issues/comments/"), 10, 64)
			c, ok := f.comments[id]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			if r.Method == http.MethodPatch {
				var payload struct {
					Body string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				c.Body = payload.Body
			}
			reply(comment(c))
		case strings.HasPrefix(path, prefix+"issues/"):
			parts := strings.Split(strings.TrimPrefix(path, prefix+"issues/"), "/")
			n, _ := strconv.Atoi(parts[0])
			iss, ok := f.issues[n]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			switch {
			case len(parts) == 1 && r.Method == http.MethodGet:
				var labels []map[string]string
				for _, l := range iss.Labels {
					labels = append(labels, map[string]string{"name": l})
				}
				reply(map[string]any{"id": n, "number": iss.Number, "title": iss.Title, "body": iss.Body, "state": iss.State, "user": map[string]any{"login": iss.Author}, "labels": labels})
			case len(parts) == 2 && parts[1] == "comments" && r.Method == http.MethodGet:
				var list []map[string]any
				for id := int64(1); id <= int64(len(f.comments))+1000; id++ {
					if c, ok := f.comments[id]; ok && c.IssueNum == n {
						list = append(list, comment(c))
					}
				}
				reply(list)
			case len(parts) == 2 && parts[1] == "comments" && r.Method == http.MethodPost:
				var payload struct {
					Body string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				f.posted++
				c := &tracker.Comment{ID: int64(1000 + f.posted), IssueNum: n, Author: login, AuthorType: userType, Body: payload.Body, CreatedAt: f.now, UpdatedAt: f.now}
				f.comments[c.ID] = c
				reply(comment(c))
			case len(parts) >= 2 && parts[1] == "labels":
				if login != "maintainer" && login != fork.ActionsBotLogin {
					http.Error(w, "contributor cannot mutate upstream labels", http.StatusForbidden)
					return
				}
				if r.Method == http.MethodPost {
					var payload struct {
						Labels []string `json:"labels"`
					}
					_ = json.NewDecoder(r.Body).Decode(&payload)
					for _, l := range payload.Labels {
						if !slices.Contains(iss.Labels, l) {
							iss.Labels = append(iss.Labels, l)
						}
					}
					reply(map[string]any{})
					return
				}
				iss.Labels = slices.DeleteFunc(iss.Labels, func(l string) bool { return len(parts) == 3 && l == parts[2] })
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected "+r.Method+" "+path, http.StatusNotFound)
			}
		case path == prefix+"git/ref/heads/main":
			reply(map[string]any{"object": map[string]string{"sha": f.headSHA("main")}})
		case path == prefix+"pulls" && r.Method == http.MethodGet:
			var list []map[string]any
			for _, pr := range f.prs {
				if pr.Draft && strings.Split(pr.HeadRepo, "/")[0]+":"+pr.Head == r.URL.Query().Get("head") && pr.Base == r.URL.Query().Get("base") {
					list = append(list, map[string]any{"number": pr.Number, "title": pr.Title, "body": pr.Body, "draft": pr.Draft, "html_url": pr.HTMLURL})
				}
			}
			reply(list)
		case path == prefix+"pulls" && r.Method == http.MethodPost:
			var payload struct {
				Title, Body, Head, Base string
				Draft                   bool
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			headRepo, headBranch := "octo/swarm", payload.Head
			if owner, branch, ok := strings.Cut(payload.Head, ":"); ok {
				headRepo, headBranch = owner+"/swarm", branch
			}
			n := len(f.prs) + 1
			pr := &tracker.PullRequest{Number: n, Title: payload.Title, Body: payload.Body, Head: headBranch, HeadRepo: headRepo, HeadSHA: f.headSHA(payload.Head), Base: payload.Base, BaseRepo: "octo/swarm", Draft: payload.Draft, HTMLURL: fmt.Sprintf("https://github.com/octo/swarm/pull/%d", n)}
			f.prs[n] = pr
			reply(map[string]any{"number": n, "title": pr.Title, "body": pr.Body, "draft": pr.Draft, "html_url": pr.HTMLURL})
		case strings.HasPrefix(path, prefix+"pulls/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(path, prefix+"pulls/"))
			pr, ok := f.prs[n]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			if r.Method == http.MethodPatch {
				var payload struct{ Title, Body string }
				_ = json.NewDecoder(r.Body).Decode(&payload)
				pr.Title, pr.Body = payload.Title, payload.Body
			}
			reply(map[string]any{"number": pr.Number, "title": pr.Title, "body": pr.Body, "state": "open", "draft": pr.Draft, "html_url": pr.HTMLURL,
				"head": map[string]any{"ref": pr.Head, "sha": pr.HeadSHA, "repo": map[string]any{"full_name": pr.HeadRepo}},
				"base": map[string]any{"ref": pr.Base, "repo": map[string]any{"full_name": pr.BaseRepo}}})
		default:
			http.Error(w, "unexpected "+r.Method+" "+path, http.StatusNotFound)
		}
	})
	client, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "test-token",
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cloned := req.Clone(req.Context())
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, cloned)
			resp := rec.Result()
			resp.Request = cloned
			return resp, nil
		})})
	if err != nil {
		f.t.Fatal(err)
	}
	owner, repo, _ := strings.Cut(slug, "/")
	return tracker.NewGitHubTrackerWithClient(owner, repo, client)
}

// headSHA resolves main or owner:branch in the matching bare repository.
func (f *upstreamRemoteFixture) headSHA(head string) string {
	bare, branch := f.projectBare, head
	if owner, b, ok := strings.Cut(head, ":"); ok {
		branch = b
		bare = filepath.Join(f.tmp, owner, "swarm.git")
	}
	out, err := exec.Command("git", "-C", bare, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestUpstreamRemoteForkFlow covers origin = the contributor's fork and
// upstream = the project, selected first by flag and then by node profile,
// without --fork-remote.
func TestUpstreamRemoteForkFlow(t *testing.T) {
	t.Parallel()
	f := newUpstreamRemoteFixture(t)
	alice := f.aliceClone("alice")
	opts := f.as("alice", alice)

	// issue work claims through the coordinator, then stops at the plan gate.
	out, err := f.run(opts, "--upstream-remote", "upstream", "--fork", "issue", "work", "42", "--agent", "mock", "--headless")
	if !errors.Is(err, workflow.ErrPlanApprovalNeeded) {
		t.Fatalf("work should stop at the trusted plan gate, got %v\n%s", err, out)
	}
	claimed := f.state()
	branch := "swarm/issue-42-" + claimed.Lease.ClaimID
	if claimed.BaseSHA != f.baseSHA || claimed.Lease.Owner != "alice" || claimed.ForkRepo != "alice/swarm" || claimed.Lease.ClaimID == "" {
		t.Fatalf("claim state = %+v, want base %s from upstream (fork default is %s)", claimed, f.baseSHA, f.forkMainSHA)
	}
	if head := f.git(workspace.WorktreePath(alice, 42), "merge-base", "HEAD", f.baseSHA); head != f.baseSHA {
		t.Fatalf("worktree does not descend from the upstream base: %s", head)
	}
	if _, err := os.Stat(filepath.Join(workspace.WorktreePath(alice, 42), "fork-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("worktree was based on the fork's default branch: %v", err)
	}
	if out, err := f.run(RootOptions{RepoRoot: f.upstreamRoot, Backend: "github", trackerOverride: f.maintainer}, "issue", "review-plan", "42", "--approve", "--notes", "Reviewed"); err != nil {
		t.Fatalf("review-plan: %v\n%s", err, out)
	}

	// The node profile now selects upstream; only --fork is given.
	if err := os.WriteFile(f.nodeConfig, []byte("node_id: test-node\nupstream_remote: upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = f.run(opts, "--fork", "issue", "work", "42", "--agent", "mock", "--headless", "--phase", "implement")
	if err != nil {
		t.Fatalf("work --phase implement: %v\n%s", err, out)
	}
	worked := f.state()
	if worked.HeadSHA == "" || f.git(f.aliceBare, "rev-parse", "refs/heads/"+branch) != worked.HeadSHA {
		t.Fatalf("checkpoint %s not on the fork branch %s", worked.HeadSHA, branch)
	}
	if err := os.WriteFile(filepath.Join(workspace.WorktreePath(alice, 42), "feature.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = f.run(opts, "--fork", "issue", "submit", "42")
	if err != nil {
		t.Fatalf("submit: %v\n%s", err, out)
	}

	submitted := f.state()
	head := f.git(workspace.WorktreePath(alice, 42), "rev-parse", "HEAD")
	if submitted.HeadSHA != head || submitted.Branch != branch || submitted.PRNumber != 1 || submitted.Lease.Status != "completed" || submitted.BaseSHA != f.baseSHA {
		t.Fatalf("submitted state = %+v, want head %s", submitted, head)
	}
	f.mu.Lock()
	pr := f.prs[1]
	prCount := len(f.prs)
	f.mu.Unlock()
	if prCount != 1 || pr.Head != branch || pr.HeadRepo != "alice/swarm" || pr.HeadSHA != head || pr.Base != "main" || pr.BaseRepo != "octo/swarm" {
		t.Fatalf("PR = %+v (count %d)", pr, prCount)
	}
	if got := f.git(f.aliceBare, "rev-parse", "refs/heads/"+branch); got != head {
		t.Fatalf("fork branch is %s, want %s", got, head)
	}
	// Authoritative state and retention live upstream; the fork holds only
	// the task branch, and the project holds no task branch.
	if got := f.git(f.aliceBare, "for-each-ref", "refs/heads/swarm-state", "refs/heads/swarm-checkpoints", "refs/heads/swarm-contributors"); got != "" {
		t.Fatalf("coordination refs written to the fork:\n%s", got)
	}
	if got := f.git(f.projectBare, "for-each-ref", "--format=%(refname)", "refs/heads/swarm/"); got != "" {
		t.Fatalf("task branch written to the project:\n%s", got)
	}
	if f.git(f.projectBare, "rev-parse", "refs/heads/swarm-state/issue-42") == "" || f.git(f.projectBare, "rev-parse", coordination.CheckpointRef(42, head)) != head {
		t.Fatal("upstream state or checkpoint retention missing")
	}
	if f.git(f.projectBare, "rev-parse", "refs/heads/main") != f.baseSHA {
		t.Fatal("project main moved")
	}
	// Remotes are used as configured, never renamed or rewritten.
	if f.git(alice, "remote", "get-url", "origin") != f.aliceBare || f.git(alice, "remote", "get-url", "upstream") != f.projectBare || f.git(alice, "remote") != "origin\nupstream" {
		t.Fatalf("remotes changed: %s", f.git(alice, "remote", "-v"))
	}
}

// TestUpstreamRemoteValidation proves each misconfiguration names the remote
// and the fix before any claim, coordinator request, push or worktree.
func TestUpstreamRemoteValidation(t *testing.T) {
	t.Parallel()
	f := newUpstreamRemoteFixture(t)
	for _, tc := range []struct {
		name   string
		setup  func(root string)
		login  string
		args   []string
		expect []string
	}{
		{"upstream remote missing", func(root string) { f.git(root, "remote", "remove", "upstream") }, "alice",
			[]string{"--upstream-remote", "upstream"}, []string{`upstream remote "upstream" is not configured`, "git remote add upstream <project-url>", "--upstream-remote <project-remote>"}},
		{"upstream points at the fork with --repo", func(root string) { f.git(root, "remote", "set-url", "upstream", f.aliceBare) }, "alice",
			[]string{"--upstream-remote", "upstream", "--repo", "octo/swarm"}, []string{"--repo octo/swarm does not match upstream remote upstream (alice/swarm)", "--upstream-remote <project-remote>"}},
		{"upstream points at the fork", func(root string) { f.git(root, "remote", "set-url", "upstream", f.aliceBare) }, "alice",
			[]string{"--upstream-remote", "upstream"}, []string{`fork remote "origin"`, "same URL as upstream upstream", "--fork-remote <verified-personal-fork-remote>", "--upstream-remote <project-remote>"}},
		{"upstream is the fork under another URL spelling", func(root string) { f.git(root, "remote", "set-url", "upstream", f.aliceBare+"/") }, "alice",
			[]string{"--upstream-remote", "upstream"}, []string{`fork remote "origin" resolves to alice/swarm, the same repository as upstream remote "upstream"`, "--fork-remote <verified-personal-fork-remote>"}},
		{"upstream does not match --repo", func(string) {}, "alice",
			[]string{"--upstream-remote", "upstream", "--repo", "octo/other"}, []string{"--repo octo/other does not match upstream remote upstream (octo/swarm)", "point upstream at octo/other"}},
		{"origin missing with a fork remote present", func(root string) {
			f.git(root, "remote", "rename", "origin", "fork")
		}, "alice", []string{"--upstream-remote", "upstream"}, []string{`fork remote "origin" (used because the upstream remote is "upstream") failed verification`, `git remote "origin" is not configured`, "--fork-remote <verified-personal-fork-remote>"}},
		{"origin owned by someone else", func(string) {}, "bob",
			[]string{"--upstream-remote", "upstream"}, []string{`default fork remote "origin" (used because the upstream remote is "upstream") failed verification`, "--fork-remote <verified-personal-fork-remote>"}},
		{"origin without push permission", func(root string) {
			f.git(root, "remote", "set-url", "origin", filepath.Join(f.tmp, "carol", "swarm.git"))
		}, "carol",
			[]string{"--upstream-remote", "upstream"}, []string{`fork remote "origin" (used because the upstream remote is "upstream") failed verification`, "--fork-remote <verified-personal-fork-remote>"}},
		{"explicit fork remote is the upstream", func(string) {}, "alice",
			[]string{"--upstream-remote", "upstream", "--fork-remote", "upstream"}, []string{"--fork-remote upstream is also the upstream remote", "--fork-remote <verified-personal-fork-remote>"}},
		{"explicit origin fork remote while origin is upstream", func(string) {}, "alice",
			[]string{"--fork-remote", "origin"}, []string{"--fork-remote cannot be origin", "--upstream-remote <project-remote>"}},
		{"invalid upstream remote name", func(string) {}, "alice",
			[]string{"--upstream-remote", "-upstream"}, []string{`--upstream-remote: invalid git remote name "-upstream"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := f.aliceClone(strings.ReplaceAll(tc.name, " ", "-"))
			tc.setup(root)
			before, comments := f.refs(), f.postedComments()
			args := append(append(tc.args, "--fork"), "issue", "claim", "42", "--agent", "mock")
			out, err := f.run(f.as(tc.login, root), args...)
			if err == nil {
				t.Fatalf("claim succeeded:\n%s", out)
			}
			t.Log(err)
			for _, want := range tc.expect {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if f.refs() != before || f.postedComments() != comments {
				t.Fatal("rejected command wrote refs or posted a coordinator request")
			}
			if _, statErr := os.Stat(filepath.Join(root, ".swarm", "worktrees")); !os.IsNotExist(statErr) {
				t.Fatalf("rejected command created worktrees: %v", statErr)
			}
		})
	}
}

// TestUpstreamRemoteSameRepository directs same-repository coordination to a
// named upstream while a decoy origin stays untouched.
func TestUpstreamRemoteSameRepository(t *testing.T) {
	t.Parallel()
	f := newUpstreamRemoteFixture(t)
	decoy := filepath.Join(f.tmp, "decoy", "swarm.git")
	f.git(f.tmp, "clone", "--bare", f.projectBare, decoy)
	root := filepath.Join(f.tmp, "collaborator")
	f.git(f.tmp, "clone", decoy, root)
	f.git(root, "config", "user.name", "Maintainer")
	f.git(root, "config", "user.email", "maintainer@example.invalid")
	f.git(root, "remote", "add", "upstream", f.projectBare)
	decoyRefs := f.git(decoy, "for-each-ref")
	opts := f.as("maintainer", root)

	out, err := f.run(opts, "--upstream-remote", "upstream", "issue", "claim", "42", "--agent", "mock")
	if err != nil {
		t.Fatalf("claim: %v\n%s", err, out)
	}
	st := f.state()
	if st.Lease.Owner != "maintainer" || st.BaseSHA != f.baseSHA || st.ForkRepo != "" {
		t.Fatalf("state = %+v", st)
	}
	if out, err := f.run(opts, "--upstream-remote", "upstream", "issue", "status", "42"); err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if out, err := f.run(opts, "--repo", "octo/swarm", "issue", "status", "42"); err == nil || !strings.Contains(err.Error(), "--repo octo/swarm does not match origin decoy/swarm; if origin is your fork, pass --upstream-remote <project-remote>") {
		t.Fatalf("origin decoy accepted as the project: %v\n%s", err, out)
	}
	out, err = f.run(opts, "--upstream-remote", "upstream", "issue", "work", "42", "--agent", "mock", "--headless")
	if errors.Is(err, workflow.ErrPlanApprovalNeeded) {
		if out, err := f.run(opts, "--upstream-remote", "upstream", "issue", "review-plan", "42", "--approve", "--notes", "Reviewed"); err != nil {
			t.Fatalf("review-plan: %v\n%s", err, out)
		}
		out, err = f.run(opts, "--upstream-remote", "upstream", "issue", "work", "42", "--agent", "mock", "--headless", "--phase", "implement")
	}
	if err != nil {
		t.Fatalf("work: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(workspace.WorktreePath(root, 42), "feature.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run(opts, "--upstream-remote", "upstream", "issue", "submit", "42"); err != nil {
		t.Fatalf("submit: %v\n%s", err, out)
	}
	st = f.state()
	head := f.git(workspace.WorktreePath(root, 42), "rev-parse", "HEAD")
	if st.HeadSHA != head || st.PRNumber != 1 || f.git(f.projectBare, "rev-parse", "refs/heads/"+st.Branch) != head || f.git(f.projectBare, "rev-parse", coordination.CheckpointRef(42, head)) != head {
		t.Fatalf("checkpoint %s not published to the named upstream: %+v", head, st)
	}
	f.mu.Lock()
	pr := f.prs[1]
	f.mu.Unlock()
	if pr == nil || pr.HeadRepo != "octo/swarm" || pr.Head != st.Branch || pr.Base != "main" || pr.HeadSHA != head {
		t.Fatalf("PR = %+v", pr)
	}
	if got := f.git(decoy, "for-each-ref"); got != decoyRefs {
		t.Fatalf("decoy origin changed:\n%s", got)
	}
}

func TestForkRemoteDefaults(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		upstream, profile, forkRm string
		wantUpstream, wantFork    string
		wantErr                   string
	}{
		{name: "unset", wantUpstream: "origin", wantFork: "fork"},
		{name: "explicit origin upstream", upstream: "origin", wantUpstream: "origin", wantFork: "fork"},
		{name: "profile origin upstream", profile: "origin", wantUpstream: "origin", wantFork: "fork"},
		{name: "flag names upstream", upstream: "upstream", wantUpstream: "upstream", wantFork: "origin"},
		{name: "profile names upstream", profile: "upstream", wantUpstream: "upstream", wantFork: "origin"},
		{name: "flag origin overrides profile", upstream: "origin", profile: "upstream", wantUpstream: "origin", wantFork: "fork"},
		{name: "explicit fork wins over origin default", upstream: "upstream", forkRm: "fork", wantUpstream: "upstream", wantFork: "fork"},
		{name: "explicit other remote", profile: "upstream", forkRm: "mine", wantUpstream: "upstream", wantFork: "mine"},
		{name: "explicit origin with named upstream", upstream: "upstream", forkRm: "origin", wantUpstream: "upstream", wantFork: "origin"},
		{name: "explicit origin with origin upstream", forkRm: "origin", wantUpstream: "origin", wantFork: "origin", wantErr: "--fork-remote cannot be origin"},
		{name: "explicit upstream as fork", upstream: "upstream", forkRm: "upstream", wantUpstream: "upstream", wantFork: "upstream", wantErr: "is also the upstream remote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &RootOptions{UpstreamRemote: tc.upstream, profileUpstreamRemote: tc.profile, ForkRemote: tc.forkRm}
			// Copies, as node workers make per repository, resolve the same way.
			cp := *o
			for _, opts := range []*RootOptions{o, &cp} {
				if got := opts.effectiveUpstreamRemote(); got != tc.wantUpstream {
					t.Fatalf("upstream = %q, want %q", got, tc.wantUpstream)
				}
				if got := opts.effectiveForkRemote(); got != tc.wantFork {
					t.Fatalf("fork remote = %q, want %q", got, tc.wantFork)
				}
				err := opts.checkForkRemoteName()
				if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
					t.Fatalf("checkForkRemoteName = %v, want %q", err, tc.wantErr)
				}
			}
		})
	}
}

func TestUpstreamRemoteConfiguration(t *testing.T) {
	t.Parallel()
	env := newFixtureEnv(t)
	cfg := filepath.Join(t.TempDir(), "node.yaml")
	env.set("SWARM_NODE_CONFIG", cfg)
	run := func(args ...string) (string, error) {
		cmd := newRootCmdWithOptions(&RootOptions{lookupEnv: env.lookup()})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	read := func() string {
		b, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if out, err := run("--upstream-remote", "upstream", "node", "init", "--id", "n1"); err != nil || !strings.Contains(read(), "upstream_remote: upstream") || !strings.Contains(out, "Upstream:     upstream") {
		t.Fatalf("init did not save upstream_remote: %v\n%s\n%s", err, out, read())
	}
	if out, err := run("node", "status"); err != nil || !strings.Contains(out, "Upstream:     upstream") {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if _, err := run("node", "init", "--id", "n1"); err != nil || !strings.Contains(read(), "upstream_remote: upstream") {
		t.Fatalf("re-init dropped the saved default: %v\n%s", err, read())
	}
	if out, err := run("node", "status", "--json"); err != nil || !strings.Contains(out, `"upstream_remote":"upstream"`) {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if out, err := run("--upstream-remote", "origin", "node", "init", "--id", "n1"); err != nil || strings.Contains(read(), "upstream_remote") || !strings.Contains(out, "Upstream:     origin") {
		t.Fatalf("origin did not clear the default: %v\n%s", err, read())
	}
	if _, err := run("--upstream-remote", "bad/name", "node", "status"); err == nil || !strings.Contains(err.Error(), "invalid git remote name") {
		t.Fatalf("invalid name accepted: %v", err)
	}
	if _, err := run("--backend", "local", "--upstream-remote", "upstream", "issue", "status"); err == nil || !strings.Contains(err.Error(), "--upstream-remote requires the github backend") {
		t.Fatalf("local backend accepted --upstream-remote: %v", err)
	}
	if err := os.WriteFile(cfg, []byte("upstream_remote: [not, a, name\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run("issue", "status"); err == nil || !strings.Contains(err.Error(), "parse node profile") || !strings.Contains(err.Error(), "--upstream-remote <project-remote>") {
		t.Fatalf("malformed profile: %v", err)
	}
	if out, err := run("version"); err != nil {
		t.Fatalf("version failed on a malformed profile: %v\n%s", err, out)
	}
	if out, err := run("--upstream-remote", "upstream", "node", "init", "--id", "n1"); err != nil || !strings.Contains(read(), "upstream_remote: upstream") {
		t.Fatalf("init could not replace a malformed profile: %v\n%s", err, out)
	}
	help, _ := run("--help")
	for _, want := range []string{"--upstream-remote string", "--fork-remote string", `"origin" when the upstream remote is not origin`} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q:\n%s", want, help)
		}
	}
}

func TestUpstreamRemoteGeneratedCommands(t *testing.T) {
	o := &RootOptions{Backend: "github", UpstreamRemote: "my upstream", Fork: true}
	if got := strings.Join(stopContext{opts: o}.argv(false, "issue", "work", "2"), " "); !strings.Contains(got, "--upstream-remote my upstream --fork issue work 2") {
		t.Fatalf("contributor argv = %s", got)
	}
	if got := strings.Join(stopContext{opts: o}.argv(true, "issue", "approve", "2"), " "); strings.Contains(got, "--upstream-remote") {
		t.Fatalf("maintainer argv kept contributor checkout flag: %s", got)
	}
	if got := submissionRecoveryCommand(3, o); got != "swarm --upstream-remote 'my upstream' --fork pr rebase 3 --resolve" {
		t.Fatalf("recovery = %s", got)
	}
	o.UpstreamRemote, o.ForkRemote = "upstream", "fork"
	if got := submissionRecoveryCommand(3, o); got != "swarm --upstream-remote upstream --fork --fork-remote fork pr rebase 3 --resolve" {
		t.Fatalf("explicit fork override dropped: %s", got)
	}
	o.ForkRemote = "origin"
	if got := submissionRecoveryCommand(3, o); got != "swarm --upstream-remote upstream --fork pr rebase 3 --resolve" {
		t.Fatalf("default fork remote repeated: %s", got)
	}
}
