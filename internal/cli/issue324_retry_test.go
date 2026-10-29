package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestIssue324NoopRetry(t *testing.T) {
	f := newCLIFixture(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
	prGit(t, f.root, "remote", "add", "origin", remote)
	prGit(t, f.root, "push", "origin", "main")
	number := f.create(t, "agent")
	n := strconv.Itoa(number)
	f.must(t, "issue", "approve", n)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", ":")
	out, err := f.run("issue", "work", n, "--agent", "mock", "--progress=false")
	if err == nil || !strings.Contains(err.Error(), "developer produced no implementation beyond starting checkpoint") {
		t.Fatalf("expected real developer no-op failure: %v\n%s", err, out)
	}
	st := f.state(t, number)
	wt := workspace.WorktreePath(f.root, number)
	if st.HeadSHA != st.BaseSHA || prGit(t, wt, "rev-parse", "HEAD") != st.BaseSHA || prGit(t, wt, "branch", "--show-current") != st.Branch {
		t.Fatalf("no-op checkpoint identity: %+v", st)
	}
	if ref := prGit(t, f.root, "ls-remote", "--refs", "origin", "refs/heads/"+st.Branch); ref != "" {
		t.Fatalf("published: %s", ref)
	}
	for _, a := range st.Artifacts {
		if (a.Type == contract.ArtifactImplementation || a.Type == contract.ArtifactValidation) && a.Verdict == "PASS" {
			t.Fatalf("no-op granted completion: %+v", a)
		}
	}
	// Status and dry-run planning must leave authority, tokens, refs and index alone.
	snapshot := func() string {
		t.Helper()
		var b strings.Builder
		for _, name := range []string{filepath.Join(".swarm", "local-tracker", "state.json"), filepath.Join(".swarm", "coordination", "issue-"+n+".json")} {
			data, err := os.ReadFile(filepath.Join(f.root, name))
			if err != nil {
				t.Fatal(err)
			}
			b.Write(data)
		}
		b.WriteString(prGit(t, f.root, "show-ref"))
		b.WriteString(prGit(t, wt, "diff", "--binary", "HEAD"))
		return b.String()
	}
	before := snapshot()
	f.must(t, "issue", "status", n, "--json")
	f.must(t, "issue", "work", n, "--phase", "plan", "--agent", "mock", "--dry-run")
	if after := snapshot(); after != before {
		t.Fatal("read-only commands changed state or source")
	}
	out, err = f.run("issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false")
	if err != nil {
		t.Fatalf("normal plan retry after real no-op: %v\n%s", err, out)
	}
	if head := prGit(t, wt, "rev-parse", "HEAD"); head != st.HeadSHA {
		t.Fatalf("source moved: %s", head)
	}
	if ref := prGit(t, f.root, "ls-remote", "--refs", "origin", "refs/heads/"+st.Branch); ref != "" {
		t.Fatalf("retry published: %s", ref)
	}
}

func TestIssue324RecordedImplementationResume(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) {
			f := newCLIFixture(t)
			// Direct GitHub doubles use the actual GitStore atomic publication path.
			if backend == "github" {
				p := filepath.Join(f.root, ".swarm/config.yaml")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, append(b, "\n  mock:\n    binary: git\n    harness: mock\n"...), 0644); err != nil {
					t.Fatal(err)
				}
				prGit(t, f.root, "commit", "-am", "allowlist mock")
			}
			remote := filepath.Join(t.TempDir(), "origin.git")
			prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
			prGit(t, f.root, "remote", "add", "origin", remote)
			prGit(t, f.root, "push", "origin", "main")
			number := f.create(t, "agent")
			n := strconv.Itoa(number)
			run := f.run
			if backend == "github" {
				f.manager.SetStore(f.root, trustedLedgerStore(t, f.root, ""))
				tr := checkpointCLITracker{f.tracker}
				run = func(args ...string) (string, error) {
					cmd := f.command(&RootOptions{RepoRoot: f.root, Repo: "test/repo", Backend: "github", trackerOverride: tr})
					var out bytes.Buffer
					cmd.SetOut(&out)
					cmd.SetErr(&out)
					cmd.SetArgs(args)
					err := cmd.ExecuteContext(t.Context())
					return out.String(), err
				}
			}
			must := func(args ...string) {
				t.Helper()
				if out, err := run(args...); err != nil {
					t.Fatalf("swarm %v: %v\n%s", args, err, out)
				}
			}
			must("issue", "approve", n)
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'real implementation\\n' > feature.txt")
			must("issue", "work", n, "--agent", "mock", "--headless", "--progress=false")
			st := f.state(t, number)
			wt := workspace.WorktreePath(f.root, number)
			if st.HeadSHA == st.BaseSHA || st.HeadSHA == "" {
				t.Fatal("implementation did not create real source checkpoint")
			}
			remoteRef := prGit(t, f.root, "ls-remote", "--refs", "origin", "refs/heads/"+st.Branch)
			if backend == "local" && remoteRef != "" || backend == "github" && !strings.HasPrefix(remoteRef, st.HeadSHA) {
				t.Fatalf("unexpected publication: %s", remoteRef)
			}
			must("issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false")
			if prGit(t, wt, "rev-parse", "HEAD") != st.HeadSHA {
				t.Fatal("resume lost implementation")
			}
			if b, err := os.ReadFile(filepath.Join(wt, "feature.txt")); err != nil || string(b) != "real implementation\n" {
				t.Fatal("source lost")
			}
			if backend == "github" {
				prGit(t, f.root, "push", "origin", ":refs/heads/"+st.Branch)
				if out, err := run("issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false"); err == nil {
					t.Fatalf("GitStore deleted checkpoint admitted: %s", out)
				}
			}
		})
	}
}

// These doubles fail at real production PR boundaries, after checkpointing.
type issue324PublicationTracker struct {
	*tracker.LocalTracker
	fail  string
	calls int
}

func (tr *issue324PublicationTracker) CreateDraftPR(ctx context.Context, req tracker.DraftPRRequest) (*tracker.PullRequest, error) {
	tr.calls++
	switch tr.fail {
	case "create error":
		return nil, errors.New("PR create failed")
	case "nil result":
		return nil, nil
	}
	return tr.LocalTracker.CreateDraftPR(ctx, req)
}
func (tr *issue324PublicationTracker) UpdateDraftPR(ctx context.Context, n int, title, body string) (*tracker.PullRequest, error) {
	tr.calls++
	if tr.fail == "update error" {
		return nil, errors.New("PR update failed")
	}
	return tr.LocalTracker.UpdateDraftPR(ctx, n, title, body)
}

func TestIssue324SubmitFailureDeniesResume(t *testing.T) {
	for _, failure := range []string{"create error", "nil result", "update error"} {
		t.Run(failure, func(t *testing.T) {
			f := newCLIFixture(t)
			remote := filepath.Join(t.TempDir(), "origin.git")
			prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
			prGit(t, f.root, "remote", "add", "origin", remote)
			prGit(t, f.root, "push", "origin", "main")
			number := f.create(t, "agent")
			n := strconv.Itoa(number)
			f.must(t, "issue", "approve", n)
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "echo implemented > feature.txt")
			f.must(t, "issue", "work", n, "--agent", "mock", "--headless", "--progress=false")
			st := f.state(t, number)
			tr := &issue324PublicationTracker{LocalTracker: f.tracker, fail: failure}
			if failure == "update error" {
				if _, err := f.tracker.CreateDraftPR(t.Context(), tracker.DraftPRRequest{IssueNumber: number, Title: "existing", Body: "existing", Head: st.Branch, Base: "main"}); err != nil {
					t.Fatal(err)
				}
			}
			cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: tr})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"issue", "submit", n})
			if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "PR publication/description update failed") {
				t.Fatalf("submit failure: %v\n%s", err, out.String())
			}
			if tr.calls != 1 {
				t.Fatalf("PR boundary calls: %d", tr.calls)
			}
			// No remote task ref or submitted-head record is needed to retain denial.
			if ref := prGit(t, f.root, "ls-remote", "--refs", "origin", "refs/heads/"+st.Branch); ref != "" {
				t.Fatalf("local submit published task: %s", ref)
			}
			if _, err := f.run("issue", "work", n, "--phase", "plan", "--agent", "mock", "--progress=false"); err == nil {
				t.Fatal("failed PR attempt restored unpublished admission")
			}
		})
	}
}
