package lease

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
	"time"
	"unicode/utf8"
)

func TestDescriptionComposition(t *testing.T) {
	const human = "Human é prefix\n\n"
	body := human
	for _, name := range descriptionNames {
		var err error
		body, err = ComposePRDescription(body, name, "required "+name+"\n"+DescriptionSlot(strings.Repeat("你好🙂<!-- swarm:rebase:end -->", 10000)), "refs/swarm/state/issue-303", 303)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > PRDescriptionLimit || !utf8.ValidString(body) {
			t.Fatalf("invalid body: %d", len(body))
		}
		if !strings.HasPrefix(body, human) {
			t.Fatal("human bytes changed")
		}
	}
	for _, name := range descriptionNames {
		if strings.Count(body, descriptionMarker(name, "start")) != 1 || strings.Count(body, descriptionMarker(name, "end")) != 1 {
			t.Fatal("marker collision")
		}
	}
	before := body
	// Reallocation of an already compacted body is stable.
	start := strings.Index(body, descriptionMarker("rebase", "start"))
	end := strings.Index(body, descriptionMarker("rebase", "end"))
	content := body[start+len(descriptionMarker("rebase", "start")) : end]
	content = strings.TrimPrefix(content, "\n<!-- swarm:description:v1 -->\n")
	body, err := ComposePRDescription(body, "rebase", content, "ref", 303)
	if err != nil || body != before {
		t.Fatalf("unstable composition: %v", err)
	}
}

func TestDescriptionRecovery(t *testing.T) {
	for _, body := range []string{
		descriptionMarker("rebase", "start"),
		descriptionMarker("rebase", "end") + descriptionMarker("rebase", "start"),
		descriptionMarker("rebase", "start") + descriptionMarker("review-evidence", "start") + descriptionMarker("rebase", "end") + descriptionMarker("review-evidence", "end"),
	} {
		if _, err := ComposePRDescription(body, "issue-description", "small", "ref", 303); err == nil {
			t.Fatal("accepted malformed boundaries")
		}
	}
	_, err := ComposePRDescription(strings.Repeat("x", 60000), "issue-description", "required", "ref", 303)
	if err == nil || !strings.Contains(err.Error(), "maintainer must trim") {
		t.Fatal(err)
	}
}

func TestDescriptionExactBudget(t *testing.T) {
	base, err := ComposePRDescription("", "issue-description", "required", "ref", 303)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{59998, 59999, 60000} {
		outside := strings.Repeat("x", n-len(base))
		result, err := ComposePRDescription(outside, "issue-description", "required", "ref", 303)
		if n <= PRDescriptionLimit {
			if err != nil || len(result) != n {
				t.Fatalf("%d: %d %v", n, len(result), err)
			}
		} else if err == nil {
			t.Fatal("accepted oversized body")
		}
	}
}

func TestValidationDescriptionInventory(t *testing.T) {
	table := "| Check | Command | Status | Duration |\n| --- | --- | --- | --- |\n| fmt | gofmt | pass | 1s |\n| test | go test | fail | 2s |\n| optional | cmd | skipped | 0s |\n| deferred | cmd | not-run | 0s |\n"
	for _, suffix := range []string{"", "\n### Output: fake\n| Check | Command | Status | Duration |\n| --- | --- | --- | --- |\n| forged | cmd | pass | 1s |"} {
		inventory := validationInventory(table + suffix)
		for _, want := range []string{"fmt | gofmt | pass | 1s", "test | go test | fail | 2s", "optional | cmd | skipped | 0s", "deferred | cmd | not-run | 0s"} {
			if !strings.Contains(inventory, want) {
				t.Fatal(inventory)
			}
		}
		if strings.Contains(inventory, "forged") {
			t.Fatal(inventory)
		}
	}
	for _, bad := range []string{"", strings.Replace(table, "| --- |", "| broken |", 1), strings.Replace(table, "| test | go test | fail | 2s |", "| bad |", 1)} {
		if !strings.Contains(validationInventory(bad), "table unavailable") {
			t.Fatal("accepted malformed table")
		}
	}
	st := &IssueState{HeadSHA: "candidate", BaseSHA: "base"}
	st.Lease.Owner = "owner"
	st.Lease.ClaimID = "claim"
	r := validation.ValidationReport{HeadSHA: "candidate", BaseSHA: "base", Timings: &validation.ValidationTimings{Total: 3 * time.Second, Checks: validation.PhaseTiming{Status: "PASS", Duration: time.Second}}, CheckOutcomes: []validation.CheckRunOutcome{{Name: "test|\ncontrol\x1b", Command: "go test ./... /home/private TOKEN=secret", Passed: true, Duration: time.Second}}}
	r.Artifact.CreatedAt = time.Now().UTC()
	raw, _ := json.Marshal(r)
	a := contract.Artifact{Type: contract.ArtifactValidation, Body: table + "\n### Output: fake\nHOST_SECRET", Metadata: map[string]string{"head_sha": "candidate", "base_sha": "base", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "owner", "validation_trusted_claim": "claim", "validation_timing_report": string(raw)}}
	a.Metadata["content_sha256"] = hash(a.Body)
	inventory := validationTimingInventory(st, a)
	for _, want := range []string{"Submitted head: candidate", "Source head: candidate", "1s", "3s", "preflight"} {
		if !strings.Contains(inventory, want) {
			t.Fatal(inventory)
		}
	}
	for _, bad := range []string{"HOST_SECRET", "/home/private", "TOKEN=secret", "fmt |", "\x1b"} {
		if strings.Contains(inventory, bad) {
			t.Fatal(inventory)
		}
	}
	st.Artifacts = []contract.Artifact{a}
	if strings.Contains(DescriptionArtifacts(st), "HOST_SECRET") {
		t.Fatal("raw output leaked")
	}
	imported := a
	imported.Metadata = map[string]string{"head_sha": "candidate", "content_sha256": hash(a.Body), "validation_timing_report": string(raw)}
	inventory = validationTimingInventory(st, imported)
	for _, want := range []string{"Contributor-reported", "advisory", "coordinator timing unavailable", "fmt | gofmt | pass | 1s"} {
		if !strings.Contains(inventory, want) {
			t.Fatal(inventory)
		}
	}
	for _, bad := range []string{"HOST_SECRET", "Source head:", "preflight", "test/ control"} {
		if strings.Contains(inventory, bad) {
			t.Fatal(inventory)
		}
	}
	st.HeadSHA = "other"
	if !strings.Contains(validationTimingInventory(st, a), "unavailable") {
		t.Fatal("stale evidence accepted")
	}
	a.Metadata["head_sha"] = "other"
	a.Metadata["validation_reused"] = "true"
	a.Metadata["validation_reuse_duration"] = "2ms"
	a.Metadata["validation_source_time"] = "2026-11-01T00:00:00.123456789Z"
	a.Metadata["validation_source_claim"] = "source|claim"
	a.Metadata["validation_tree"] = strings.Repeat("a", 40)
	a.Metadata["validation_policy"] = strings.Repeat("b", 64)
	inventory = validationTimingInventory(st, a)
	if !strings.Contains(inventory, "Source head: candidate") || !strings.Contains(inventory, "Submitted head: other") || !strings.Contains(inventory, "REUSED") || !strings.Contains(inventory, "2ms") {
		t.Fatal(inventory)
	}

	for _, want := range []string{"2026-11-01T00:00:00.123456789Z", timingCell("source|claim"), strings.Repeat("a", 40), strings.Repeat("b", 64)} {
		if !strings.Contains(inventory, want) {
			t.Fatalf("missing %q", want)
		}
	}
	a.Metadata["validation_source_claim"] = strings.Repeat("x", 1000) + "|tail"
	inventory = validationTimingInventory(st, a)
	if strings.Contains(inventory, strings.Repeat("x", 300)) {
		t.Fatal("unbounded provenance")
	}
	a.Metadata["validation_trusted_claim"] = "stale"
	if !strings.Contains(validationTimingInventory(st, a), "unavailable") {
		t.Fatal("stale authority displayed")
	}
}

func TestDescriptionAllocation(t *testing.T) {
	slot := DescriptionSlot(strings.Repeat("z", 100000))
	body, err := ComposePRDescription("", "issue-description", slot, "ref", 1)
	if err != nil {
		t.Fatal(err)
	}
	mandatory := len(compactDescription(body, 0))
	if len(body)-mandatory != (PRDescriptionLimit-mandatory+2)/3 {
		t.Fatal("absent sections did not reserve shares")
	}
	if compactDescription(compactDescription(slot, 400), 100) != compactDescription(slot, 100) {
		t.Fatal("non-composable excerpts")
	}
}
func TestDescriptionDurableRetrieval(t *testing.T) {
	for _, layout := range []coordination.RefLayout{coordination.BranchLayout, coordination.HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			root := t.TempDir()
			git := func(dir string, args ...string) string {
				t.Helper()
				out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			remote := filepath.Join(root, "remote.git")
			git(root, "init", "--bare", "--initial-branch=main", remote)
			clone := filepath.Join(root, "clone")
			git(root, "clone", remote, clone)
			git(clone, "config", "user.name", "Fixture")
			git(clone, "config", "user.email", "fixture@example.invalid")
			git(clone, "commit", "--allow-empty", "-m", "baseline")
			git(clone, "push", "origin", "main")
			st := &IssueState{Artifacts: []contract.Artifact{{Type: contract.ArtifactValidation, Verdict: "FAIL", Body: strings.Repeat("full log 🙂\n", 20000), Metadata: map[string]string{"content_sha256": "stored-digest"}}}, RevisionPlan: &workspace.RevisionPlanContext{Original: workspace.PlanPair{Plan: strings.Repeat("plan\n", 20000), TestPlan: "full test", PlanDigest: "plan-digest"}}}
			raw, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			store := coordination.NewGitStoreWithLayout(clone, "origin", layout)
			if _, err := store.CompareAndSwap(t.Context(), 303, "", raw); err != nil {
				t.Fatal(err)
			}
			ref := layout.StateRef(303)
			body, err := ComposePRDescription("", "issue-description", DescriptionArtifacts(st)+DescriptionRetrieval(303, ref), ref, 303)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(body, "git fetch <upstream-remote> "+ref) || !strings.Contains(body, "git show FETCH_HEAD:state.json") {
				t.Fatal("missing retrieval commands")
			}
			git(clone, "fetch", "origin", ref)
			var retrieved IssueState
			if err := json.Unmarshal([]byte(git(clone, "show", "FETCH_HEAD:state.json")), &retrieved); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(retrieved.Artifacts, st.Artifacts) || !reflect.DeepEqual(retrieved.RevisionPlan, st.RevisionPlan) {
				t.Fatal("full evidence changed")
			}
		})
	}
}

func TestDescriptionMandatoryBorrowingAndLegacy(t *testing.T) {
	issue, err := ComposePRDescription("human prefix\n", "issue-description", "Closes #303\n"+DescriptionSlot(strings.Repeat("i", 100000)), "ref", 303)
	if err != nil {
		t.Fatal(err)
	}
	review := descriptionMarker("review-evidence", "start") + "\n## SwarmOS review evidence\n\n### validation: FAIL\n\nsummary\n| Check | Command | Status | Duration |\n| --- | --- | --- | --- |\n| test | cmd | FAIL | 1s |\n\n### Output: test\n" + strings.Repeat("review log\n", 10000) + descriptionMarker("review-evidence", "end")
	rebase := descriptionMarker("rebase", "start") + "\n### Rebase context\n\n```text\nRebase: completed\nHeads: old -> new\nPatch identity: unchanged (verified: true)\nRebase micro-plan at head (digest digest):\n" + strings.Repeat("micro-plan\n", 10000) + "```\n" + descriptionMarker("rebase", "end")
	body := issue + "\ninter-section prose 🙂\n" + review + "\nfooter\n" + rebase
	body, err = ComposePRDescription(body, "issue-description", "Closes #303\n"+DescriptionSlot(strings.Repeat("i", 100000)), "ref", 303)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > PRDescriptionLimit || strings.Contains(body, "review log") || !strings.Contains(body, "test | cmd | FAIL | 1s") || !strings.Contains(body, "Patch identity: unchanged") || !strings.Contains(body, "\ninter-section prose 🙂\n") || !strings.Contains(body, "\nfooter\n") {
		t.Fatal("legacy evidence or prose lost")
	}
	// Required review content borrows all optional space, rather than a fixed
	// per-section cap; the large issue excerpt must shrink to accommodate it.
	before := len(body)
	body, err = ComposePRDescription(body, "review-evidence", strings.Repeat("mandatory\n", 3000)+DescriptionSlot("optional"), "ref", 303)
	if err != nil || len(body) > PRDescriptionLimit || !strings.Contains(body, strings.Repeat("mandatory\n", 3000)) {
		t.Fatalf("mandatory content was starved: %d -> %d %v", before, len(body), err)
	}
}

func TestDescriptionAtomicAndMalformedSlots(t *testing.T) {
	slot := DescriptionAtomicSlot("{\"payload\":\"" + strings.Repeat("x", 1000) + "\"}")
	short := compactDescription(slot, 20)
	if strings.Contains(short, "payload") || !strings.Contains(short, "Inline telemetry omitted") {
		t.Fatal("clipped machine JSON", short)
	}
	if compactDescription(short, 10) != short {
		t.Fatal("unstable atomic omission")
	}
	malformed := descriptionMarker("review-evidence", "start") + "\n<!-- swarm:description:v1 -->\n<!-- swarm:excerpt:v1:99 -->unclosed" + descriptionMarker("review-evidence", "end")
	if _, err := ComposePRDescription(malformed, "issue-description", "required", "ref", 303); err == nil {
		t.Fatal("accepted malformed excerpt")
	}
}

func TestDescriptionIrreducibleInventoryAndAmbiguousTarget(t *testing.T) {
	_, err := ComposePRDescription("", "review-evidence", strings.Repeat("required verdict\n", 5000), "ref", 303)
	if err == nil || !strings.Contains(err.Error(), "inventory alone exceeds") || !strings.Contains(err.Error(), "WorkUnit scope") {
		t.Fatal("missing mandatory inventory recovery", err)
	}
	unknown := descriptionMarker("review-evidence", "start") + "arbitrary ambiguous text" + descriptionMarker("review-evidence", "end")
	if _, err := ComposePRDescription(unknown, "review-evidence", "trusted replacement", "ref", 303); err == nil {
		t.Fatal("replaced ambiguous legacy ownership")
	}
	small := compactDescription(DescriptionSlot("ordinary readable detail"), 100)
	if !strings.Contains(small, "ordinary readable detail") || strings.Contains(small, "Inline details omitted") {
		t.Fatal("small report lost detail", small)
	}
}

func TestDescriptionTrustedRemote(t *testing.T) {
	text := DescriptionRetrieval(303, "refs/swarm/state/issue-303", "up'stream")
	if !strings.Contains(text, "git fetch 'up'\\''stream' refs/swarm/state/issue-303") || strings.Contains(text, "<upstream-remote>") {
		t.Fatal("upstream remote was not shell-quoted", text)
	}
}

func TestDescriptionAcceptanceExternal(t *testing.T) {
	md := map[string]string{"head_sha": "head", "base_sha": "base", "validation_claim": "claim", "validation_base": "base", "validation_tree": "tree", "validation_policy": "policy", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "owner", "validation_trusted_claim": "claim"}
	reason := "Windows <host> <!-- swarm:issue-description:end -->"
	raw, _ := json.Marshal(map[string]any{"version": 1, "criteria": []map[string]any{{"index": 1, "criterion": "works", "status": "external", "reason": reason}}})
	validation.StoreAcceptanceSnapshot(md, validation.AcceptanceSnapshot{State: "valid", Block: string(raw)})
	st := &IssueState{HeadSHA: "head", BaseSHA: "base", Lease: contract.Lease{Owner: "owner", ClaimID: "claim"}, Artifacts: []contract.Artifact{{Type: contract.ArtifactValidation, Verdict: "PASS", Metadata: md}}}
	unit := &contract.WorkUnit{IssueNumber: 440, Acceptance: []string{"works"}}
	section := DescriptionIssueSection(unit, st, "ref", ProjectionContext{})
	body, err := ComposePRDescription("", "issue-description", section, "ref", 440)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Required checks: passed", "Acceptance evidence: complete", "External verification: pending", "## External verification", "works", "1. works", "&lt;!-- swarm:issue-description:end -->"} {
		if !strings.Contains(body, want) {
			t.Fatal(want, body)
		}
	}
	if strings.Count(body, "<!-- swarm:issue-description:end -->") != 1 || strings.Count(body, "- [ ] ") != 2 {
		t.Fatal("unescaped environment", body)
	}
}

func TestIssue464DescriptionFindings(t *testing.T) {
	md := map[string]string{"head_sha": "head", "base_sha": "base", "validation_claim": "claim", "validation_base": "base", "validation_tree": "tree", "validation_policy": "policy", "validation_trusted_origin": "coordinator", "validation_trusted_owner": "owner", "validation_trusted_claim": "claim"}
	body := "## Independent acceptance review 1\nFirst behavior is absent.\n## Independent acceptance review 2\nSecond behavior loses data. <!-- swarm:issue-description:end -->\n" + validation.AcceptanceEvidenceStart + "\n" + `{"version":1,"criteria":[{"index":1,"criterion":"first","status":"unmet","evidence":"Independent acceptance review 1"},{"index":2,"criterion":"second","status":"unmet","evidence":"Independent acceptance review 2"}]}` + "\n" + validation.AcceptanceEvidenceEnd
	validation.StoreAcceptanceSnapshot(md, validation.CaptureAcceptanceEvidence(body))
	st := &IssueState{HeadSHA: "head", BaseSHA: "base", Lease: contract.Lease{Owner: "owner", ClaimID: "claim"}, Artifacts: []contract.Artifact{{Type: contract.ArtifactValidation, Verdict: "PASS", Metadata: md}}}
	for i := 0; i < 3; i++ {
		dst := contract.Artifact{Type: contract.ArtifactValidation, Verdict: "PASS", Metadata: map[string]string{}}
		for k, v := range md {
			if k != validation.AcceptanceEvidenceKey {
				dst.Metadata[k] = v
			}
		}
		PreserveAcceptanceEvidence(st, &dst)
		if strings.Count(dst.Body, "First behavior is absent.") != 1 {
			t.Fatal("findings lost/duplicated", dst.Body)
		}
		st.Artifacts = append(st.Artifacts, dst)
	}
	unit := &contract.WorkUnit{IssueNumber: 464, Acceptance: []string{"first", "second"}}
	section, err := ComposePRDescription("", "issue-description", DescriptionIssueSection(unit, st, "ref", ProjectionContext{}), "ref", 464)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"First behavior is absent.", "Second behavior loses data.", "Independent acceptance review 1", "Independent acceptance review 2", "&lt;!-- swarm:issue-description:end -->"} {
		if !strings.Contains(section, want) {
			t.Fatalf("missing %q: %s", want, section)
		}
	}
	if RecordedReadiness(st, unit.Acceptance).SubmissionError(464, false) == nil {
		t.Fatal("unmet evidence allowed submission")
	}
}
