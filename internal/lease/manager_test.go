package lease

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
)

func defaultTestSecurity() contract.SecurityConfig {
	return contract.SecurityConfig{
		RequireReadyLabel:   true,
		ReadyLabel:          "swarm:ready",
		TrustedAssociations: []string{"OWNER", "MEMBER", "COLLABORATOR"},
	}
}

func TestUntrustedIssueRejection(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	lt := tracker.NewLocalTracker(tmp).WithUser("external-user", "NONE")

	// Create an issue without swarm:ready and from an untrusted user ("NONE")
	iss, err := lt.CreateIssue(ctx, "Untrusted Issue", "### Objective\nHack CI\n", nil)
	if err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(lt, defaultTestSecurity())
	_, err = mgr.Acquire(ctx, iss.Number, "agent-1", "claude", "plan", 4*time.Hour, 3)
	if !errors.Is(err, ErrUntrustedIssue) {
		t.Fatalf("expected ErrUntrustedIssue, got: %v", err)
	}
}

func TestAcquireAndAlreadyClaimed(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	lt := tracker.NewLocalTracker(tmp)

	iss, err := lt.CreateIssue(ctx, "Trusted Issue", "### Objective\nBuild feature\n", []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(lt, defaultTestSecurity())
	st, err := mgr.Acquire(ctx, iss.Number, "alice", "claude", "plan", 4*time.Hour, 3)
	if err != nil {
		t.Fatalf("unexpected error acquiring lease: %v", err)
	}
	if st.Lease.Owner != "alice" || st.Lease.Status != "active" {
		t.Fatalf("unexpected lease state: %+v", st.Lease)
	}

	// Second claimant bob must fail with ErrAlreadyClaimed while lease is active
	_, err = mgr.Acquire(ctx, iss.Number, "bob", "omp", "plan", 4*time.Hour, 3)
	if !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("expected ErrAlreadyClaimed, got: %v", err)
	}
}

func TestConcurrentClaimTieBreaking(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	lt := tracker.NewLocalTracker(tmp)

	iss, err := lt.CreateIssue(ctx, "Race Issue", "### Objective\nRace test\n", []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()

	// Simulate a concurrent winner who posts an earlier comment ID right before bob's comment is verified.
	// We insert alice's active state comment directly after bob's initial ListComments check by using an earlier comment ID:
	// Even better, let's pre-populate an active comment that races during Acquire by hooking CreateComment.
	hookTriggered := false
	lt.SetAfterCreateCommentHook(func(issueNumber int, c *tracker.Comment) {
		if hookTriggered {
			return
		}
		hookTriggered = true
		// Mutate comment 1000 in place to belong to concurrent winner "alice" and re-append bob's comment as 1001
		winnerState := &IssueState{
			Lease: contract.Lease{
				Version:     1,
				IssueNumber: issueNumber,
				Owner:       "alice-winner",
				Agent:       "claude",
				Phase:       "plan",
				Status:      "active",
				ClaimedAt:   now,
				ExpiresAt:   now.Add(4 * time.Hour),
				CommentID:   c.ID,
			},
		}
		winnerBody, _ := FormatStateComment(winnerState)
		// Create second comment for bob and make first comment alice's
		bobCopy, _ := lt.CreateComment(ctx, issueNumber, c.Body)
		_, _ = lt.UpdateComment(ctx, c.ID, winnerBody)
		c.ID = bobCopy.ID
	})

	mgr := NewManager(lt, defaultTestSecurity())
	_, err = mgr.Acquire(ctx, iss.Number, "bob-loser", "omp", "plan", 4*time.Hour, 3)
	if !errors.Is(err, ErrClaimRaceLost) {
		t.Fatalf("expected ErrClaimRaceLost, got: %v", err)
	}
}

func TestLazyLeaseExpirationReclamation(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	lt := tracker.NewLocalTracker(tmp)

	iss, err := lt.CreateIssue(ctx, "Expiring Issue", "### Objective\nLazy expiry\n", []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)
	mgr := NewManager(lt, defaultTestSecurity())
	mgr.SetNowFunc(func() time.Time { return t0 })

	_, err = mgr.Acquire(ctx, iss.Number, "stale-owner", "claude", "plan", 1*time.Hour, 3)
	if err != nil {
		t.Fatalf("initial acquire failed: %v", err)
	}

	// Advance clock past 1h TTL -> lazy expiration allows new-owner to reclaim without manual release
	mgr.SetNowFunc(func() time.Time { return t0.Add(2 * time.Hour) })
	st2, err := mgr.Acquire(ctx, iss.Number, "new-owner", "omp", "plan", 4*time.Hour, 3)
	if err != nil {
		t.Fatalf("expected reclamation after lazy lease expiry, got: %v", err)
	}
	if st2.Lease.Owner != "new-owner" {
		t.Fatalf("expected owner new-owner, got %q", st2.Lease.Owner)
	}
}

func TestUpdateArtifactAndRelease(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	lt := tracker.NewLocalTracker(tmp)

	iss, err := lt.CreateIssue(ctx, "Artifact Issue", "### Objective\nTest artifacts\n", []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(lt, defaultTestSecurity())
	_, err = mgr.Acquire(ctx, iss.Number, "alice", "claude", "plan", 4*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}

	st, err := mgr.UpdateArtifact(ctx, iss.Number, "alice", contract.Artifact{
		Type:     contract.ArtifactDesign,
		Producer: "claude:planner",
		Summary:  "Initial design",
		Body:     "## Design\nModify main.go",
	})
	if err != nil {
		t.Fatalf("UpdateArtifact failed: %v", err)
	}
	if len(st.Artifacts) != 1 || st.Artifacts[0].Type != contract.ArtifactDesign {
		t.Fatalf("unexpected artifacts: %+v", st.Artifacts)
	}

	if _, err := mgr.Renew(ctx, iss.Number, "alice", 4*time.Hour); err != nil {
		t.Fatalf("Renew failed: %v", err)
	}
	if _, err := mgr.Renew(ctx, iss.Number, "alice", 4*time.Hour); err != nil {
		t.Fatalf("Second renew failed: %v", err)
	}
	if _, err := mgr.Renew(ctx, iss.Number, "alice", 4*time.Hour); !errors.Is(err, ErrMaxRenewals) {
		t.Fatalf("expected ErrMaxRenewals on 3rd renew, got: %v", err)
	}

	relState, err := mgr.Release(ctx, iss.Number, "alice", "completed testing")
	if err != nil {
		t.Fatalf("Release failed: %v", err)
	}
	if relState.Lease.Status != "released" {
		t.Fatalf("expected status released, got %q", relState.Lease.Status)
	}
}

func TestTrustedAndUntrustedReviewerComments(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	lt := tracker.NewLocalTracker(tmp)

	iss, err := lt.CreateIssue(ctx, "Review comment trust", "### Objective\nReview plan\n", []string{"swarm:ready"})
	if err != nil {
		t.Fatal(err)
	}

	security := defaultTestSecurity()
	security.TrustedReviewers = []string{"grok-bot", "openai-dot"}
	mgr := NewManager(lt, security)
	if _, err := mgr.Acquire(ctx, iss.Number, "alice", "claude", "plan", 4*time.Hour, 3); err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	untrusted := lt.WithUser("random-stranger", "NONE")
	if _, err := untrusted.CreateComment(ctx, iss.Number, "/swarm approve-plan"); err != nil {
		t.Fatalf("creating untrusted review comment: %v", err)
	}
	st, err := mgr.GetCurrentState(ctx, iss.Number)
	if err != nil {
		t.Fatalf("GetCurrentState after untrusted comment: %v", err)
	}
	for _, artifact := range st.Artifacts {
		if artifact.Type == contract.ArtifactReview {
			t.Fatalf("untrusted commenter created review artifact: %+v", artifact)
		}
	}

	trusted := lt.WithUser("grok-bot", "NONE")
	if _, err := trusted.CreateComment(ctx, iss.Number, "/swarm approve-plan"); err != nil {
		t.Fatalf("creating trusted review comment: %v", err)
	}
	st, err = mgr.GetCurrentState(ctx, iss.Number)
	if err != nil {
		t.Fatalf("GetCurrentState after trusted comment: %v", err)
	}
	for _, artifact := range st.Artifacts {
		if artifact.Type == contract.ArtifactReview {
			if artifact.Verdict != "APPROVED" || artifact.Producer != "reviewer:grok-bot" {
				t.Fatalf("unexpected trusted review artifact: %+v", artifact)
			}
			return
		}
	}
	t.Fatal("trusted reviewer comment did not create an APPROVED review artifact")
}
