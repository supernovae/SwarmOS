package pr

import (
	"context"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"strconv"
	"strings"
	"testing"
)

func TestIssue102RetargetIdentity(t *testing.T) {
	s, issue, number, _ := fixture(t)
	ctx := context.Background()
	pr, err := s.Tracker.GetPullRequest(ctx, number)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := s.Tracker.GetIssue(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	original := pr.Base
	pr.Base = "different-102" // Same SHA, different name.
	if _, err := s.verifyLink(pr, iss, st); err == nil || !strings.Contains(err.Error(), original) || !strings.Contains(err.Error(), "different-102") {
		t.Fatalf("retarget accepted: %v", err)
	}
}

type issue102PublishTracker struct {
	*forkPRTracker
	reads, retargetAt, writes int
}

func (tr *issue102PublishTracker) GetPullRequest(ctx context.Context, n int) (*tracker.PullRequest, error) {
	pr, err := tr.forkPRTracker.GetPullRequest(ctx, n)
	tr.reads++
	if err == nil && tr.reads == tr.retargetAt {
		cp := *pr
		cp.Base = "other-102"
		return &cp, nil
	}
	return pr, err
}
func (tr *issue102PublishTracker) UpdateDraftPR(ctx context.Context, n int, title, body string) (*tracker.PullRequest, error) {
	tr.writes++
	return tr.Tracker.UpdateDraftPR(ctx, n, title, body)
}

func TestIssue102ForkReleasePublishLateRetarget(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(strconv.FormatBool(late), func(t *testing.T) {
			s, issue, number, base := fixture(t)
			ctx := context.Background()
			git := func(args ...string) string {
				t.Helper()
				out, err := gitfixture.Command(t, "", append([]string{"-C", s.Root}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("branch", "release", base)
			iss, err := s.Tracker.GetIssue(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Tracker.UpdateIssueBody(ctx, issue, strings.Replace(iss.Body, "workflow: feature", "base_branch: release\nworkflow: feature", 1)); err != nil {
				t.Fatal(err)
			}
			svc, gateway, view, forkHead := forkContributor(t, s, issue, number, base)
			view.target = "release"
			worked, err := svc.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
			if err != nil {
				t.Fatal(err)
			}
			if worked.State.BaseBranch != "release" || worked.State.BaseSHA != base {
				t.Fatalf("wrong target: %+v", worked.State)
			}
			observed := &issue102PublishTracker{forkPRTracker: view}
			svc.Tracker = observed
			if late {
				observed.retargetAt = 2
			}
			published := forkHead()
			refs := git("ls-remote", "fork")
			ops := len(gateway.operations)
			result, err := svc.Submit(ctx, number)
			if late {
				if err == nil || !strings.Contains(err.Error(), "before publish") {
					t.Fatalf("late retarget accepted: reads=%d err=%v", observed.reads, err)
				}
				if observed.reads != 2 || observed.writes != 0 || forkHead() != published || git("ls-remote", "fork") != refs {
					t.Fatal("late retarget published or projected")
				}
				if len(gateway.operations[ops:]) != 1 || gateway.operations[ops] != "update-artifact" {
					t.Fatalf("unexpected prepublication mutations: %v", gateway.operations[ops:])
				}
				for _, op := range gateway.operations[ops:] {
					if op == "complete-pr-revision" {
						t.Fatal("late retarget completed revision")
					}
				}
				st, e := s.Manager.GetCurrentState(ctx, issue)
				if e != nil || st.PRRevision == nil || st.Lease.Status != "active" {
					t.Fatalf("completion mutated: %+v %v", st, e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.PR.Base != "release" || forkHead() != worked.HeadSHA {
				t.Fatalf("wrong publication: %+v", result)
			}
			st, e := s.Manager.GetCurrentState(ctx, issue)
			if e != nil || st.PRRevision != nil || st.Lease.Status != "completed" {
				t.Fatalf("completion: %+v %v", st, e)
			}
		})
	}
}
