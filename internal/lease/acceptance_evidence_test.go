package lease

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"strings"
	"testing"
	"time"
)

func TestAcceptanceEvidenceCarryBindings(t *testing.T) {
	for _, change := range []string{"", "head_sha", "base_sha", "validation_claim", "validation_base", "validation_tree", "validation_policy", "validation_trusted_origin", "validation_trusted_owner", "validation_trusted_claim", "newer-missing", "newer-malformed", "newer-unmet"} {
		t.Run(change, func(t *testing.T) {
			metadata := map[string]string{"head_sha": "head", "base_sha": "base", "validation_claim": "claim", "validation_base": "base", "validation_tree": "tree", "validation_policy": "policy", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "owner", "validation_trusted_claim": "claim"}
			validation.StoreAcceptanceSnapshot(metadata, validation.CaptureAcceptanceEvidence("## Independent acceptance review 1\nThe original finding.\n"+validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"met","evidence":"Independent acceptance review 1"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
			dst := contract.Artifact{Body: "checks", Metadata: map[string]string{}}
			for k, v := range metadata {
				if k != validation.AcceptanceEvidenceKey {
					dst.Metadata[k] = v
				}
			}
			src := contract.Artifact{Type: contract.ArtifactValidation, Metadata: metadata}
			st := &IssueState{BaseSHA: "base", HeadSHA: "head", Lease: contract.Lease{Owner: "owner", ClaimID: "claim"}, Artifacts: []contract.Artifact{src}}
			if strings.HasPrefix(change, "newer-") {
				next := contract.Artifact{Type: contract.ArtifactValidation, Metadata: map[string]string{}}
				for k, v := range metadata {
					next.Metadata[k] = v
				}
				delete(next.Metadata, validation.AcceptanceEvidenceKey)
				if change == "newer-malformed" {
					validation.StoreAcceptanceSnapshot(next.Metadata, validation.AcceptanceSnapshot{State: "malformed"})
				}
				if change == "newer-unmet" {
					validation.StoreAcceptanceSnapshot(next.Metadata, validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"unmet"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
				}
				st.Artifacts = append(st.Artifacts, next)
			} else if change != "" {
				metadata[change] = "different"
			}
			PreserveAcceptanceEvidence(st, &dst)
			if strings.Contains(dst.Body, "The original finding.") != (change == "") {
				t.Fatal("stale or lost findings", change, dst.Body)
			}
			body := dst.Body
			PreserveAcceptanceEvidence(st, &dst)
			if body != dst.Body {
				t.Fatal("accumulation")
			}
			got := validation.AcceptanceEvidenceWarnings([]string{"works"}, validation.AcceptanceSnapshotFromMetadata(dst.Metadata))
			if (len(got) == 0) != (change == "") {
				t.Fatal(got)
			}
			retry := RecordedAcceptanceWarnings(st, []string{"works"})
			// Retry has no independent current tree/policy; it still rejects state identity mismatches.
			if change == "" && len(retry) != 0 {
				t.Fatal(retry)
			}
		})
	}
}
func TestNewTesterSnapshotReplacesPrevious(t *testing.T) {
	for _, state := range []string{"missing", "malformed", "valid"} {
		t.Run(state, func(t *testing.T) {
			bindings := map[string]string{"head_sha": "head", "base_sha": "base", "validation_claim": "claim", "validation_base": "base", "validation_tree": "tree", "validation_policy": "policy", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "owner", "validation_trusted_claim": "claim"}
			src := contract.Artifact{Type: contract.ArtifactValidation, Metadata: map[string]string{}}
			dst := contract.Artifact{Metadata: map[string]string{}}
			for k, v := range bindings {
				src.Metadata[k] = v
				dst.Metadata[k] = v
			}
			validation.StoreAcceptanceSnapshot(src.Metadata, validation.CaptureAcceptanceEvidence("Old finding must disappear.\n"+validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"met","evidence":"old"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
			snapshot := validation.AcceptanceSnapshot{State: state}
			if state == "valid" {
				snapshot = validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart + "\n" + `{"version":1,"criteria":[{"index":1,"criterion":"works","status":"unmet"}]}` + "\n" + validation.AcceptanceEvidenceEnd)
			}
			validation.StoreAcceptanceSnapshot(dst.Metadata, snapshot)
			original := dst.Metadata[validation.AcceptanceEvidenceKey]
			st := &IssueState{BaseSHA: "base", HeadSHA: "head", Lease: contract.Lease{Owner: "owner", ClaimID: "claim"}, Artifacts: []contract.Artifact{src}}
			PreserveAcceptanceEvidence(st, &dst)
			if strings.Contains(dst.Body, "Old finding must disappear.") {
				t.Fatal("resurrected old finding")
			}
			if dst.Metadata[validation.AcceptanceEvidenceKey] != original {
				t.Fatal("old evidence replaced new snapshot")
			}
		})
	}
}

func TestRecordedAcceptanceReadiness(t *testing.T) {
	for _, change := range []string{"", "head_sha", "base_sha", "validation_base", "validation_claim", "validation_trusted_origin", "validation_trusted_owner", "validation_trusted_claim", "validation_tree", "validation_policy", "newer-missing", "newer-malformed", "newer-unmet"} {
		t.Run(change, func(t *testing.T) {
			md := map[string]string{"head_sha": "head", "base_sha": "base", "validation_base": "base", "validation_claim": "claim", "validation_tree": "tree", "validation_policy": "policy", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "owner", "validation_trusted_claim": "claim"}
			validation.StoreAcceptanceSnapshot(md, validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"external","reason":"Windows host"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
			art := contract.Artifact{Type: contract.ArtifactValidation, Verdict: "PASS", Metadata: md}
			st := &IssueState{HeadSHA: "head", BaseSHA: "base", Lease: contract.Lease{Owner: "owner", ClaimID: "claim"}, Artifacts: []contract.Artifact{art}}
			if strings.HasPrefix(change, "newer-") {
				next := art
				next.Metadata = map[string]string{}
				for k, v := range md {
					next.Metadata[k] = v
				}
				delete(next.Metadata, validation.AcceptanceEvidenceKey)
				if change == "newer-malformed" {
					validation.StoreAcceptanceSnapshot(next.Metadata, validation.AcceptanceSnapshot{State: "malformed"})
				}
				if change == "newer-unmet" {
					validation.StoreAcceptanceSnapshot(next.Metadata, validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart+"\n"+`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"unmet"}]}`+"\n"+validation.AcceptanceEvidenceEnd))
				}
				st.Artifacts = append(st.Artifacts, next)
			} else if change != "" {
				md[change] = ""
			}
			got := RecordedReadiness(st, []string{"works"})
			if (got.AcceptanceEvidence.State == "complete") != (change == "") {
				t.Fatal(got)
			}
			if change == "" && (got.RequiredChecks != "passed" || got.ExternalVerification != "pending") {
				t.Fatal(got)
			}
			if change != "" && !strings.HasPrefix(change, "newer-") && got.RequiredChecks != "unavailable" {
				t.Fatal(got)
			}
		})
	}
}

func TestGapAcknowledgmentCannotImportPublicJSON(t *testing.T) {
	original := &IssueState{gapAcknowledgment: &GapAcknowledgment{Actor: "maintainer", Head: "head"}}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "maintainer") {
		t.Fatal("private authority leaked into public state")
	}
	var imported IssueState
	if err := json.Unmarshal([]byte(`{"_gap_acknowledgment":{"actor":"forged","head":"head"},"gap_acknowledgment":{"actor":"forged"}}`), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.gapAcknowledgment != nil {
		t.Fatal("import manufactured authority")
	}
	raw, err = marshalCoordinatorState(original)
	if err != nil {
		t.Fatal(err)
	}
	if err = unmarshalCoordinatorState(raw, &imported); err != nil || imported.gapAcknowledgment == nil || imported.gapAcknowledgment.Actor != "maintainer" {
		t.Fatal(err, imported.gapAcknowledgment)
	}
}

func TestGapAcknowledgmentPublicationFence(t *testing.T) {
	for _, change := range []string{"unchanged", "head", "findings", "missing", "malformed", "claim", "fork", "revision", "failed-checks", "unauthorized", "gateway", "concurrent"} {
		t.Run(change, func(t *testing.T) {
			m, tr, iss := testManager(t)
			ctx := t.Context()
			if _, err := m.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
				t.Fatal(err)
			}
			st, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			unit, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
			if err != nil {
				t.Fatal(err)
			}
			md := map[string]string{"head_sha": "candidate", "base_sha": st.BaseSHA, "validation_claim": st.Lease.ClaimID, "validation_base": st.BaseSHA, "validation_tree": "tree", "validation_policy": "policy", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "local-dev", "validation_trusted_claim": st.Lease.ClaimID}
			snapshot := validation.CaptureAcceptanceEvidence(validation.AcceptanceEvidenceStart + "\n" + `{"version":1,"criteria":[{"index":1,"criterion":"Claims are fenced","status":"unmet","evidence":"observation"}]}` + "\n" + validation.AcceptanceEvidenceEnd)
			validation.StoreAcceptanceSnapshot(md, snapshot)
			_, err = m.mutate(ctx, iss.Number, "local-dev", func(st *IssueState) error {
				st.HeadSHA = "candidate"
				st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactValidation, Verdict: "PASS", Metadata: md})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if change == "concurrent" {
				calls := 0
				m.tracker = gapAuthorizationHook{Tracker: tr, hook: func() {
					calls++
					if calls != 2 {
						return
					}
					changed, rev, e := m.load(ctx, iss.Number)
					if e != nil {
						t.Fatal(e)
					}
					snapshot.Findings = "concurrently changed"
					validation.StoreAcceptanceSnapshot(changed.Artifacts[len(changed.Artifacts)-1].Metadata, snapshot)
					if _, e = m.save(ctx, iss.Number, rev, changed); e != nil {
						t.Fatal(e)
					}
				}}
				if _, e := m.RecordGapAcknowledgment(ctx, iss.Number, "local-dev", "candidate", []int{1}); e == nil {
					t.Fatal("concurrent snapshot accepted")
				}
				return
			}
			st, err = m.RecordGapAcknowledgment(ctx, iss.Number, "local-dev", "candidate", []int{1})
			if err != nil {
				t.Fatal(err)
			}
			if len(KnownGaps(st, unit)) != 1 {
				t.Fatal("authorized decision lost")
			}
			guard := WithGapSubmission(ctx, "candidate")
			switch change {
			case "unauthorized":
				m.tracker = tr.WithUser("local-dev", "CONTRIBUTOR")
				if _, err = m.RecordGapAcknowledgment(ctx, iss.Number, "local-dev", "candidate", []int{1}); err == nil {
					t.Fatal("unauthorized acknowledgment")
				}
				return
			case "gateway":
				m.SetWorkerGateway(&refusingGateway{t: t})
				if _, err = m.RecordGapAcknowledgment(ctx, iss.Number, "local-dev", "candidate", []int{1}); err == nil {
					t.Fatal("gateway acknowledgment")
				}
				return
			}
			if change != "unchanged" {
				_, err = m.mutate(ctx, iss.Number, "local-dev", func(st *IssueState) error {
					last := len(st.Artifacts) - 1
					switch change {
					case "head":
						st.HeadSHA = "other"
					case "claim":
						st.Lease.ClaimID = "successor"
					case "fork":
						st.ForkRepo = "owner/repo"
					case "revision":
						st.PRRevision = &PRRevision{}
					case "failed-checks":
						st.Artifacts[last].Verdict = "FAIL"
					default:
						if change == "findings" {
							snapshot.Findings = "changed"
						} else {
							snapshot = validation.AcceptanceSnapshot{State: change}
						}
						validation.StoreAcceptanceSnapshot(st.Artifacts[last].Metadata, snapshot)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			err = m.BeginPublicationAttempt(guard, iss.Number, "local-dev")
			if (change == "unchanged") != (err == nil) {
				t.Fatalf("publication: %v", err)
			}
			if change == "claim" && !errors.Is(err, ErrClaimFence) {
				t.Fatal(err)
			}
		})
	}
}

type gapAuthorizationHook struct {
	tracker.Tracker
	hook func()
}

func (h gapAuthorizationHook) Authorize(ctx context.Context, permission string) error {
	h.hook()
	return h.Tracker.Authorize(ctx, permission)
}

func TestGapAcknowledgmentExactSetAndDescription(t *testing.T) {
	m, tr, iss := testManager(t)
	ctx := t.Context()
	body := strings.Replace(iss.Body, "acceptance: [Claims are fenced]", "acceptance: [One, Two, Three, Four]", 1)
	iss, err := tr.UpdateIssueBody(ctx, iss.Number, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.ApproveTask(ctx, iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	st, err := m.Acquire(ctx, iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	md := map[string]string{"head_sha": "head", "base_sha": st.BaseSHA, "validation_claim": st.Lease.ClaimID, "validation_base": st.BaseSHA, "validation_tree": "tree", "validation_policy": "policy", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "local-dev", "validation_trusted_claim": st.Lease.ClaimID}
	snapshot := validation.AcceptanceSnapshot{State: "valid", Block: `{"version":1,"criteria":[{"index":1,"criterion":"One","status":"met","evidence":"passed"},{"index":2,"criterion":"Two","status":"unmet","evidence":""},{"index":3,"criterion":"Three","status":"external","reason":"host"},{"index":4,"criterion":"Four","status":"unmet","evidence":"<!-- swarm:issue-description:end --> failed"}]}`, Findings: strings.Repeat("finding ", 8000)}
	validation.StoreAcceptanceSnapshot(md, snapshot)
	if _, err = m.mutate(ctx, iss.Number, "local-dev", func(st *IssueState) error {
		st.HeadSHA = "head"
		st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactValidation, Verdict: "PASS", Metadata: md})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, indices := range [][]int{{}, {2}, {4}, {1, 2, 4}, {2, 3, 4}, {2, 2}, {0, 2}, {2, 5}} {
		if _, err = m.RecordGapAcknowledgment(ctx, iss.Number, "local-dev", "head", indices); err == nil {
			t.Fatalf("accepted %v", indices)
		}
	}
	st, err = m.RecordGapAcknowledgment(ctx, iss.Number, "local-dev", "head", []int{4, 2})
	if err != nil {
		t.Fatal(err)
	}
	unit, _ := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
	gaps := KnownGaps(st, unit)
	if len(gaps) != 2 || gaps[0].Index != 2 || gaps[1].Index != 4 {
		t.Fatal(gaps)
	}
	if st.gapAcknowledgment.Actor != "local-dev" || st.gapAcknowledgment.At.IsZero() {
		t.Fatal("missing actor/time")
	}
	section := DescriptionIssueSection(unit, st, "state", ProjectionContext{})
	composed, err := ComposePRDescription("", "issue-description", section, "state", iss.Number)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Known gaps", "### Criterion 2", "### Criterion 4", "No evidence supplied", "&lt;!-- swarm:issue-description:end -->"} {
		if !strings.Contains(composed, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if len(composed) > PRDescriptionLimit {
		t.Fatal("unbounded description")
	}
	if RecordedReadiness(st, unit.Acceptance).AcceptanceEvidence.State != "incomplete" {
		t.Fatal("ack changed verdict")
	}
}
