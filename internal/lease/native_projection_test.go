package lease

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
)

type nativeAuthorityTracker struct {
	tracker.Tracker
	dependency *tracker.Issue
	native     []tracker.BlockedByReference
	reads      int
}

func (tr *nativeAuthorityTracker) ListBlockedBy(context.Context, int) ([]tracker.BlockedByReference, error) {
	tr.reads++
	return tr.native, nil
}
func (tr *nativeAuthorityTracker) GetIssue(ctx context.Context, n int) (*tracker.Issue, error) {
	if n == tr.dependency.Number {
		return tr.dependency, nil
	}
	return tr.Tracker.GetIssue(ctx, n)
}

func TestNativeProjectionBodyOnlyClaims(t *testing.T) {
	for _, bodyDependency := range []bool{false, true} {
		t.Run(map[bool]string{false: "native-only", true: "body-only"}[bodyDependency], func(t *testing.T) {
			m, local, issue := testManager(t)
			tr := &nativeAuthorityTracker{Tracker: local, dependency: &tracker.Issue{Number: 77, State: "open"}}
			if bodyDependency {
				body := strings.Replace(issue.Body, "workflow: feature", "depends_on: [77]\nworkflow: feature", 1)
				if _, err := local.UpdateIssueBody(t.Context(), issue.Number, body); err != nil {
					t.Fatal(err)
				}
				if _, err := m.ApproveTask(t.Context(), issue.Number, "local-dev"); err != nil {
					t.Fatal(err)
				}
			} else {
				tr.native = []tracker.BlockedByReference{{Owner: "octo", Repo: "swarm", Number: 77}}
			}
			m.tracker = tr
			current, err := local.GetIssue(t.Context(), issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			wu, err := contract.ParseWorkUnitFromIssue(current.Number, current.Title, current.Body)
			if err != nil {
				t.Fatal(err)
			}
			verifyErr := m.VerifyDependencies(t.Context(), wu)
			_, claimErr := m.Acquire(t.Context(), issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
			if bodyDependency {
				if !errors.Is(verifyErr, ErrDependenciesUnmet) || !errors.Is(claimErr, ErrDependenciesUnmet) {
					t.Fatalf("body dependency: verify=%v claim=%v", verifyErr, claimErr)
				}
				tr.dependency.State = "closed"
				if err := m.VerifyDependencies(t.Context(), wu); err != nil {
					t.Fatal(err)
				}
				if _, err := m.Acquire(t.Context(), issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
					t.Fatal(err)
				}
			} else if verifyErr != nil || claimErr != nil {
				t.Fatalf("native dependency blocked: verify=%v claim=%v", verifyErr, claimErr)
			}
			if tr.reads != 0 {
				t.Fatalf("authority read native projection %d times", tr.reads)
			}
		})
	}
}
