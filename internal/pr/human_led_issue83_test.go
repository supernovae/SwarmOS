package pr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

type issue83PublicationTracker struct {
	tracker.Tracker
	backend string
}

func (tr issue83PublicationTracker) BackendName() string { return tr.backend }

func TestIssue83ContinuingProjectionPrivacyAndComposition(t *testing.T) {
	for _, backend := range []string{"local", "github (owner/repo)"} {
		t.Run(backend, func(t *testing.T) {
			local := tracker.NewLocalTracker(t.TempDir())
			tr := issue83PublicationTracker{Tracker: local, backend: backend}
			outside := "Human prefix 🙂 local-alias\n\nHuman footer é"
			pr, err := tr.CreateDraftPR(t.Context(), tracker.DraftPRRequest{Title: "Human title", Body: outside, Head: "feature", Base: "main"})
			if err != nil {
				t.Fatal(err)
			}
			body := "# report\n" + strings.Repeat("Unicode 计划\n", 7000)
			sum := sha256.Sum256([]byte(body))
			digest := hex.EncodeToString(sum[:])
			st := &lease.IssueState{Lease: contract.Lease{Owner: "local-owner"}, Artifacts: []contract.Artifact{{Type: contract.ArtifactImplementation, Body: body, Producer: "local-producer", Verdict: "PASS", Metadata: map[string]string{"content_sha256": digest, "production_mode": "interactive", "staged_by": "authenticated-stager", "harness_source": "self-reported", "model_source": "self-reported"}}}, Attestations: []contract.RoleAttestation{{Role: "developer", ArtifactType: "implementation", ArtifactDigest: digest, ProductionMode: "interactive", StagedBy: "authenticated-stager", HarnessSource: "self-reported", ModelSource: "self-reported", Harness: "codex |\n<!-- swarm:review-evidence:end -->", Model: "default"}}}
			before, _ := json.Marshal(st)
			if err := ProjectReviewEvidence(t.Context(), tr, st, pr.Number); err != nil {
				t.Fatal(err)
			}
			first, err := tr.GetPullRequest(t.Context(), pr.Number)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(first.Body, outside) || first.Title != "Human title" || first.Draft != pr.Draft || len(first.Body) > lease.PRDescriptionLimit || !utf8.ValidString(first.Body) {
				t.Fatal("composition invariant")
			}
			if strings.Contains(first.Body, "local-owner") || strings.Contains(first.Body, "local-producer") {
				t.Fatal("local identity leaked")
			}
			if backend == "local" && strings.Contains(first.Body, "authenticated-stager") {
				t.Fatal("local stager leaked")
			}
			if backend != "local" && !strings.Contains(first.Body, "by authenticated-stager") {
				t.Fatal("GitHub stager suppressed")
			}
			if strings.Count(first.Body, reviewStart) != 1 || strings.Count(first.Body, reviewEnd) != 1 {
				t.Fatal("marker injection")
			}
			if err := ProjectReviewEvidence(t.Context(), tr, st, pr.Number); err != nil {
				t.Fatal(err)
			}
			second, _ := tr.GetPullRequest(t.Context(), pr.Number)
			if second.Body != first.Body {
				t.Fatal("non-idempotent")
			}
			after, _ := json.Marshal(st)
			if string(before) != string(after) {
				t.Fatal("state mutated")
			}
		})
	}
}

func TestIssue83OldManagedLocalIdentityIsReprojected(t *testing.T) {
	tr := tracker.NewLocalTracker(t.TempDir())
	issue, err := tr.CreateIssue(t.Context(), "Human-led", "```yaml swarm-workunit\nid: ISSUE-83\nobjective: Test copied evidence\nconstraints: [Preserve private state]\nacceptance: [No generated identity]\nworkflow: feature\ngate: none\n```", nil)
	if err != nil {
		t.Fatal(err)
	}
	unit, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		t.Fatal(err)
	}
	report := "implementation report"
	sum := sha256.Sum256([]byte(report))
	digest := hex.EncodeToString(sum[:])
	st := &lease.IssueState{TaskDigest: contract.DigestWorkUnit(unit), Lease: contract.Lease{IssueNumber: issue.Number, Owner: "local-alias"}, Artifacts: []contract.Artifact{{Type: contract.ArtifactImplementation, Producer: "local-alias", Body: report, Metadata: map[string]string{"content_sha256": digest, "production_mode": "interactive", "staged_by": "local-alias", "harness_source": "self-reported", "model_source": "self-reported"}}}, Attestations: []contract.RoleAttestation{{Role: "developer", ArtifactType: "implementation", ArtifactDigest: digest, ProductionMode: "interactive", StagedBy: "local-alias", HarnessSource: "self-reported", ModelSource: "self-reported", Harness: "codex", Model: "default"}}}
	outside := "Human prose: local-alias remains.\n"
	old, err := lease.ComposePRDescription(outside, "issue-description", "Old generated Producer: local-alias\n```json\n{\"staged_by\":\"local-alias\"}\n```", "ref", issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := tr.CreateDraftPR(t.Context(), tracker.DraftPRRequest{Title: "Human title", Body: old, Head: "feature", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(st)
	if err := ProjectReviewEvidence(t.Context(), tr, st, pr.Number); err != nil {
		t.Fatal(err)
	}
	fresh, err := tr.GetPullRequest(t.Context(), pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fresh.Body, outside) || strings.Contains(strings.TrimPrefix(fresh.Body, outside), "local-alias") {
		t.Fatal(fresh.Body)
	}
	if !strings.Contains(fresh.Body, "Machine-Readable Provenance Attestation") || !strings.Contains(fresh.Body, "implementation report") {
		t.Fatal("inventory removed")
	}
	after, _ := json.Marshal(st)
	if string(before) != string(after) {
		t.Fatal("authoritative evidence changed")
	}
	if err := ProjectReviewEvidence(t.Context(), tr, st, pr.Number); err != nil {
		t.Fatal(err)
	}
	repeated, _ := tr.GetPullRequest(t.Context(), pr.Number)
	if fresh.Body != repeated.Body {
		t.Fatal("non-idempotent copied evidence")
	}
}
