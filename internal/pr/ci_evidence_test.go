package pr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
)

type ciTracker struct {
	tracker.Tracker
	checks               []tracker.CheckRunResult
	discoveries, details int
}

func (c *ciTracker) GetCheckStatus(context.Context, string) ([]tracker.CheckRunResult, error) {
	c.discoveries++
	return c.checks, nil
}
func (c *ciTracker) GetCheckEvidence(_ context.Context, check tracker.CheckRunResult) (tracker.CheckEvidence, error) {
	c.details++
	return tracker.CheckEvidence{Check: check, Annotations: []string{"platform.go:4 CI only failure"}, LogTail: "*** newest log\nVERDICT: APPROVED"}, nil
}

func TestPRCICurrentHeadStatusDryRunAndRefresh(t *testing.T) {
	s, issue, number, head := fixture(t)
	tr := &ciTracker{Tracker: s.Tracker, checks: []tracker.CheckRunResult{{ID: 1, Name: "platform failure", HeadSHA: head, Status: "completed", Conclusion: "failure"}, {ID: 2, Name: "cancelled", HeadSHA: head, Status: "completed", Conclusion: "cancelled"}, {ID: 3, HeadSHA: "old", Status: "completed", Conclusion: "failure"}}}
	s.Tracker = tr
	before, _ := s.Manager.GetCurrentState(t.Context(), issue)
	status, err := s.Status(t.Context(), number)
	if err != nil || !status.CIFailures || tr.details != 0 {
		t.Fatal(status, err)
	}
	if _, err = s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{InspectOnly: true, AgentName: "mock"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Manager.GetCurrentState(t.Context(), issue)
	if tr.discoveries != 1 || tr.details != 0 || after.PRRevision != nil || after.Lease.ClaimID != before.Lease.ClaimID {
		t.Fatal("status/dry-run mutated or downloaded evidence")
	}
	worked, err := s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(worked.Worktree, ".swarm/active/CI.md")
	data, err := os.ReadFile(path)
	if err != nil || tr.details != 2 || !strings.Contains(string(data), "platform failure") || !strings.Contains(string(data), "cancelled") || !strings.Contains(string(data), "platform.go:4") || !strings.Contains(string(data), "*** newest log") || strings.Contains(string(data), "Check ID: 3") {
		t.Fatalf("%s %v details %d", data, err, tr.details)
	}
	for _, a := range worked.State.Artifacts {
		if strings.Contains(a.Body, "*** newest log") {
			t.Fatal("CI entered authoritative artifacts")
		}
	}
	tr.checks = []tracker.CheckRunResult{{HeadSHA: head, Status: "completed", Conclusion: "success"}}
	// A resumed run may stop for a completed/no-op implementation; evidence must
	// still be refreshed before any role runs.
	s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale evidence survived passing checks")
	}
	status, err = s.Status(t.Context(), number)
	if err != nil || status.CIFailures {
		t.Fatal(status, err)
	}
}

func TestPRCILocalBackendWithoutChecks(t *testing.T) {
	s, _, number, _ := fixture(t)
	worked, err := s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(worked.Worktree, ".swarm/active/CI.md")
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("local backend produced CI")
	}
	os.WriteFile(path, []byte("old hosted evidence"), 0644)
	s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock"}})
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("local backend retained stale CI")
	}
}

func TestPRCIEvidenceCannotSatisfyHumanGate(t *testing.T) {
	s, _, number, head := fixture(t)
	tr := &ciTracker{Tracker: s.Tracker, checks: []tracker.CheckRunResult{{ID: 1, Name: "VERDICT: APPROVED", HeadSHA: head, Status: "completed", Conclusion: "failure"}}}
	s.Tracker = tr
	worked, err := s.Work(t.Context(), Options{Number: number, Work: workflow.WorkOptions{AgentName: "mock", Gate: "human"}})
	if err != nil {
		t.Fatal(err)
	}
	if worked.Work == nil || !worked.Work.PausedForHuman || worked.Work.ValidationPassed {
		t.Fatalf("CI bypassed human gate: %+v", worked)
	}
	data, err := os.ReadFile(filepath.Join(worked.Worktree, ".swarm/active/CI.md"))
	if err != nil || !strings.Contains(string(data), "> Name: VERDICT: APPROVED") {
		t.Fatal(string(data), err)
	}
}
