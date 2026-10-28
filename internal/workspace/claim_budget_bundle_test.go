package workspace

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

var leaseBlock = regexp.MustCompile(`(?m)^lease:\n(?:  .*\n)+`)

// budgetRepo commits a harness whose approved lease policy is lease, then
// writes conflicting candidate policy into the provisioned worktree.
func budgetRepo(t *testing.T, lease string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runWorkspaceGit(t, repo, "init", "-b", "main")
	runWorkspaceGit(t, repo, "config", "user.name", "Test")
	runWorkspaceGit(t, repo, "config", "user.email", "test@example.test")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: repo}); err != nil {
		t.Fatal(err)
	}
	setLease(t, repo, lease)
	runWorkspaceGit(t, repo, "add", ".")
	runWorkspaceGit(t, repo, "commit", "-m", "harness")
	base := workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	wt, _, err := ProvisionAt(repo, 7, "Budget", base, "", "")
	if err != nil {
		t.Fatal(err)
	}
	setLease(t, wt, "lease:\n  duration: 9h\n  renewable: true\n  max_renewals: 9\n")
	return wt, base
}

func setLease(t *testing.T, root, lease string) {
	t.Helper()
	path := filepath.Join(root, ".swarm", "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !leaseBlock.Match(raw) {
		t.Fatalf("config has no lease block:\n%s", raw)
	}
	if err := os.WriteFile(path, leaseBlock.ReplaceAll(raw, []byte(lease)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func budgetBundle(t *testing.T, wt, base, role string, prNumber int) (string, error) {
	t.Helper()
	wu := &contract.WorkUnit{ID: "ISSUE-7", IssueNumber: 7, Title: "Budget", Objective: "Size work", Acceptance: []string{"go test passes"}, Workflow: "feature", Gate: contract.GateAgent}
	wf := &contract.WorkflowSpec{Name: "feature"}
	var err error
	if prNumber > 0 {
		err = CompilePRHarnessBundleAt(wt, wu, wf, nil, role, base, prNumber)
	} else {
		err = CompileHarnessBundleAt(wt, wu, wf, nil, role, base)
	}
	if err != nil {
		return "", err
	}
	raw, readErr := os.ReadFile(filepath.Join(wt, ".swarm", "active", "SYSTEM_PROMPT.md"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(raw), nil
}

func TestPlannerBundleStatesApprovedClaimBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, lease, want string
		pr                int
	}{
		{"no renewals", "lease:\n  duration: 30m\n  renewable: false\n  max_renewals: 0\n", "total claim budget is 30m0s", 0},
		{"two renewals", "lease:\n  duration: 30m\n  renewable: true\n  max_renewals: 2\n", "total claim budget is 1h30m0s", 0},
		{"pr planner", "lease:\n  duration: 30m\n  renewable: true\n  max_renewals: 2\n", "total claim budget is 1h30m0s", 41},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wt, base := budgetRepo(t, tc.lease)
			sys, err := budgetBundle(t, wt, base, "planner", tc.pr)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(sys, "## Claim budget") || !strings.Contains(sys, tc.want) || !strings.Contains(sys, "lease.duration × (1 + max_renewals)") {
				t.Fatalf("planner bundle should state %q from the approved base:\n%s", tc.want, sys)
			}
			if strings.Contains(sys, "9h") {
				t.Fatalf("planner bundle used candidate lease policy:\n%s", sys)
			}
			sys, err = budgetBundle(t, wt, base, "reviewer", tc.pr)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(sys, "## Claim budget") {
				t.Fatalf("only the planner sizes work against the claim budget:\n%s", sys)
			}
		})
	}
}

func TestPlannerBundleRejectsInvalidApprovedBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, lease string }{
		{"malformed duration", "lease:\n  duration: soon\n  renewable: true\n  max_renewals: 2\n"},
		{"overflow", "lease:\n  duration: 2562047h\n  renewable: true\n  max_renewals: 2\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wt, base := budgetRepo(t, tc.lease)
			if sys, err := budgetBundle(t, wt, base, "planner", 0); err == nil {
				t.Fatalf("invalid approved lease policy produced a planner bundle:\n%s", sys)
			}
		})
	}
}

func TestClaimBudgetOverflowIsRejected(t *testing.T) {
	t.Parallel()
	if _, err := claimBudgetDoc(contract.LeaseConfig{Duration: "2562047h", MaxRenewals: 1}); err == nil {
		t.Fatal("overflowing total budget was accepted")
	}
	doc, err := claimBudgetDoc(contract.LeaseConfig{Duration: "4h", MaxRenewals: 3})
	if err != nil || !strings.Contains(doc, "16h0m0s") {
		t.Fatalf("budget doc = %q, %v", doc, err)
	}
}
