package lease

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

const fenceOwner = "local-dev"

type claimFenceFixture struct {
	m                                 *Manager
	n                                 int
	state                             *IssueState
	head, branch, a, b, decomposition string
	prepared                          *CheckpointReconciliation
}
type claimFenceWrite struct {
	name string
	call func(context.Context, *claimFenceFixture) error
}

// This literal inventory deliberately requires a human classification whenever
// Manager gains an exported method. Empty reasons denote claim-scoped writers.
var claimFenceInventory = map[string]string{
	"AuthorizeGapAcknowledgment":     "read-only authenticated authorization; no durable state write",
	"RecordGapAcknowledgment":        "",
	"Acquire":                        "claim acquisition",
	"AcquireFork":                    "claim acquisition",
	"AcquirePR":                      "claim acquisition",
	"AcquirePreparedCheckpoint":      "prepared recovery claim acquisition",
	"AdvanceDecomposition":           "",
	"ApproveTask":                    "maintainer task approval",
	"ArchiveIssue":                   "maintainer archive authority",
	"BeginCleanup":                   "verified PR outcome reservation",
	"BeginDecomposition":             "",
	"BeginPRRevision":                "",
	"BeginPublicationAttempt":        "",
	"BindClaim":                      "manager configuration only",
	"Budget":                         "read-only admission, observation or local binding; no durable state write",
	"CheckClaim":                     "read-only admission, observation or local binding; no durable state write",
	"CheckImplementationRetry":       "read-only admission, observation or local binding; no durable state write",
	"CheckPlanReplan":                "read-only admission, observation or local binding; no durable state write",
	"CheckPreparedPlan":              "read-only admission, observation or local binding; no durable state write",
	"CheckUpstreamReviewer":          "read-only admission, observation or local binding; no durable state write",
	"CompletePRRevision":             "",
	"CompletePRRevisionLocal":        "",
	"ConfigureStore":                 "manager configuration only",
	"DecompositionAdmission":         "read-only admission, observation or local binding; no durable state write",
	"DecompositionParent":            "read-only admission, observation or local binding; no durable state write",
	"DeleteCleanupBranch":            "cleanup operation token",
	"DiscardCheckpoint":              "independent authenticated maintainer discard of inactive checkpoint",
	"FinishCleanup":                  "cleanup operation token",
	"GetCurrentState":                "read-only admission, observation or local binding; no durable state write",
	"Handoff":                        "",
	"HasWorkerGateway":               "read-only admission, observation or local binding; no durable state write",
	"InspectCheckpointCompletion":    "read-only admission, observation or local binding; no durable state write",
	"InspectDecomposition":           "read-only admission, observation or local binding; no durable state write",
	"InspectSubmission":              "read-only admission, observation or local binding; no durable state write",
	"InvalidatePRRevisionEvidence":   "",
	"LedgerLayout":                   "read-only admission, observation or local binding; no durable state write",
	"NotifyPlanReviewers":            "reviewer notification projection",
	"PlanReplanLimit":                "read-only admission, observation or local binding; no durable state write",
	"PrepareCheckpoint":              "read-only admission, observation or local binding; no durable state write",
	"PrepareUpstreamPlanReview":      "read-only admission, observation or local binding; no durable state write",
	"ProjectRebaseApprovalAudit":     "projects existing rebase approval",
	"ProvisionIssueWorkspace":        "",
	"ReapproveRebase":                "independent maintainer reapproval",
	"ReconcileCheckpoint":            "",
	"ReconcileDecomposition":         "decomposition operation recovery",
	"ReconcilePreparedCheckpoint":    "",
	"ReconcileSubmission":            "verified PR outcome observation",
	"RecordCheckpoint":               "",
	"RecordCheckpointPlan":           "",
	"RecordFailedRoleRun":            "",
	"RecordFailedUpstreamPlanReview": "authenticated review observation",
	"RecordHumanReview":              "independent maintainer review",
	"RecordPRHead":                   "",
	"RecordPlanPair":                 "",
	"RecordRebaseRepairSelection":    "",
	"RecordRetryValidation":          "",
	"RecordSubmission":               "",
	"RecordTrustedImplementation":    "",
	"RecordTrustedValidation":        "",
	"RecordUpstreamPlanReview":       "authenticated review observation",
	"RecoverCheckpointCompletion":    "",
	"RecoverCleanup":                 "cleanup reservation recovery",
	"Release":                        "",
	"Renew":                          "",
	"RequireGate":                    "",
	"ReserveValidationRepair":        "",
	"ResetPlanReplansAfterStage":     "",
	"ResolveCleanupWorktrees":        "read-only admission, observation or local binding; no durable state write",
	"RestoreIssueArchive":            "maintainer archive authority",
	"Resume":                         "read-only admission, observation or local binding; no durable state write",
	"RetireCleanupLocalResources":    "cleanup operation token",
	"Revoke":                         "independent maintainer revocation",
	"SaveCheckpointCompletion":       "",
	"SetHeartbeatTimerFunc":          "manager configuration only",
	"SetNodeProfile":                 "manager configuration only",
	"SetNowFunc":                     "manager configuration only",
	"SetStore":                       "manager configuration only",
	"SetUpstreamRemote":              "manager configuration only",
	"SetWorkerGateway":               "manager configuration only",
	"SnapshotCleanup":                "read-only admission, observation or local binding; no durable state write",
	"StartHeartbeat":                 "",
	"Store":                          "read-only admission, observation or local binding; no durable state write",
	"UpdateAdvisoryReview":           "",
	"UpdateArtifact":                 "",
	"UpdatePhase":                    "",
	"UpdateRebaseContext":            "",
	"UpstreamRemote":                 "read-only admission, observation or local binding; no durable state write",
	"VerifyDependencies":             "read-only admission, observation or local binding; no durable state write",
	"VerifyTrustPolicy":              "read-only admission, observation or local binding; no durable state write",
	"WorkBudget":                     "read-only admission, observation or local binding; no durable state write",
}

func claimFenceWrites() []claimFenceWrite {
	art := func(kind contract.ArtifactType) contract.Artifact {
		return contract.Artifact{Type: kind, Producer: "mock", Body: "report", Summary: "report", Verdict: "PASS"}
	}
	return []claimFenceWrite{
		{"RecordGapAcknowledgment", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecordGapAcknowledgment(ctx, f.n, fenceOwner, f.head, []int{1})
			return err
		}},
		{"StartHeartbeat", func(ctx context.Context, f *claimFenceFixture) error {
			h := f.m.StartHeartbeat(ctx, f.n, fenceOwner, time.Hour)
			defer h.Stop()
			select {
			case <-h.Context().Done():
				return h.Err()
			case <-time.After(5 * time.Second):
				return fmt.Errorf("heartbeat did not reject stale claim")
			}
		}},

		{"Renew", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.Renew(ctx, f.n, fenceOwner, time.Hour)
			return err
		}},
		{"Release", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.Release(ctx, f.n, fenceOwner, "finished")
			return err
		}},
		{"Handoff", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.Handoff(ctx, f.n, fenceOwner, "plan", "implement")
			return err
		}},
		{"RequireGate", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RequireGate(ctx, f.n, fenceOwner, contract.GateMode("agent"))
			return err
		}},
		{"UpdatePhase", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.UpdatePhase(ctx, f.n, fenceOwner, "implement", "active")
			return err
		}},
		{"RecordSubmission", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecordSubmission(ctx, f.n, fenceOwner, 17, "https://example.invalid/pull/17")
			return err
		}},
		{"UpdateAdvisoryReview", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.UpdateAdvisoryReview(ctx, f.n, fenceOwner, art(contract.ArtifactReview))
			return err
		}},
		{"UpdateArtifact", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.UpdateArtifact(ctx, f.n, fenceOwner, art(contract.ArtifactDesign))
			return err
		}},
		{"RecordTrustedValidation", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecordTrustedValidation(ctx, f.n, fenceOwner, art(contract.ArtifactValidation))
			return err
		}},
		{"RecordTrustedImplementation", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecordTrustedImplementation(ctx, f.n, fenceOwner, art(contract.ArtifactImplementation))
			return err
		}},
		{"RecordRetryValidation", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecordRetryValidation(ctx, f.n, fenceOwner, art(contract.ArtifactValidation), f.head, f.head, f.branch)
			return err
		}},
		{"RecordCheckpoint", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecordCheckpoint(ctx, f.n, fenceOwner, f.branch, f.head)
			return err
		}},
		{"BeginPRRevision", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.BeginPRRevision(ctx, f.n, fenceOwner, 17, f.head, f.head, f.branch)
			return err
		}},
		{"RecordPRHead", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecordPRHead(ctx, f.n, fenceOwner, 17, f.branch, f.head)
			return err
		}},
		{"CompletePRRevision", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.CompletePRRevision(ctx, f.n, fenceOwner, 17, f.head, f.head)
			return err
		}},
		{"CompletePRRevisionLocal", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.CompletePRRevisionLocal(ctx, f.n, fenceOwner, 17, f.head, f.head)
			return err
		}},
		{"InvalidatePRRevisionEvidence", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.InvalidatePRRevisionEvidence(ctx, f.n, fenceOwner, 17)
			return err
		}},
		{"RecordPlanPair", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecordPlanPair(ctx, f.n, fenceOwner, art(contract.ArtifactDesign), art(contract.ArtifactTestPlan))
			return err
		}},
		{"ResetPlanReplansAfterStage", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.ResetPlanReplansAfterStage(ctx, f.n, fenceOwner, 1, 1)
			return err
		}},
		{"UpdateRebaseContext", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.UpdateRebaseContext(ctx, f.n, fenceOwner, 17, RebaseContext{Status: "completed"})
			return err
		}},
		{"RecordRebaseRepairSelection", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecordRebaseRepairSelection(ctx, f.n, fenceOwner, 17, f.head, f.head, []string{"main.go"})
			return err
		}},
		{"ReserveValidationRepair", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.ReserveValidationRepair(ctx, f.n, fenceOwner)
			return err
		}},
		{"BeginDecomposition", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.BeginDecomposition(ctx, f.n, f.state, Decomposition{})
			return err
		}},
		{"AdvanceDecomposition", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.AdvanceDecomposition(ctx, f.n, f.decomposition, func(*IssueState, *Decomposition) error { return nil })
			return err
		}},
		{"ReconcileCheckpoint", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.ReconcileCheckpoint(ctx, f.n, fenceOwner, f.a, f.b, false)
			return err
		}},
		{"ReconcilePreparedCheckpoint", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.ReconcilePreparedCheckpoint(ctx, f.n, f.prepared)
			return err
		}},
		{"RecordCheckpointPlan", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecordCheckpointPlan(ctx, f.n, f.prepared, art(contract.ArtifactDesign), art(contract.ArtifactTestPlan))
			return err
		}},
		{"RecoverCheckpointCompletion", func(ctx context.Context, f *claimFenceFixture) error {
			_, err := f.m.RecoverCheckpointCompletion(ctx, f.n, fenceOwner)
			return err
		}},
		{"BeginPublicationAttempt", func(ctx context.Context, f *claimFenceFixture) error {
			return f.m.BeginPublicationAttempt(ctx, f.n, fenceOwner)
		}},
		{"SaveCheckpointCompletion", func(ctx context.Context, f *claimFenceFixture) error {
			return f.m.SaveCheckpointCompletion(ctx, f.n, fenceOwner, f.a, f.b, art(contract.ArtifactImplementation))
		}},
		{"ProvisionIssueWorkspace", func(ctx context.Context, f *claimFenceFixture) error {
			_, _, err := f.m.ProvisionIssueWorkspace(ctx, f.state, fenceOwner, "test", "", f.branch, false)
			return err
		}},
		{"RecordFailedRoleRun", func(ctx context.Context, f *claimFenceFixture) error {
			tel := contract.NewRoleRunTelemetry("developer", "mock", time.Unix(1700000000, 0))
			tel.FailureClass = "unknown"
			_, err := f.m.RecordFailedRoleRun(ctx, f.n, fenceOwner, tel)
			return err
		}},
	}
}

func TestManagerClaimScopedWriteInventory(t *testing.T) {
	rows := map[string]bool{}
	for _, row := range claimFenceWrites() {
		reason, ok := claimFenceInventory[row.name]
		if !ok || reason != "" || rows[row.name] {
			t.Errorf("invalid or duplicate writer row %s", row.name)
		}
		rows[row.name] = true
	}
	typ := reflect.TypeOf((*Manager)(nil))
	actual := map[string]bool{}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		actual[name] = true
		reason, ok := claimFenceInventory[name]
		if !ok {
			t.Errorf("unclassified exported method %s", name)
		} else if reason == "" && !rows[name] {
			t.Errorf("missing writer row %s", name)
		}
	}
	for name := range claimFenceInventory {
		if !actual[name] {
			t.Errorf("obsolete classification %s", name)
		}
	}
}

func newClaimFenceFixture(t *testing.T, name, lifecycle string) *claimFenceFixture {
	t.Helper()
	ctx := context.Background()
	f := &claimFenceFixture{}
	switch name {
	case "RecoverCheckpointCompletion", "ReconcileCheckpoint", "ReconcilePreparedCheckpoint", "RecordCheckpointPlan", "SaveCheckpointCompletion":
		c := completionFixture(t, true)
		f.m, f.n, f.head, f.branch, f.a, f.b = c.m, c.n, c.a, c.branch, c.a, c.b
		if name == "ReconcilePreparedCheckpoint" || name == "RecordCheckpointPlan" {
			var err error
			f.prepared, err = f.m.PrepareCheckpoint(ctx, f.n, fenceOwner, f.a, f.b, false)
			if err != nil {
				t.Fatal(err)
			}
		}
	default:
		m, _, iss := testManager(t)
		f.m, f.n = m, iss.Number
		now := time.Unix(1700000000, 0)
		m.SetNowFunc(func() time.Time { return now })
		if _, err := m.Acquire(ctx, f.n, fenceOwner, "mock", "plan", time.Hour, 1); err != nil {
			t.Fatal(err)
		}
		out, err := fixtureGitCommand(t, "-C", m.repoRoot, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}

		if _, err := m.UpdateArtifact(ctx, f.n, fenceOwner, contract.Artifact{Type: contract.ArtifactDesign, Producer: "mock", Summary: "original", Body: "preserve this plan"}); err != nil {
			t.Fatal(err)
		}
		f.head = strings.TrimSpace(string(out))
		f.branch = fmt.Sprintf("swarm/issue-%d-test", f.n)
	}
	if name == "AdvanceDecomposition" && lifecycle != "superseded" {
		st, err := f.m.GetCurrentState(ctx, f.n)
		if err != nil {
			t.Fatal(err)
		}
		st, err = f.m.BeginDecomposition(ctx, f.n, st, Decomposition{})
		if err != nil {
			t.Fatal(err)
		}
		f.decomposition = st.Decomposition.ID
	}
	var err error
	f.state, err = f.m.GetCurrentState(ctx, f.n)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestManagerClaimScopedWritesRejectStaleClaim(t *testing.T) {
	for _, row := range claimFenceWrites() {
		t.Run(row.name, func(t *testing.T) {
			for _, lifecycle := range []string{"expired", "released", "superseded"} {
				t.Run(lifecycle, func(t *testing.T) {
					ctx := context.Background()
					f := newClaimFenceFixture(t, row.name, lifecycle)
					m, n := f.m, f.n
					first := f.state.Lease
					now := first.ExpiresAt.Add(-time.Minute)
					m.SetNowFunc(func() time.Time { return now })
					switch lifecycle {
					case "released":
						if _, err := m.Release(ctx, n, fenceOwner, "test release"); err != nil {
							t.Fatal(err)
						}
					case "expired":
						now = first.ExpiresAt.Add(time.Second)
					case "superseded":
						now = first.ExpiresAt.Add(time.Second)
						b, err := m.Acquire(ctx, n, fenceOwner, "mock", "plan", time.Hour, 1)
						if err != nil {
							t.Fatal(err)
						}
						if b.Lease.ClaimID == first.ClaimID || b.Lease.Owner != first.Owner || b.Lease.Status != "active" || !b.Lease.ExpiresAt.After(now) {
							t.Fatalf("invalid successor: %+v", b.Lease)
						}
					}

					// Pending decomposition blocks reacquisition. Prepare the
					// operation under B, then attempt its write using stale A.
					if row.name == "AdvanceDecomposition" && lifecycle == "superseded" {
						st, err := m.GetCurrentState(ctx, n)
						if err != nil {
							t.Fatal(err)
						}
						st, err = m.BeginDecomposition(ctx, n, st, Decomposition{})
						if err != nil {
							t.Fatal(err)
						}
						f.decomposition = st.Decomposition.ID
					}
					m.BindClaim(n, first.ClaimID)
					before, revision, err := m.Store().Load(ctx, n)
					if err != nil {
						t.Fatal(err)
					}
					var old IssueState
					if err := unmarshalCoordinatorState(before, &old); err != nil {
						t.Fatal(err)
					}
					if lifecycle == "superseded" && (old.Lease.ClaimID == first.ClaimID || old.Lease.Status != "active") {
						t.Fatal("successor missing before call")
					}
					if err := row.call(ctx, f); !errors.Is(err, ErrClaimFence) {
						t.Fatalf("stale write error = %v; want ErrClaimFence", err)
					}
					after, next, err := m.Store().Load(ctx, n)
					if err != nil {
						t.Fatal(err)
					}
					var current IssueState
					if err := unmarshalCoordinatorState(after, &current); err != nil {
						t.Fatal(err)
					}
					if revision == "" || revision != next || !bytes.Equal(before, after) || !reflect.DeepEqual(old, current) {
						t.Fatal("refused write changed authoritative state")
					}
					if current.Lease.ClaimID != old.Lease.ClaimID || current.Lease.Owner != old.Lease.Owner || current.Lease.Status != old.Lease.Status || !current.Lease.ExpiresAt.Equal(old.Lease.ExpiresAt) {
						t.Fatal("refused write changed lease")
					}
				})
			}
		})
	}
}

func TestIssue470ReusedProvenanceCannotAuthorizeWrite(t *testing.T) {
	f := newClaimFenceFixture(t, "RecordTrustedValidation", "superseded")
	a := f.state.Lease
	now := a.ExpiresAt.Add(time.Second)
	f.m.SetNowFunc(func() time.Time { return now })
	b, err := f.m.Acquire(t.Context(), f.n, fenceOwner, "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.m.BindClaim(f.n, a.ClaimID)
	before, revision, err := f.m.Store().Load(t.Context(), f.n)
	if err != nil {
		t.Fatal(err)
	}
	art := contract.Artifact{Type: contract.ArtifactValidation, Verdict: "PASS", Metadata: map[string]string{
		"validation_source_claim": a.ClaimID, "validation_claim": b.Lease.ClaimID, "validation_trusted_claim": b.Lease.ClaimID, "validation_reused": "true",
	}}
	if _, err := f.m.RecordTrustedValidation(t.Context(), f.n, fenceOwner, art); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("provenance authorized stale writer: %v", err)
	}
	after, next, err := f.m.Store().Load(t.Context(), f.n)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || revision != next {
		t.Fatal("stale writer changed CAS")
	}
}
