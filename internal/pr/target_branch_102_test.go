package pr

import (
	"context"
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
