package validation

import (
	"errors"
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
