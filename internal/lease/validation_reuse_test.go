package lease

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"testing"
	"time"
)

func TestTrustedValidationCannotBeImported(t *testing.T) {
	t.Parallel()
	m, _, issue := testManager(t)
	ctx := context.Background()
	st, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "implement", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	art := contract.Artifact{Type: contract.ArtifactValidation, Producer: "swarm:validator", Verdict: "PASS", Summary: "passed", Body: "evidence", Metadata: map[string]string{"validation_trusted_origin": "coordinator", "validation_trusted_owner": "local-dev", "validation_trusted_claim": st.Lease.ClaimID}}
	imported, err := m.UpdateArtifact(ctx, issue.Number, "local-dev", art)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range imported.Artifacts {
		if a.Type == contract.ArtifactValidation && a.Metadata["validation_trusted_origin"] != "" {
			t.Fatal("import gained authority")
		}
	}
	trusted, err := m.RecordTrustedValidation(ctx, issue.Number, "local-dev", art)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range trusted.Artifacts {
		if a.Type == contract.ArtifactValidation && (a.Metadata["validation_trusted_origin"] != "coordinator" || a.Metadata["validation_trusted_claim"] != st.Lease.ClaimID) {
			t.Fatal("trusted recording missing binding")
		}
	}
	m.BindClaim(issue.Number, "stale")
	if _, err := m.RecordTrustedValidation(ctx, issue.Number, "local-dev", art); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("stale trusted writer: %v", err)
	}
}

type validationGateway struct{ artifact *contract.Artifact }

func (g *validationGateway) Apply(_ context.Context, r ForkRequest) (*IssueState, error) {
	g.artifact = r.Artifact
	return &IssueState{}, nil
}

func TestGatewayValidationCannotGainReuseAuthority(t *testing.T) {
	t.Parallel()
	m, _, issue := testManager(t)
	gateway := &validationGateway{}
	m.SetWorkerGateway(gateway)
	_, err := m.RecordTrustedValidation(context.Background(), issue.Number, "local-dev", contract.Artifact{Type: contract.ArtifactValidation, Metadata: map[string]string{"validation_trusted_origin": "coordinator", "validation_trusted_claim": "copied"}})
	if err != nil {
		t.Fatal(err)
	}
	if gateway.artifact == nil || gateway.artifact.Metadata["validation_trusted_origin"] != "" || gateway.artifact.Metadata["validation_trusted_claim"] != "" {
		t.Fatal("gateway retained reuse authority")
	}
}

func TestTrustedImplementationAdmission(t *testing.T) {
	m, _, issue := testManager(t)
	ctx := t.Context()
	_, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "implement", time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.mutate(ctx, issue.Number, "local-dev", func(st *IssueState) error {
		st.EffectiveGate = contract.GateNone
		st.HeadSHA = st.BaseSHA
		st.Branch = "retry"
		st.Artifacts = []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan"}, {Type: contract.ArtifactTestPlan, Body: "test plan"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	md := ReviewBindings(st)
	md["head_sha"] = st.HeadSHA
	md["implementation_trusted_unknown"] = "forged"
	art := contract.Artifact{Type: contract.ArtifactImplementation, Producer: "mock:developer", Verdict: "PASS", Summary: "implemented", Body: "implementation", Metadata: md}
	imported, err := m.UpdateArtifact(ctx, issue.Number, "local-dev", art)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := m.implementationTree(ctx, st.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyTrustedImplementation(imported, st.HeadSHA, tree, st.Branch) == nil {
		t.Fatal("generic PASS gained authority")
	}
	for _, a := range imported.Artifacts {
		if a.Metadata["implementation_trusted_unknown"] != "" {
			t.Fatal("reserved namespace retained")
		}
	}
	for _, tc := range []string{"type", "fail", "head", "bindings"} {
		bad := art
		bad.Metadata = map[string]string{}
		for k, v := range art.Metadata {
			bad.Metadata[k] = v
		}
		switch tc {
		case "type":
			bad.Type = contract.ArtifactValidation
		case "fail":
			bad.Verdict = "FAIL"
		case "head":
			bad.Metadata["head_sha"] = "wrong"
		case "bindings":
			bad.Metadata["plan_digest"] = "wrong"
		}
		if _, err := m.RecordTrustedImplementation(ctx, issue.Number, "local-dev", bad); err == nil {
			t.Fatalf("accepted %s", tc)
		}
	}
	trusted, err := m.RecordTrustedImplementation(ctx, issue.Number, "local-dev", art)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyTrustedImplementation(trusted, st.HeadSHA, tree, st.Branch); err != nil {
		t.Fatal(err)
	}
	original := trusted.Lease.ClaimID
	// Model an independently fenced successor without changing producing evidence.
	trusted, err = m.mutate(ctx, issue.Number, "local-dev", func(st *IssueState) error { st.Lease.ClaimID = "successor"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CheckImplementationRetry(ctx, issue.Number, "local-dev", st.HeadSHA, tree, st.Branch); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("stale read: %v", err)
	}
	m.BindClaim(issue.Number, "successor")
	if _, err := m.CheckImplementationRetry(ctx, issue.Number, "local-dev", st.HeadSHA, tree, st.Branch); err != nil {
		t.Fatal(err)
	}
	for _, a := range trusted.Artifacts {
		if a.Type == contract.ArtifactImplementation && a.Metadata["implementation_trusted_claim"] != original {
			t.Fatal("producing claim rewritten")
		}
	}
	report := contract.Artifact{Type: contract.ArtifactValidation, Producer: "swarm:validator", Verdict: "PASS", Summary: "checks passed", Body: "trusted checks", Metadata: map[string]string{"base_sha": st.BaseSHA, "head_sha": st.HeadSHA, "validation_tree": tree, "validation_claim": original, "validation_base": st.BaseSHA}}
	if _, err := m.RecordRetryValidation(ctx, issue.Number, "local-dev", report, st.HeadSHA, tree, st.Branch); err == nil {
		t.Fatal("old claim report accepted")
	}
	report.Metadata["validation_claim"] = "successor"
	if _, err := m.RecordRetryValidation(ctx, issue.Number, "local-dev", report, st.HeadSHA, tree, st.Branch); err != nil {
		t.Fatal(err)
	}
	originalStore := m.store
	m.store = &retryCASStore{Store: originalStore}
	if _, err := m.RecordRetryValidation(ctx, issue.Number, "local-dev", report, st.HeadSHA, tree, st.Branch); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("CAS successor: %v", err)
	}
	for _, field := range []string{"task_digest", "policy_digest", "base_sha", "plan_digest", "test_plan_digest", "branch", "head", "tree", "revision", "claim", "owner", "review_claim_id"} {
		copyState := *trusted
		copyState.Artifacts = append([]contract.Artifact(nil), trusted.Artifacts...)
		for i, a := range copyState.Artifacts {
			if a.Type != contract.ArtifactImplementation {
				continue
			}
			copyState.Artifacts[i].Metadata = map[string]string{}
			for k, v := range a.Metadata {
				copyState.Artifacts[i].Metadata[k] = v
			}
			copyState.Artifacts[i].Metadata["implementation_trusted_"+field] = ""
		}
		if VerifyTrustedImplementation(&copyState, st.HeadSHA, tree, st.Branch) == nil {
			t.Fatalf("accepted missing %s", field)
		}
	}
}

func TestImplementationNamespaceStrippedBeforeGateway(t *testing.T) {
	m, _, issue := testManager(t)
	g := &validationGateway{}
	m.SetWorkerGateway(g)
	art := contract.Artifact{Type: contract.ArtifactImplementation, Metadata: map[string]string{"implementation_trusted_origin": "coordinator", "implementation_trusted_future": "forged", "validation_trusted_future": "forged", "ordinary": "kept"}}
	if _, err := m.UpdateArtifact(t.Context(), issue.Number, "local-dev", art); err != nil {
		t.Fatal(err)
	}
	if g.artifact == nil || len(g.artifact.Metadata) != 1 || g.artifact.Metadata["ordinary"] != "kept" {
		t.Fatalf("gateway metadata: %+v", g.artifact)
	}
	g.artifact = nil
	if _, err := m.RecordTrustedImplementation(t.Context(), issue.Number, "local-dev", art); err == nil || g.artifact != nil {
		t.Fatal("trusted recorder dispatched gateway operation")
	}
}

func TestRetryEligibilityRejectsChangedAuthoritativeBindings(t *testing.T) {
	st := &IssueState{BaseSHA: "base", TaskDigest: "task", PolicyDigest: "policy", PolicyTreeDigest: "policy-tree", EffectiveGate: contract.GateNone, HeadSHA: "head", Branch: "branch", Lease: contract.Lease{IssueNumber: 1, ClaimID: "producer"},
		Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan"}, {Type: contract.ArtifactTestPlan, Body: "tests"}}}
	md := map[string]string{"implementation_trusted_issue": "1", "implementation_trusted_origin": "coordinator", "implementation_trusted_owner": "owner", "implementation_trusted_claim": "producer", "implementation_trusted_head": "head", "implementation_trusted_tree": "tree", "implementation_trusted_branch": "branch", "implementation_trusted_revision": "issue", "content_sha256": hash("implementation")}
	for k, v := range ReviewBindings(st) {
		md["implementation_trusted_"+k] = v
	}
	st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactImplementation, Verdict: "PASS", Body: "implementation", Metadata: md})
	if err := VerifyTrustedImplementation(st, "head", "tree", "branch"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*IssueState)
	}{
		{"task", func(s *IssueState) { s.TaskDigest = "other" }},
		{"policy", func(s *IssueState) { s.PolicyDigest = "other" }},
		{"base", func(s *IssueState) { s.BaseSHA = "other" }},
		{"policy tree", func(s *IssueState) { s.PolicyTreeDigest = "other" }},
		{"plan", func(s *IssueState) { s.Artifacts[0].Body = "other" }},
		{"test plan", func(s *IssueState) { s.Artifacts[1].Body = "other" }},
		{"missing implementation", func(s *IssueState) { s.Artifacts = s.Artifacts[:2] }},
		{"failed", func(s *IssueState) { s.Artifacts[2].Verdict = "FAIL" }},
		{"body", func(s *IssueState) { s.Artifacts[2].Body = "other" }},
		{"missing gate", func(s *IssueState) { s.EffectiveGate = contract.GateAgent }},
		{"human gate", func(s *IssueState) { s.EffectiveGate = contract.GateHuman }},
		{"head", func(s *IssueState) { s.HeadSHA = "other" }},
		{"branch", func(s *IssueState) { s.Branch = "other" }},
		{"revision", func(s *IssueState) {
			s.PRRevision = &PRRevision{Number: 5, LocalHeadSHA: "head", Branch: "branch", RemoteHeadSHA: "remote"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := *st
			candidate.Artifacts = append([]contract.Artifact(nil), st.Artifacts...)
			tc.change(&candidate)
			if VerifyTrustedImplementation(&candidate, "head", "tree", "branch") == nil {
				t.Fatal("changed binding accepted")
			}
		})
	}
}

type retryCASStore struct {
	coordination.Store
	moved bool
}

func (s *retryCASStore) CompareAndSwap(ctx context.Context, n int, rev string, data []byte) (string, error) {
	if !s.moved {
		s.moved = true
		current, version, err := s.Store.Load(ctx, n)
		if err != nil {
			return "", err
		}
		var state IssueState
		if err := json.Unmarshal(current, &state); err != nil {
			return "", err
		}
		state.Lease.ClaimID = "next-successor"
		current, err = json.Marshal(&state)
		if err != nil {
			return "", err
		}
		if _, err := s.Store.CompareAndSwap(ctx, n, version, current); err != nil {
			return "", err
		}
		return "", coordination.ErrConflict
	}
	return s.Store.CompareAndSwap(ctx, n, rev, data)
}
