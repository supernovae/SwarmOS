package fork

import (
	"context"
	"fmt"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"strings"
	"testing"
)

type issue102Tracker struct {
	tracker.Tracker
	pr *tracker.PullRequest
}

func (t issue102Tracker) GetPullRequest(context.Context, int) (*tracker.PullRequest, error) {
	return t.pr, nil
}

func TestIssue102CoordinatorRetargetAndDiscovery(t *testing.T) {
	st := &lease.IssueState{ForkRepo: "alice/repo", Branch: "work"}
	pr := &tracker.PullRequest{Number: 7, State: "open", Base: "other", BaseRepo: "upstream/repo", HeadRepo: "alice/repo", Head: "alice:work"}
	c := &Coordinator{RepoRoot: t.TempDir(), UpstreamRepo: "upstream/repo", Tracker: issue102Tracker{pr: pr}, DefaultBaseFunc: func(string) (string, error) { return "release", nil }}
	if _, err := c.verifyForkPR(context.Background(), st, 7); err == nil || !strings.Contains(err.Error(), "release") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("retarget accepted: %v", err)
	}
	pr.Base = "release"
	if _, err := c.verifyForkPR(context.Background(), st, 7); err != nil {
		t.Fatal(err)
	}
	c.DefaultBaseFunc = func(string) (string, error) { return "", fmt.Errorf("discovery failed") }
	if _, err := c.verifyForkPR(context.Background(), st, 7); err == nil || !strings.Contains(err.Error(), "discovery failed") {
		t.Fatalf("discovery bypass: %v", err)
	}
}
