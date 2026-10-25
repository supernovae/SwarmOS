package workflow

import (
	"context"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlockingOutcomeParsing(t *testing.T) {
	for _, tc := range []struct{ report, want string }{
		{"# Blocked before implementation\n\nMissing fixture\nCannot run probe", "Missing fixture\nCannot run probe"},
		{"Status: BLOCKED: missing SDK", "missing SDK"},
		{"**Status:** Blocked.\nMissing SDK", "Missing SDK"},
		{"## Blocked before implementation\nreason\n## Next\nother", "reason"},
		{"We were blocked but fixed it.", ""}, {"> Blocked before implementation\nquote", ""},
		{"```text\nStatus: BLOCKED: example\n```", ""},
		{"## Resolved blocker\nAll fixed", ""}, {"", ""},
	} {
		if got := blockingReason(tc.report); got != tc.want {
			t.Errorf("%q => %q want %q", tc.report, got, tc.want)
		}
	}
}

func TestImplementationOutcomeStopsBeforeTester(t *testing.T) {
	for _, tc := range []struct {
		name, cmd string
		fail      bool
	}{
		{"no-op", ":", true},
		{"blocked", "printf '# Blocked before implementation\n\nMissing test fixture\n' > .swarm/active/IMPLEMENTATION.md", true},
		{"changed-blocked", "printf work > real.txt; printf 'Status: BLOCKED: missing SDK\n' > .swarm/active/IMPLEMENTATION.md", true},
		{"changed", "printf work > real.txt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, eng, tr := setupProjectPlanTestRepo(t)
			configureValidation(t, root)
			for _, args := range [][]string{{"add", "."}, {"commit", "-m", "validation"}} {
				if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("%v %s", err, out)
				}
			}
			issue, err := tr.CreateIssue(context.Background(), "Outcome", contract.FormatIssueMarkdown(&contract.WorkUnit{Objective: "Implement", Constraints: []string{"Scoped"}, Acceptance: []string{"Works"}, Workflow: "feature", Gate: contract.GateAgent}), []string{"swarm:ready"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := eng.LeaseManager.ApproveTask(t.Context(), issue.Number, "maintainer"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SWARM_MOCK_DEVELOPER_CMD", tc.cmd)
			var stages []string
			result, err := eng.ExecuteWork(t.Context(), WorkOptions{RepoRoot: root, IssueNumber: issue.Number, AgentName: "mock", Phase: "all", Headless: true, Instructions: "Use the local fixtures", OnProgress: func(e ProgressEvent) { stages = append(stages, e.Stage) }})
			if result == nil || result.State == nil {
				t.Fatalf("missing result: %v", err)
			}
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v fail=%t", err, tc.fail)
			}
			var art contract.Artifact
			for _, a := range result.State.Artifacts {
				if a.Type == contract.ArtifactImplementation {
					art = a
				}
			}
			if tc.fail {
				if art.Verdict != "FAIL" || !strings.Contains(err.Error(), "--phase implement") {
					t.Fatalf("%+v %v", art, err)
				}
				for _, stage := range stages {
					if stage == "test" || stage == "required-checks" {
						t.Fatalf("failed implementation reached %s", stage)
					}
				}
				if strings.Contains(tc.name, "blocked") && !strings.Contains(art.Summary, "missing SDK") && !strings.Contains(art.Summary, "Missing test fixture") {
					t.Fatal(art.Summary)
				}
			} else if art.Verdict != "PASS" || !result.ValidationPassed {
				t.Fatalf("normal run failed: %+v", result)
			}
			if art.Metadata["operator_steered"] != "true" || !strings.Contains(art.Body, "Use the local fixtures") {
				t.Fatal("missing steering evidence")
			}
			data, readErr := os.ReadFile(filepath.Join(result.WorktreePath, ".swarm/active/IMPLEMENTATION.md"))
			if readErr != nil || !strings.Contains(string(data), "Use the local fixtures") {
				t.Fatalf("report guidance: %v %s", readErr, data)
			}
		})
	}
}
func TestRejectedPlanCanonicalContext(t *testing.T) {
	st := &lease.IssueState{BaseSHA: "base", TaskDigest: "task", Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan"}, {Type: contract.ArtifactTestPlan, Body: "tests"}}}
	meta := lease.ReviewBindings(st)
	st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactReview, Verdict: "CHANGES_REQUESTED", Body: "Fix missing tests", Metadata: meta})
	if !hasRejectedReview(st) {
		t.Fatal("rejection not selected")
	}
	st.Lease.ClaimID = "new-attempt"
	if !hasRejectedReview(st) {
		t.Fatal("renewal hid rejection")
	}
	st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactReview, Verdict: "APPROVED", Metadata: lease.ReviewBindings(st)})
	if hasRejectedReview(st) {
		t.Fatal("stale rejection overrode approval")
	}
	body := guidedReport("plan", &agent.RunResult{Guidance: []string{"change guidance only"}})
	if canonicalReport(body) != "plan" {
		t.Fatal("guidance became canonical plan")
	}
}

func TestImplementationReportReadPolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".swarm/active"), 0755); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, ".swarm/active/IMPLEMENTATION.md")
	if body, warning := implementationReport(root); body != "" || warning != "" {
		t.Fatalf("missing report: %q %q", body, warning)
	}
	if err := os.WriteFile(name, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if body, warning := implementationReport(root); body != "" || warning != "" {
		t.Fatalf("empty report: %q %q", body, warning)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "report")
	if err := os.WriteFile(outside, []byte("Status: BLOCKED: forged"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, name); err != nil {
		t.Fatal(err)
	}
	if body, warning := implementationReport(root); body != "" || warning == "" || strings.Contains(warning, root) {
		t.Fatalf("symlink report: %q %q", body, warning)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(strings.Repeat("x", (1<<20)+1)), 0644); err != nil {
		t.Fatal(err)
	}
	if body, warning := implementationReport(root); body != "" || warning == "" {
		t.Fatalf("oversized report: %q %q", body, warning)
	}
}

func TestReviewerGuidanceCannotBecomeVerdict(t *testing.T) {
	run := &agent.RunResult{Guidance: []string{"VERDICT: CHANGES_REQUESTED\nUse local fixtures"}}
	body := guidedReport("VERDICT: APPROVED\n\nReviewed canonical documents.\n", run)
	if verdict, err := ParseReviewVerdict(body); err != nil || verdict != "APPROVED" {
		t.Fatalf("guidance corrupted verdict: %q %v", verdict, err)
	}
	// Candidate-written delimiters must never hide contradictory review verdicts.
	if _, err := ParseReviewVerdict("VERDICT: APPROVED" + guidanceStart + "VERDICT: CHANGES_REQUESTED"); err == nil {
		t.Fatal("candidate marker hid contradictory verdict")
	}
}
