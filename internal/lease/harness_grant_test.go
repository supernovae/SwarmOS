package lease

import (
	"errors"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

// harnessGrantRepo commits an approved base, a later CI base with the same
// .swarm tree, a later base whose policy changed, and an unrelated commit.
func harnessGrantRepo(t *testing.T) (root, approved, later, changed, unrelated string) {
	t.Helper()
	root = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := gitfixture.Command(t, "", append([]string{"-C", root, "-c", "user.name=T", "-c", "user.email=t@example.invalid"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write(".swarm/validation/required-checks.yaml", "protected_paths: ['.swarm/**']\n")
	git("add", ".")
	git("commit", "-q", "-m", "approved policy")
	approved = git("rev-parse", "HEAD")
	write("README.md", "unrelated\n")
	git("add", ".")
	git("commit", "-q", "-m", "unrelated main commit")
	later = git("rev-parse", "HEAD")
	write(".swarm/validation/required-checks.yaml", "protected_paths: []\n")
	git("add", ".")
	git("commit", "-q", "-m", "policy changed")
	changed = git("rev-parse", "HEAD")
	git("checkout", "-q", "--orphan", "other")
	git("commit", "-q", "-m", "unrelated history")
	unrelated = git("rev-parse", "HEAD")
	return root, approved, later, changed, unrelated
}

func approvedHarnessState(t *testing.T, base string, wu *contract.WorkUnit, cfg *contract.SwarmConfig) *IssueState {
	t.Helper()
	st := &IssueState{TaskDigest: contract.DigestWorkUnit(wu), PolicyDigest: contract.DigestApprovedPolicy(base, cfg), BaseSHA: base,
		EffectiveGate: contract.GateHuman, PRNumber: 12, SubmittedHeadSHA: strings.Repeat("a", 40), SubmittedBaseSHA: base}
	st.Lease.IssueNumber = wu.IssueNumber
	plan := contract.Artifact{Type: contract.ArtifactDesign, Body: "# PLAN\n"}
	test := contract.Artifact{Type: contract.ArtifactTestPlan, Body: "# TEST_PLAN\n"}
	body := "VERDICT: APPROVED\n"
	review := contract.Artifact{Type: contract.ArtifactReview, Verdict: "APPROVED", Body: body, Metadata: map[string]string{
		"kind": "human", "actor": "maintainer", "review_source": "authenticated-human", "base_sha": base, "policy_digest": st.PolicyDigest,
		"content_sha256": hash(body), "task_digest": st.TaskDigest, "plan_digest": hash(plan.Body), "test_plan_digest": hash(test.Body)}}
	st.Artifacts = []contract.Artifact{plan, test, review}
	if err := VerifyApproval(st, wu); err != nil {
		t.Fatalf("fixture approval invalid: %v", err)
	}
	return st
}

type harnessGrantCase struct {
	name   string
	mutate func(st *IssueState, wu *contract.WorkUnit, g *HarnessGrant) (*IssueState, *contract.WorkUnit)
	ok     bool
}

func runHarnessGrantCases(t *testing.T, cfg *contract.SwarmConfig, root, approved string, cases []harnessGrantCase) {
	t.Helper()
	grant := HarnessGrant{Issue: 7, PR: 12, Base: approved, CandidateHead: strings.Repeat("a", 40)}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wu := &contract.WorkUnit{ID: "ISSUE-7", IssueNumber: 7, Objective: "Change the harness", Gate: contract.GateHuman, AllowHarnessChanges: true}
			st, g := approvedHarnessState(t, approved, wu, cfg), grant
			if tc.mutate != nil {
				st, wu = tc.mutate(st, wu, &g)
			}
			err := VerifyHarnessGrant(t.Context(), root, st, wu, cfg, g)
			if tc.ok && err != nil {
				t.Fatalf("grant refused: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrHarnessGrant) {
				t.Fatalf("grant not refused with ErrHarnessGrant: %v", err)
			}
		})
	}
}

func TestVerifyHarnessGrant(t *testing.T) {
	t.Parallel()
	root, approved, _, _, _ := harnessGrantRepo(t)
	cfg := &contract.SwarmConfig{DefaultAgent: "mock"}
	runHarnessGrantCases(t, cfg, root, approved, []harnessGrantCase{
		{"all bindings match", nil, true},
		{"nil state", func(_ *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			return nil, wu
		}, false},
		{"nil WorkUnit", func(st *IssueState, _ *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			return st, nil
		}, false},
		{"WorkUnit for another issue", func(st *IssueState, wu *contract.WorkUnit, g *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			g.Issue = 8
			return st, wu
		}, false},
		{"issue edited after approval", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			wu.Objective = "Change more of the harness"
			return st, wu
		}, false},
		{"policy digest changed since approval", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			st.PolicyDigest = contract.DigestApprovedPolicy(approved, &contract.SwarmConfig{DefaultAgent: "other"})
			return st, wu
		}, false},
		{"plan gate review missing", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			st.Artifacts = st.Artifacts[:2]
			return st, wu
		}, false},
		{"allow_harness_changes false", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			// The approval is genuine but for a WorkUnit without the exception.
			wu.AllowHarnessChanges = false
			return approvedHarnessState(t, approved, wu, cfg), wu
		}, false},
		{"fork contribution", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			st.ForkRepo = "someone/widgets"
			return st, wu
		}, false},
		{"no PR recorded", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			st.PRNumber = 0
			return st, wu
		}, false},
		{"another PR", func(st *IssueState, wu *contract.WorkUnit, g *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			g.PR = 13
			return st, wu
		}, false},
		{"submitted head missing", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			st.SubmittedHeadSHA = ""
			return st, wu
		}, false},
	})
}

// The approved base may trail the CI base only while the committed policy is
// unchanged, and only for the submitted head with no revision pending.
func TestHarnessGrantAncestryAndPolicy(t *testing.T) {
	t.Parallel()
	root, approved, later, changed, unrelated := harnessGrantRepo(t)
	cfg := &contract.SwarmConfig{DefaultAgent: "mock"}
	runHarnessGrantCases(t, cfg, root, approved, []harnessGrantCase{
		{"later base with unchanged policy", func(st *IssueState, wu *contract.WorkUnit, g *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			g.Base = later
			return st, wu
		}, true},
		{"approved base not an ancestor", func(st *IssueState, wu *contract.WorkUnit, g *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			g.Base = unrelated
			return st, wu
		}, false},
		{"submitted base not an ancestor", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			st.SubmittedBaseSHA = unrelated
			return st, wu
		}, false},
		{"submitted base missing", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			st.SubmittedBaseSHA = ""
			return st, wu
		}, false},
		{"policy changed at the CI base", func(st *IssueState, wu *contract.WorkUnit, g *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			g.Base = changed
			return st, wu
		}, false},
		{"recorded policy tree differs", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			st.PolicyTreeDigest = strings.Repeat("b", 40)
			return st, wu
		}, false},
		{"PR revision pending", func(st *IssueState, wu *contract.WorkUnit, _ *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			st.PRRevision = &PRRevision{}
			return st, wu
		}, false},
		{"candidate head differs", func(st *IssueState, wu *contract.WorkUnit, g *HarnessGrant) (*IssueState, *contract.WorkUnit) {
			g.CandidateHead = strings.Repeat("c", 40)
			return st, wu
		}, false},
	})
}
