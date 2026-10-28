package lease

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/contract"
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
