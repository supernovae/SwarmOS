package validation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

// cacheableTestCheck omits -count=1, so Go's test cache may reuse results.
const cacheableTestCheck = "  - name: test\n    command: go test ./...\n    timeout: 5m\n"

// cachedPackages returns the packages that `go test` reported as cached.
func cachedPackages(output string) map[string]bool {
	cached := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "ok" && fields[2] == "(cached)" {
			cached[fields[1]] = true
		}
	}
	return cached
}

func TestGoFastPassReusesTestCache(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, cacheableTestCheck, nil)
	commitRepoFile(t, repo, "leaf/leaf.go", passingLeaf)
	report, err := Validate(t.Context(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base, GoFastPass: true})
	if err != nil || report == nil || !report.Passed || len(report.CheckOutcomes) != 2 {
		t.Fatalf("validation: %+v %v", report, err)
	}
	pre, full := report.CheckOutcomes[0], report.CheckOutcomes[1]
	if !pre.Preliminary || !pre.Passed {
		t.Fatalf("preliminary: %+v", pre)
	}
	if full.Command != "go test ./..." || full.Preliminary || !full.Passed {
		t.Fatalf("full check must run unchanged: %+v", full)
	}
	cached := cachedPackages(full.Output)
	for _, pkg := range affectedByLeaf {
		if pkg == "example.com/m/transitive" {
			continue // no test files, so nothing to cache
		}
		if !cached[pkg] {
			t.Errorf("full run did not reuse the changed-package result for %s:\n%s", pkg, full.Output)
		}
	}
	if !strings.Contains(full.Output, "example.com/m/unrelated") || cached["example.com/m/unrelated"] {
		t.Fatalf("full run must still test unrelated packages:\n%s", full.Output)
	}
}

func TestGoFastPassCachedFullStillFails(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, cacheableTestCheck, map[string]string{"unrelated/unrelated_test.go": "package unrelated\n\nimport \"testing\"\n\nfunc TestUnrelated(t *testing.T) { t.Fatal(\"broken\") }\n"})
	commitRepoFile(t, repo, "leaf/leaf.go", passingLeaf)
	report, err := Validate(t.Context(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base, GoFastPass: true})
	if !errors.Is(err, ErrRequiredCheckFailed) || report == nil || report.Passed || len(report.CheckOutcomes) != 2 {
		t.Fatalf("an unrelated failure must fail validation: %+v %v", report, err)
	}
	pre, full := report.CheckOutcomes[0], report.CheckOutcomes[1]
	if !pre.Preliminary || !pre.Passed {
		t.Fatalf("preliminary: %+v", pre)
	}
	if full.Passed || !strings.Contains(full.Output, "TestUnrelated") || !cachedPackages(full.Output)["example.com/m/leaf"] {
		t.Fatalf("full run must reuse leaf and still fail on unrelated:\n%s", full.Output)
	}
}

func TestSkippedTestsCachedEvidence(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, cacheableTestCheck, nil)
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte(".swarm/active/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	commitRepoFile(t, repo, "leaf/leaf_test.go", goModuleFixture["leaf/leaf_test.go"]+"\nfunc TestSandbox(t *testing.T) { t.Skip(\"nested bwrap unavailable\") }\n")
	report, err := Validate(t.Context(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base, GoFastPass: true, Version: "v1", ClaimID: "claim"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.SkippedTotal != 1 || len(report.SkippedTests) != 1 {
		t.Fatalf("%+v", report)
	}
	skip := report.SkippedTests[0]
	if skip.Test != "TestSandbox" || skip.Status != "new" || !strings.Contains(skip.Message, "nested bwrap unavailable") {
		t.Fatalf("%+v", skip)
	}
	full := report.CheckOutcomes[len(report.CheckOutcomes)-1]
	if !cachedPackages(full.Output)["example.com/m/leaf"] || strings.Contains(full.Output, `"Action"`) {
		t.Fatal(full.Output)
	}
	if !strings.Contains(report.Artifact.Body, "TestSandbox [new]") || !strings.Contains(report.Artifact.Body, "1 test skip events") {
		t.Fatal(report.Artifact.Body)
	}
	report.Artifact.Metadata["validation_trusted_origin"] = "coordinator"
	report.Artifact.Metadata["validation_trusted_owner"] = "owner"
	report.Artifact.Metadata["validation_trusted_claim"] = "claim"
	reused, err := Reuse(t.Context(), repo, ValidateOptions{BaseSHA: base, Version: "v1", ClaimID: "claim"}, "owner", []contract.Artifact{report.Artifact})
	if err != nil || reused == nil || reused.SkippedTotal != 1 || reused.SkippedTests[0] != skip || strings.Contains(reused.CheckOutcomes[len(reused.CheckOutcomes)-1].Output, `"Action"`) {
		t.Fatalf("reuse: %+v %v", reused, err)
	}
}
