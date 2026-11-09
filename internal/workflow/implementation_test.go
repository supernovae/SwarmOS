package workflow

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlockingOutcomeParsing(t *testing.T) {
	t.Parallel()
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
				if out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
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
			// A tester launch strips the previous role appendix from delivered copies.
			// Failed developer rounds keep their report; stored evidence above always retains it.
			if readErr != nil || strings.Contains(string(data), "Use the local fixtures") != tc.fail {
				t.Fatalf("report guidance: %v %s", readErr, data)
			}
		})
	}
}
func TestRejectedPlanCanonicalContext(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestRoleScopedInstructionsDelivery(t *testing.T) {
	e, issue := budgetEngine(t)
	opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true,
		Instructions: "global-marker", DeveloperInstructions: "developer-marker\nsecond line", TesterInstructions: "tester-marker", PlannerInstructions: "planner-marker", ReviewerInstructions: "reviewer-marker"}
	seen := map[string]bool{}
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			seen[req.Role] = true
			want := "global-marker\n\n" + map[string]string{"developer": "developer-marker\nsecond line", "tester": "tester-marker", "planner": "planner-marker", "reviewer": "reviewer-marker"}[req.Role]
			if req.OperatorInstructions != want || !strings.Contains(req.TaskPrompt, want) {
				t.Fatalf("%s guidance: %+v", req.Role, req)
			}
			for _, role := range []string{"planner", "reviewer", "developer", "tester"} {
				if strings.Contains(req.TaskPrompt, role+"-marker") != (role == req.Role) {
					t.Fatalf("%s task leaked %s", req.Role, role)
				}
			}
			for _, name := range []string{"SYSTEM_PROMPT.md", "WORKUNIT.md", "REVIEW_EVIDENCE.md", "IMPLEMENTATION.md", "PLAN.md", "TEST_PLAN.md", "REVIEW.md"} {
				body, err := os.ReadFile(filepath.Join(req.WorktreePath, ".swarm/active", name))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, role := range []string{"planner", "reviewer", "developer", "tester"} {
					if strings.Contains(string(body), role+"-marker") {
						t.Fatalf("%s leaked prior guidance in %s: %s", req.Role, name, body)
					}
				}
			}
			return (&agent.MockRunner{}).Run(ctx, req)
		}}, nil
	}
	res, err := e.ExecuteWork(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"planner", "reviewer", "developer", "tester"} {
		if !seen[role] {
			t.Fatalf("missing %s", role)
		}
	}
	for _, a := range res.State.Artifacts {
		role := map[contract.ArtifactType]string{contract.ArtifactDesign: "planner", contract.ArtifactTestPlan: "planner", contract.ArtifactReview: "reviewer", contract.ArtifactImplementation: "developer", contract.ArtifactValidation: "tester"}[a.Type]
		if role != "" && (!strings.Contains(a.Body, "global-marker") || !strings.Contains(a.Body, role+"-marker")) {
			t.Fatalf("missing guidance in %s: %s", a.Type, a.Body)
		}
	}
}

func TestTesterOutcomeParsing(t *testing.T) {
	guidance := &agent.RunResult{Guidance: []string{"VERDICT: FAIL"}}
	if got := parseTesterOutcome(guidedReport("", guidance)); got != testerUnknown {
		t.Fatalf("guidance supplied outcome: %q", got)
	}
	if got := parseTesterOutcome(guidedReport("VERDICT: PASS", guidance)); got != testerPass {
		t.Fatalf("guidance overrode outcome: %q", got)
	}

	for _, tc := range []struct {
		body string
		want testerOutcome
	}{
		{"# ISSUE-176 validation (tester, advisory)\nVERDICT: FAIL. The implementation was not delivered, so validation is blocked on reopening plan review.", testerFail},
		{"VERDICT: FAIL", testerFail},
		{"VERDICT: BLOCKED", testerBlocked},
		{"VERDICT: PASS", testerPass},
		{"Independent regression review passed.\nAC2 awaits coordinator policy.", testerUnknown},
		{"All criteria remain unmet.", testerUnknown},
		{"", testerUnknown},
		{"MISSING", testerUnknown},
		{"<!-- swarm:acceptance-evidence:start -->", testerUnknown},
		{"Partial coverage.", testerUnknown},
		{"# title\n## second\nVERDICT: FAIL", testerUnknown},
		{"Introduction\nVERDICT: FAIL", testerUnknown},
		{"VERDICT: FAILED", testerUnknown},
		{"Verdict: fail", testerUnknown},
		{"```\nVERDICT: FAIL\n```", testerUnknown},
		{"> VERDICT: FAIL", testerUnknown},
		{"## Acceptance\nVERDICT: FAIL", testerUnknown},
		{"A regression printed FAIL but passed.", testerUnknown},
		{"", testerUnknown},
		{"\r\n# Title\r\n\r\nVERDICT: PASS: Reviewed.\r\n", testerPass},
		{"VERDICT: BLOCKED - Missing fixture.", testerBlocked},
		{"VERDICT: PASS\nVERDICT: FAIL", testerPass},
		{"VERDICT: FAIL\nVERDICT: PASS", testerFail},
		{"VERDICT: FAIL trailing prose", testerUnknown},
		{"VERDICT: FAIL.", testerUnknown},
		{"# Title\n", testerUnknown},
	} {
		if got := parseTesterOutcome(tc.body); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.body, got, tc.want)
		}
	}
}

func TestCheckpointPublicationFailureGuidance(t *testing.T) {
	predecessor, candidate := strings.Repeat("a", 40), strings.Repeat("b", 40)
	cause := errors.New("publication unavailable")
	for _, tc := range []struct {
		name string
		fork bool
		pr   int
		want string
	}{
		{"issue", false, 0, "swarm issue reconcile 462 --checkpoint --expected-head " + predecessor + " --candidate-head " + candidate},
		{"fork", true, 0, "automatic checkpoint reconciliation is unavailable for fork mode"},
		{"pr", false, 9, "checkpoint reconciliation is unavailable for PR revisions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &workRun{e: &Engine{LeaseManager: &lease.Manager{}}, opts: WorkOptions{IssueNumber: 462, PRNumber: tc.pr}, forkMode: tc.fork}
			err := r.checkpointPublicationError(cause, predecessor, candidate)
			if !errors.Is(err, cause) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "IMPLEMENTATION.md retained") {
				t.Fatal(err)
			}
			if (tc.fork || tc.pr != 0) && strings.Contains(err.Error(), "swarm issue reconcile") {
				t.Fatal("unsupported reconcile hint", err)
			}
			if err := r.checkpointPublicationError(cause, "", candidate); strings.Contains(err.Error(), "swarm issue reconcile") {
				t.Fatal("unpinned reconcile hint", err)
			}
		})
	}
}

func TestPartialImplementationParsing(t *testing.T) {
	for _, tc := range []struct{ report, want string }{
		{"Status: PARTIAL\nSDK criterion unmet\n## Handoff\nother", "SDK criterion unmet"},
		{"Status: PARTIAL", "Approved plan assumption failed; unmet acceptance criteria require reassessment"},
		{"partial implementation", ""}, {"> Status: PARTIAL", ""},
		{"```text\nStatus: PARTIAL\n```", ""}, {"~~~\nStatus: PARTIAL\n~~~", ""},
		{"Status: PARTIAL extra", ""}, {"status: partial", ""}, {"", ""},
	} {
		if got := partialReason(tc.report); got != tc.want {
			t.Errorf("%q: %q want %q", tc.report, got, tc.want)
		}
	}
}

func TestPartialImplementationOutcomeStopsBeforeTester(t *testing.T) {
	for _, tc := range []struct {
		name, cmd string
		fail      bool
	}{
		{"partial", "printf work > real.txt; printf 'Status: PARTIAL\nUnmet criterion: SDK integration\n' > .swarm/active/IMPLEMENTATION.md", true},
		{"unchanged-partial", "printf 'Status: PARTIAL\nUnmet criterion: SDK integration\n' > .swarm/active/IMPLEMENTATION.md", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, eng, tr := setupProjectPlanTestRepo(t)
			configureValidation(t, root)
			for _, args := range [][]string{{"add", "."}, {"commit", "-m", "validation"}} {
				if out, err := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
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
				var partial *PartialImplementationError
				if !errors.As(err, &partial) || !strings.Contains(err.Error(), "swarm issue edit") || art.Metadata["implementation_outcome"] != "partial" || art.Metadata["implementation_trusted_origin"] != "" || !strings.Contains(art.Summary, "Unmet criterion: SDK integration") || !strings.Contains(art.Body, "Unmet criterion: SDK integration") || art.Metadata["head_sha"] != result.HeadSHA {
					t.Fatalf("partial evidence: %+v %v", art, err)
				}
				if c, e := eng.LeaseManager.InspectCheckpointCompletion(t.Context(), issue.Number); e != nil || c != nil {
					t.Fatalf("completion: %+v %v", c, e)
				}
				if art.Verdict != "FAIL" || !strings.Contains(err.Error(), "--phase plan") {
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
			// A tester launch strips the previous role appendix from delivered copies.
			// Failed developer rounds keep their report; stored evidence above always retains it.
			if readErr != nil || strings.Contains(string(data), "Use the local fixtures") != tc.fail {
				t.Fatalf("report guidance: %v %s", readErr, data)
			}
		})
	}
}

func TestPartialImplementationPASSOnlyConsumers(t *testing.T) {
	st := &lease.IssueState{BaseSHA: "base", TaskDigest: "task", PolicyDigest: "policy", NextPhaseReason: "claim budget", Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan"}, {Type: contract.ArtifactTestPlan, Body: "tests"}}}
	md := lease.ReviewBindings(st)
	md["head_sha"] = "head"
	md["implementation_trusted_origin"] = "coordinator"
	md["implementation_outcome"] = "partial"
	st.Artifacts = append(st.Artifacts, contract.Artifact{Type: contract.ArtifactImplementation, Verdict: "PASS", Metadata: md})
	if !implementationProven(st, "head") || !hasTrustedImplementation(st) {
		t.Fatal("invalid PASS control")
	}
	st.Artifacts[len(st.Artifacts)-1].Verdict = "FAIL"
	if implementationProven(st, "head") || hasTrustedImplementation(st) {
		t.Fatal("partial accepted as success")
	}
}

type partialGateway struct{}

func (partialGateway) Apply(context.Context, lease.ForkRequest) (*lease.IssueState, error) {
	return nil, errors.New("unexpected gateway call")
}

func TestPartialImplementationScopeExclusions(t *testing.T) {
	for _, mode := range []string{"native", "fork", "gateway", "option-pr", "state-pr", "revision", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			r := &workRun{e: &Engine{LeaseManager: &lease.Manager{}}, st: &lease.IssueState{}}
			report := "Status: PARTIAL\nUnmet criterion\n"
			switch mode {
			case "fork":
				r.forkMode = true
			case "gateway":
				r.e.LeaseManager.SetWorkerGateway(partialGateway{})
			case "option-pr":
				r.opts.PRNumber = 1
			case "state-pr":
				r.st.PRNumber = 1
			case "revision":
				r.st.PRRevision = &lease.PRRevision{}
			case "blocked":
				report += "Status: BLOCKED: missing SDK"
			}
			if got := r.partialImplementationReason(report); (got != "") != (mode == "native") {
				t.Fatalf("%s: %q", mode, got)
			}
		})
	}
}

func TestHasTrustedImplementationCurrentBindings(t *testing.T) {
	for _, change := range []string{"matching", "retry claim", "plan", "test-plan", "missing plan", "missing test-plan", "task", "policy", "base", "policy tree", "missing metadata", "empty binding", "untrusted", "failed"} {
		t.Run(change, func(t *testing.T) {
			st := &lease.IssueState{BaseSHA: "base", TaskDigest: "task", PolicyDigest: "policy", PolicyTreeDigest: "policy-tree", Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan"}, {Type: contract.ArtifactTestPlan, Body: "tests"}}}
			md := map[string]string{"implementation_trusted_origin": "coordinator"}
			for k, v := range lease.ReviewBindings(st) {
				md["implementation_trusted_"+k] = v
			}
			implementation := contract.Artifact{Type: contract.ArtifactImplementation, Verdict: "PASS", Metadata: md}
			switch change {
			case "retry claim":
				st.Lease.ClaimID = "successor-claim"
			case "plan":
				st.Artifacts[0].Body += " changed"
			case "test-plan":
				st.Artifacts[1].Body += " changed"
			case "missing plan":
				st.Artifacts = st.Artifacts[1:]
			case "missing test-plan":
				st.Artifacts = st.Artifacts[:1]
			case "task":
				st.TaskDigest = "changed"
			case "policy":
				st.PolicyDigest = "changed"
			case "base":
				st.BaseSHA = "changed"
			case "policy tree":
				st.PolicyTreeDigest = "changed"
			case "missing metadata":
				delete(md, "implementation_trusted_plan_digest")
			case "empty binding":
				st.TaskDigest = ""
				md["implementation_trusted_task_digest"] = ""
			case "untrusted":
				delete(md, "implementation_trusted_origin")
			case "failed":
				implementation.Verdict = "FAIL"
			}
			st.Artifacts = append(st.Artifacts, implementation)
			want := change == "matching" || change == "retry claim"
			if got := hasTrustedImplementation(st); got != want {
				t.Fatalf("hasTrustedImplementation = %t, want %t", got, want)
			}
		})
	}
}
