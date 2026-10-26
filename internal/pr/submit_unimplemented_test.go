package pr

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workflow"
)

func TestPRSubmitPreservesApprovedPlanWithoutImplementation(t *testing.T) {
	s, issue, number, published := fixture(t)
	ctx := context.Background()
	roles := map[string]int{}
	failDeveloper := true
	mock := &agent.MockRunner{}
	s.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &pausePRRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			roles[req.Role]++
			if req.Role == "developer" && failDeveloper {
				return nil, errors.New("developer agent exit 1")
			}
			return mock.Run(ctx, req)
		}}, nil
	}
	work := func(phase string) (*Result, error) {
		return s.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: phase, Gate: contract.GateAgent, Headless: true}})
	}
	if _, err := work("all"); err == nil {
		t.Fatal("failing developer reported success")
	}
	before, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	approved := func(st *lease.IssueState) bool {
		iss, err := s.Tracker.GetIssue(ctx, issue)
		if err != nil {
			t.Fatal(err)
		}
		wu, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range st.Artifacts {
			if a.Type == contract.ArtifactReview && a.Verdict == "APPROVED" {
				return st.EffectiveGate == contract.GateAgent && lease.VerifyApproval(st, wu) == nil
			}
		}
		return false
	}
	if roles["planner"] != 1 || roles["reviewer"] != 1 || before.PRRevision == nil || before.PRRevision.LocalHeadSHA != published || !approved(before) {
		t.Fatalf("setup: roles %v, state %+v", roles, before)
	}
	refuse := func(svc *Service, hint string) {
		t.Helper()
		if _, err := svc.Submit(ctx, number); err == nil || !strings.Contains(err.Error(), hint) {
			t.Fatalf("submit without implementation = %v, want hint %q", err, hint)
		}
		after, err := s.Manager.GetCurrentState(ctx, issue)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("refused submit changed state:\nbefore %+v\nafter  %+v", before, after)
		}
		if remote, err := s.Tracker.GetPullRequest(ctx, number); err != nil || remote.HeadSHA != published {
			t.Fatalf("refused submit published: %+v %v", remote, err)
		}
	}
	refuse(s, "swarm pr work "+strconv.Itoa(number)+" --phase implement")

	// The same refusal applies when the tracker is a GitHub upstream.
	hosted := *s
	hosted.Tracker = &upstreamTracker{Tracker: s.Tracker, upstream: "octo/swarm"}
	refuse(&hosted, "swarm pr work "+strconv.Itoa(number)+" --phase implement")

	// An expired claim is not replaced by a refused submit.
	expired := *s
	expired.Now = func() time.Time { return before.Lease.ExpiresAt.Add(time.Minute) }
	refuse(&expired, "swarm pr work "+strconv.Itoa(number)+" --phase implement")

	// A recorded FAIL implementation at the unchanged head is not an implementation.
	fail := contract.Artifact{Type: contract.ArtifactImplementation, Producer: "mock:developer", Verdict: "FAIL", Summary: "developer produced no implementation", Body: "no change", Metadata: lease.ReviewBindings(before)}
	fail.Metadata["head_sha"] = published
	if _, err = s.Manager.UpdateArtifact(ctx, issue, "alice", fail); err != nil {
		t.Fatal(err)
	}
	if before, err = s.Manager.GetCurrentState(ctx, issue); err != nil {
		t.Fatal(err)
	}
	refuse(s, "--phase implement")

	failDeveloper = false
	resumed, err := work("implement")
	if err != nil {
		t.Fatalf("resume implementation: %v", err)
	}
	if roles["planner"] != 1 || roles["reviewer"] != 1 || roles["developer"] != 2 || resumed.HeadSHA == published {
		t.Fatalf("resume re-planned or produced nothing: roles %v head %s", roles, resumed.HeadSHA)
	}
	submitted, err := s.Submit(ctx, number)
	if err != nil {
		t.Fatalf("submit with new commits: %v", err)
	}
	final, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil || final.PRRevision != nil || final.HeadSHA != resumed.HeadSHA || submitted.PR.HeadSHA != resumed.HeadSHA {
		t.Fatalf("submit after implementation = %+v, %v", final, err)
	}
}

func TestForkPRSubmitPreservesApprovedPlanWithoutImplementation(t *testing.T) {
	s, issue, number, base := fixture(t)
	ctx := context.Background()
	svc, gateway, _, forkHead := forkContributor(t, s, issue, number, base)
	published := forkHead()
	failDeveloper := true
	mock := &agent.MockRunner{}
	svc.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &pausePRRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			if req.Role == "developer" && failDeveloper {
				return nil, errors.New("developer agent exit 1")
			}
			return mock.Run(ctx, req)
		}}, nil
	}
	work := func(phase string) (*Result, error) {
		return svc.Work(ctx, Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Phase: phase, Gate: contract.GateAgent, Headless: true}})
	}
	if _, err := work("all"); err == nil {
		t.Fatal("failing developer reported success")
	}
	before, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil || before.PRRevision == nil || before.PRRevision.LocalHeadSHA != published {
		t.Fatalf("setup: %+v %v", before, err)
	}
	operations := len(gateway.operations)
	hint := "swarm --fork pr work " + strconv.Itoa(number) + " --phase implement"
	if _, err = svc.Submit(ctx, number); err == nil || !strings.Contains(err.Error(), hint) {
		t.Fatalf("contributor submit without implementation = %v, want hint %q", err, hint)
	}
	after, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil || !reflect.DeepEqual(before, after) || len(gateway.operations) != operations || forkHead() != published {
		t.Fatalf("refused contributor submit wrote state or published: ops %v, %v", gateway.operations[operations:], err)
	}

	failDeveloper = false
	resumed, err := work("implement")
	if err != nil {
		t.Fatalf("contributor resume: %v", err)
	}
	if _, err = svc.Submit(ctx, number); err != nil {
		t.Fatalf("contributor submit with new commits: %v", err)
	}
	final, err := s.Manager.GetCurrentState(ctx, issue)
	if err != nil || final.PRRevision != nil || final.HeadSHA != resumed.HeadSHA || forkHead() != resumed.HeadSHA {
		t.Fatalf("contributor submit after implementation = %+v, %v", final, err)
	}
}
