package lease

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
)

type issue83Tracker struct {
	tracker.Tracker
	actor    string
	actorErr error
	backend  string
}

func TestIssue83HistoricalProjectionFields(t *testing.T) {
	unit := &contract.WorkUnit{IssueNumber: 83, Objective: "Human-authored local-alias remains unchanged"}
	state := &IssueState{}
	safe := descriptionMarker("issue-description", "start") + "\n<!-- swarm:description:v1 -->\n" + DescriptionIssueSection(unit, state, "ref", ProjectionContext{}) + descriptionMarker("issue-description", "end")
	if DescriptionIssueNeedsProjection(safe) {
		t.Fatal("anonymous historical evidence must survive revision")
	}
	for name, body := range map[string]string{
		"legacy":          "<!-- swarm:issue-description:start -->old generated identity<!-- swarm:issue-description:end -->",
		"named summary":   strings.Replace(safe, "- planner: unknown", "- planner: staged interactively by private-alias", 1),
		"named inventory": strings.Replace(safe, "## 📦 Attached SwarmOS Artifacts", "Human review by private-alias\n## 📦 Attached SwarmOS Artifacts", 1),
		"producer":        strings.Replace(safe, "## 📦 Attached SwarmOS Artifacts", "Producer: private-alias\n## 📦 Attached SwarmOS Artifacts", 1),
		"invalid JSON":    strings.Replace(safe, "```json\n", "```json\ninvalid", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if !DescriptionIssueNeedsProjection(body) {
				t.Fatal("historical identity or unknown evidence was retained")
			}
		})
	}
	state.Attestations = []contract.RoleAttestation{{Role: "developer", StagedBy: "private-alias"}}
	raw, _ := json.MarshalIndent(BuildProvenanceAttestation(unit, state), "", "  ")
	start := strings.Index(safe, "```json\n") + len("```json\n")
	end := start + strings.Index(safe[start:], "\n```")
	if !DescriptionIssueNeedsProjection(safe[:start] + string(raw) + safe[end:]) {
		t.Fatal("raw historical actor was retained")
	}
}

func (tr issue83Tracker) CurrentUser(context.Context) (string, error) { return tr.actor, tr.actorErr }
func (tr issue83Tracker) BackendName() string                         { return tr.backend }

func TestIssue83FencedInteractiveActorAndPublicProjection(t *testing.T) {
	for _, backend := range []string{"local", "github (owner/repo)"} {
		t.Run(backend, func(t *testing.T) {
			m, lt, iss := testManager(t)
			ctx := t.Context()
			claim, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			m.BindClaim(iss.Number, claim.Lease.ClaimID)
			actor := "distinct-private-alias"
			if backend != "local" {
				actor = "authenticated-github-login"
			}
			m.tracker = issue83Tracker{Tracker: lt, actor: actor, backend: backend}
			for _, mode := range []string{"", "headless", "interactive"} {
				for _, typ := range []contract.ArtifactType{contract.ArtifactDesign, contract.ArtifactTestPlan, contract.ArtifactReview, contract.ArtifactImplementation, contract.ArtifactValidation} {
					art := contract.Artifact{Type: typ, Producer: "forged-producer", Summary: "staged report", Verdict: "PASS", Body: "body for " + string(typ), Metadata: map[string]string{"production_mode": mode, "staged_by": "forged-login", "harness_source": "verified", "model_source": "verified", "role": artifactRole(typ), "harness": "codex", "model": "default", "validation_trusted_owner": "forged-login", "implementation_trusted_owner": "forged-login"}}
					var st *IssueState
					var err error
					if typ == contract.ArtifactReview {
						art.Verdict = "APPROVED"
						art.Body = "VERDICT: APPROVED\n\nReview"
						art.Metadata["kind"] = "agent"
						art.Metadata["actor"] = "forged-login"
						st, err = m.UpdateAdvisoryReview(ctx, iss.Number, "local-dev", art)
					} else {
						st, err = m.UpdateArtifact(ctx, iss.Number, "local-dev", art)
					}
					if err != nil {
						t.Fatal(typ, err)
					}
					var saved contract.Artifact
					for _, a := range st.Artifacts {
						if a.Type == typ {
							saved = a
						}
					}
					if saved.Metadata["validation_trusted_owner"] != "" || saved.Metadata["implementation_trusted_owner"] != "" {
						t.Fatal(saved.Metadata)
					}
					if mode == "interactive" {
						if saved.Metadata["staged_by"] != actor || saved.Metadata["harness_source"] != "self-reported" || saved.Metadata["model_source"] != "self-reported" {
							t.Fatal(saved.Metadata)
						}
					} else if saved.Metadata["staged_by"] != "" || saved.Metadata["harness_source"] != "" || saved.Metadata["model_source"] != "" {
						t.Fatal(saved.Metadata)
					}
					before, _ := json.Marshal(st)
					projection := PublicProjectionContext(m.tracker)
					raw, _ := json.Marshal(PublicProvenanceAttestation(nil, st, projection))
					rendered := DescriptionProductionSummary(st, projection) + DescriptionArtifacts(st, projection) + string(raw)
					if strings.Contains(rendered, "forged-login") || strings.Contains(rendered, "forged-producer") {
						t.Fatal(rendered)
					}
					if backend == "local" && strings.Contains(rendered, actor) {
						t.Fatal(rendered)
					}
					if backend != "local" && mode == "interactive" && (!strings.Contains(rendered, "staged interactively by "+actor) || !strings.Contains(string(raw), `"staged_by":"`+actor+`"`)) {
						t.Fatal(rendered)
					}
					if strings.Contains(DescriptionProductionSummary(st)+DescriptionArtifacts(st), actor) {
						t.Fatal("unsafe default")
					}
					after, _ := json.Marshal(st)
					if string(before) != string(after) {
						t.Fatal("projection mutated state")
					}
				}
			}
			for _, broken := range []issue83Tracker{{Tracker: lt, backend: backend}, {Tracker: lt, backend: backend, actorErr: errors.New("lookup failed")}} {
				before, err := m.GetCurrentState(ctx, iss.Number)
				if err != nil {
					t.Fatal(err)
				}
				m.tracker = broken
				_, err = m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Body: "must not persist", Metadata: map[string]string{"production_mode": "interactive"}})
				if err == nil {
					t.Fatal("missing identity accepted")
				}
				after, err := m.GetCurrentState(ctx, iss.Number)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before.Artifacts, after.Artifacts) {
					t.Fatal("failed authentication wrote artifacts")
				}
			}
			m.tracker = issue83Tracker{Tracker: lt, actor: actor, backend: backend}
			m.BindClaim(iss.Number, "stale-token")
			_, err = m.UpdateArtifact(ctx, iss.Number, "local-dev", contract.Artifact{Type: contract.ArtifactDesign, Body: "stale", Metadata: map[string]string{"production_mode": "interactive"}})
			if !errors.Is(err, ErrClaimFence) {
				t.Fatalf("fence: %v", err)
			}
		})
	}
}

func TestIssue83ReplacementAndValidationReuse(t *testing.T) {
	st := &IssueState{}
	old := contract.RoleAttestation{Role: "tester", ProductionMode: "interactive", StagedBy: "old-stager", HarnessSource: "self-reported", ModelSource: "self-reported", Harness: "codex", Model: "default", ArtifactType: "validation", ArtifactDigest: "digest"}
	RecordRoleAttestation(st, old)
	fresh := contract.RoleAttestation{Role: "tester", Agent: "configured", Harness: "codex", ExecutionMode: "isolated", Authentication: "none"}
	RecordRoleAttestation(st, fresh)
	got := st.Attestations[0]
	if got.StagedBy != "" || got.ProductionMode != "" || got.HarnessSource != "" || got.ModelSource != "" || got.ArtifactDigest != "digest" || got.ArtifactType != "validation" {
		t.Fatal(got)
	}
	base := old.Metadata(map[string]string{"execution_mode": "isolated", "authentication": "none", "validation_claim": "current"})
	original := map[string]string{}
	for k, v := range base {
		original[k] = v
	}
	for _, att := range []contract.RoleAttestation{old, fresh} {
		st.Attestations = []contract.RoleAttestation{att}
		md := TesterMetadata(st, base)
		for _, key := range []string{"production_mode", "staged_by", "harness_source", "model_source", "execution_mode", "authentication"} {
			if _, ok := md[key]; ok {
				t.Fatal(key, md)
			}
		}
		if md["harness"] != "codex" || md["validation_claim"] != "current" {
			t.Fatal(md)
		}
	}
	if !reflect.DeepEqual(base, original) {
		t.Fatal("base mutated")
	}
}

func TestIssue83RoleEvidenceAndPrivacy(t *testing.T) {
	body := "current report"
	digest := hash(body)
	st := &IssueState{Lease: contract.Lease{Owner: "private-owner"}, Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: body, Metadata: map[string]string{"content_sha256": digest, "production_mode": "interactive", "staged_by": "private-stager", "harness_source": "self-reported", "model_source": "self-reported"}}}, Attestations: []contract.RoleAttestation{{Role: "planner", ArtifactType: "design", ArtifactDigest: digest, ProductionMode: "interactive", StagedBy: "private-stager", HarnessSource: "self-reported", ModelSource: "self-reported", Harness: "codex", Model: "default"}}}
	if !strings.Contains(RoleProductionSummary(st, "planner"), "private-stager") {
		t.Fatal("private status lost actor")
	}
	if got := DescriptionProductionSummary(st); strings.Contains(got, "private-") || !strings.Contains(got, "staged interactively;") {
		t.Fatal(got)
	}
	st.Artifacts[0].Body = "replaced"
	if got := RoleProductionSummary(st, "planner"); got != "unknown" {
		t.Fatal(got)
	}
	raw, _ := json.Marshal(PublicProvenanceAttestation(nil, st, PublicProjectionContext(issue83Tracker{backend: "github (owner/repo)"})))
	if strings.Contains(string(raw), "private-stager") {
		t.Fatal("stale binding published actor", string(raw))
	}
}

func TestIssue83FreshAndReusedTesterEvidence(t *testing.T) {
	m, _, iss := testManager(t)
	st, err := m.Acquire(t.Context(), iss.Number, "local-dev", "mock", "implement", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	claim := st.Lease.ClaimID
	// Configured RunResult attestations enter the manager as Metadata; standalone
	// coordinator validation deliberately goes through TesterMetadata instead.
	configured := contract.RoleAttestation{Role: "tester", Agent: "configured-agent", Harness: "codex", Model: "default", ExecutionMode: contract.ExecutionIsolated, Authentication: "none", Sandbox: "codex-native"}
	art := contract.Artifact{Type: contract.ArtifactValidation, Producer: "validator", Summary: "validation", Verdict: "PASS", Body: "Current validation", Metadata: configured.Metadata(map[string]string{"base_sha": st.BaseSHA, "head_sha": "current-head", "validation_base": st.BaseSHA, "validation_tree": "current-tree", "validation_policy": "policy", "validation_claim": claim})}
	st, err = m.RecordTrustedValidation(t.Context(), iss.Number, "local-dev", art)
	if err != nil {
		t.Fatal(err)
	}
	// Rendering needs the current recorded checkpoint. The manager's authority
	// and retry semantics are untouched by this descriptive fixture.
	st.HeadSHA = "current-head"
	if got := RoleProductionSummary(st, "tester"); !strings.Contains(got, "headless agent run by local-dev; configured agent configured-agent") {
		t.Fatal(got)
	}
	for _, actorAllowed := range []bool{false, true} {
		ctx := ProjectionContext{publishActors: actorAllowed}
		text := DescriptionProductionSummary(st, ctx)
		if strings.Contains(text, "local-dev") != actorAllowed {
			t.Fatal(text)
		}
	}
	art.Metadata = TesterMetadata(st, art.Metadata)
	reused, err := m.RecordTrustedValidation(t.Context(), iss.Number, "local-dev", art)
	if err != nil {
		t.Fatal(err)
	}
	reused.HeadSHA = "current-head"
	if got := RoleProductionSummary(reused, "tester"); !strings.Contains(got, "coordinator validation by local-dev; prior runner provenance reused") {
		t.Fatal(got)
	}
	if got := DescriptionProductionSummary(reused); strings.Contains(got, "local-dev") || !strings.Contains(got, "coordinator validation;") {
		t.Fatal(got)
	}
	for _, key := range []string{"validation_trusted_origin", "validation_trusted_owner", "validation_trusted_claim", "validation_claim", "validation_base", "base_sha", "head_sha", "validation_tree", "validation_policy", "content_sha256"} {
		t.Run(key, func(t *testing.T) {
			copy := *reused
			copy.Artifacts = append([]contract.Artifact(nil), reused.Artifacts...)
			for i := range copy.Artifacts {
				if copy.Artifacts[i].Type == contract.ArtifactValidation {
					md := map[string]string{}
					for k, v := range copy.Artifacts[i].Metadata {
						md[k] = v
					}
					delete(md, key)
					copy.Artifacts[i].Metadata = md
				}
			}
			if got := RoleProductionSummary(&copy, "tester"); got != "unknown" {
				t.Fatal(key, got)
			}
		})
	}
	noRunner := *reused
	noRunner.Attestations = nil
	if got := RoleProductionSummary(&noRunner, "tester"); got != "coordinator validation by local-dev; unknown prior runner" {
		t.Fatal(got)
	}
	forkWithStamp := *reused
	forkWithStamp.ForkRepo = "contributor/repo"
	if got := RoleProductionSummary(&forkWithStamp, "tester"); got != "unknown" {
		t.Fatal(got)
	}
	// Fork standalone validation has no trusted coordinator origin.
	fork := *reused
	fork.Artifacts = append([]contract.Artifact(nil), reused.Artifacts...)
	for i := range fork.Artifacts {
		md := map[string]string{}
		for k, v := range fork.Artifacts[i].Metadata {
			if !strings.HasPrefix(k, "validation_trusted_") {
				md[k] = v
			}
		}
		fork.Artifacts[i].Metadata = md
	}
	if got := RoleProductionSummary(&fork, "tester"); got != "unknown" {
		t.Fatal(got)
	}
}

func TestIssue83HumanReviewWithoutRoleAttestation(t *testing.T) {
	body := "VERDICT: APPROVED\n\nPrivate review notes"
	st := &IssueState{BaseSHA: "base", TaskDigest: "task", PolicyDigest: "policy", Lease: contract.Lease{Owner: "claim-owner", ClaimID: "claim"}, Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan"}, {Type: contract.ArtifactTestPlan, Body: "tests"}}}
	md := ReviewBindings(st)
	md["kind"] = "human"
	md["actor"] = "authenticated-reviewer"
	md["review_source"] = "authenticated-human"
	md["content_sha256"] = hash(body)
	st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactReview, Producer: "human:authenticated-reviewer", Verdict: "APPROVED", Body: body, Metadata: md})
	before, _ := json.Marshal(st)
	if got := RoleProductionSummary(st, "reviewer"); got != "human review by authenticated-reviewer" {
		t.Fatal(got)
	}
	if got := DescriptionProductionSummary(st) + DescriptionArtifacts(st); strings.Contains(got, "authenticated-reviewer") || !strings.Contains(got, "human review") {
		t.Fatal(got)
	}
	if got := DescriptionProductionSummary(st, ProjectionContext{publishActors: true}); !strings.Contains(got, "human review by authenticated-reviewer") {
		t.Fatal(got)
	}
	after, _ := json.Marshal(st)
	if string(before) != string(after) {
		t.Fatal("render mutated state")
	}
	st.Artifacts[0].Body = "changed plan"
	if got := RoleProductionSummary(st, "reviewer"); got != "unknown" {
		t.Fatal(got)
	}
}

func TestIssue83ReviewerBoundaryNeedsCurrentEvidence(t *testing.T) {
	body := "VERDICT: APPROVED\n\nAgent review"
	st := &IssueState{BaseSHA: "base", TaskDigest: "task", PolicyDigest: "policy", Lease: contract.Lease{Owner: "authoritative-owner", ClaimID: "claim"}, Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan"}, {Type: contract.ArtifactTestPlan, Body: "tests"}}}
	md := ReviewBindings(st)
	md["kind"] = "agent"
	md["actor"] = "authenticated-reviewer"
	md["review_source"] = "authenticated-agent"
	md["content_sha256"] = hash(body)
	md["agent"] = "configured-reviewer"
	md["harness"] = "codex"
	md["sandbox"] = "bwrap"
	md["execution_mode"] = contract.ExecutionIsolated
	st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactReview, Producer: "forged-producer", Verdict: "APPROVED", Body: body, Metadata: md})
	st.Attestations = []contract.RoleAttestation{{Role: "reviewer", ArtifactType: "review", ArtifactDigest: hash(body), Agent: "configured-reviewer", Harness: "codex", Sandbox: "bwrap", ExecutionMode: contract.ExecutionIsolated}}
	if got := RoleProductionSummary(st, "reviewer"); !strings.Contains(got, "headless agent run by authoritative-owner") || !strings.Contains(got, "sandboxed reviewer (bwrap)") || strings.Contains(got, "forged-producer") {
		t.Fatal(got)
	}
	for _, key := range []string{"sandbox", "review_source", "plan_digest"} {
		old := md[key]
		delete(md, key)
		if got := RoleProductionSummary(st, "reviewer"); strings.Contains(got, "sandboxed reviewer") {
			t.Fatal(key, got)
		}
		md[key] = old
	}
	st.Artifacts[len(st.Artifacts)-1].Body += "changed"
	if got := RoleProductionSummary(st, "reviewer"); got != "unknown" {
		t.Fatal(got)
	}
}

func TestIssue83AttributionStrippedBeforeGateway(t *testing.T) {
	for _, mode := range []string{"", "headless", "interactive"} {
		for _, typ := range []contract.ArtifactType{contract.ArtifactDesign, contract.ArtifactTestPlan, contract.ArtifactReview, contract.ArtifactImplementation, contract.ArtifactValidation} {
			gateway := &validationGateway{}
			m := NewManager(issue83Tracker{actorErr: errors.New("worker must not authenticate stager")}, contract.SecurityConfig{})
			m.SetWorkerGateway(gateway)
			md := map[string]string{"production_mode": mode, "staged_by": "forged-stager", "harness_source": "authenticated", "model_source": "authenticated", "validation_trusted_origin": "coordinator", "implementation_trusted_origin": "coordinator", "harness": "codex"}
			if _, err := m.UpdateArtifact(t.Context(), 83, "worker", contract.Artifact{Type: typ, Metadata: md}); err != nil {
				t.Fatal(err)
			}
			if gateway.artifact == nil {
				t.Fatal("not forwarded")
			}
			for _, key := range []string{"staged_by", "harness_source", "model_source", "validation_trusted_origin", "implementation_trusted_origin"} {
				if _, ok := gateway.artifact.Metadata[key]; ok {
					t.Fatal(mode, typ, key)
				}
			}
			if gateway.artifact.Metadata["production_mode"] != mode || gateway.artifact.Metadata["harness"] != "codex" || md["staged_by"] != "forged-stager" {
				t.Fatal("declaration lost or source mutated")
			}
		}
	}
}

func TestIssue83RelatedSectionsComposeAtomically(t *testing.T) {
	outside := "Human prefix é🙂\n"
	huge := strings.Repeat("Unicode 计划🙂\n", 10000)
	body, err := ComposePRDescription(outside, "issue-description", DescriptionSlot(huge), "ref", 83)
	if err != nil {
		t.Fatal(err)
	}
	body, err = ComposePRDescription(body, "review-evidence", strings.Repeat(DescriptionSlot(huge), 12), "ref", 83)
	if err != nil {
		t.Fatal(err)
	}
	body, err = ComposePRDescription(body, "rebase", DescriptionSlot(huge), "ref", 83)
	if err != nil {
		t.Fatal(err)
	}
	replacements := map[string]string{"issue-description": "Current inventory\n" + strings.Repeat(DescriptionSlot(huge), 5), "review-evidence": "Current review\n" + DescriptionSlot(huge)}
	first, err := ComposePRDescriptionSections(body, replacements, "ref", 83)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ComposePRDescriptionSections(first, replacements, "ref", 83)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("related section updates progressively truncated evidence")
	}
	if !strings.HasPrefix(first, outside) || len(first) > PRDescriptionLimit {
		t.Fatal("body invariant")
	}
	for _, name := range descriptionNames {
		if strings.Count(first, descriptionMarker(name, "start")) != 1 || strings.Count(first, descriptionMarker(name, "end")) != 1 {
			t.Fatal("marker invariant", name)
		}
	}
}
