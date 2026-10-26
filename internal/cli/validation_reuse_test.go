package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workspace"
)

func countedValidationFixture(t *testing.T) (*cliFixture, string) {
	t.Helper()
	f := newCLIFixture(t)
	counter := filepath.Join(t.TempDir(), "checks")
	policy := fmt.Sprintf("required_checks:\n  - name: counted\n    command: \"printf x >> '%s'; test ! -e '%s.fail'\"\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n", counter, counter)
	if err := os.WriteFile(filepath.Join(f.root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".swarm/validation/required-checks.yaml")
	prGit(t, f.root, "commit", "-m", "count checks")
	return f, counter
}
func checkCount(t *testing.T, path string) int {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return len(b)
}
func latestValidation(t *testing.T, f *cliFixture, n int) contract.Artifact {
	t.Helper()
	for _, a := range f.state(t, n).Artifacts {
		if a.Type == contract.ArtifactValidation {
			return a
		}
	}
	t.Fatal("missing validation")
	return contract.Artifact{}
}
func submitWithDiagnostics(t *testing.T, f *cliFixture, args ...string) (string, string) {
	t.Helper()
	cmd := f.command(nil)
	var out, diag bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diag)
	cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
	if e := cmd.Execute(); e != nil {
		t.Fatalf("submit: %v %s %s", e, out.String(), diag.String())
	}
	return out.String(), diag.String()
}

func TestIssueSubmitValidationReuse(t *testing.T) {
	for _, kind := range []string{"same", "edited", "version", "dev", "dirty", "empty", "FAIL", "import", "base", "policy", "claim", "tree", "missing"} {
		t.Run(kind, func(t *testing.T) {
			old := Version
			Version = "v1.2.3"
			t.Cleanup(func() { Version = old })
			f, counter := countedValidationFixture(t)
			n := f.create(t, "agent")
			number := strconv.Itoa(n)
			f.must(t, "issue", "approve", number)
			f.must(t, "issue", "work", number, "--agent", "mock", "--progress=false")
			workEvidence := latestValidation(t, f, n)
			if workEvidence.Metadata["validation_version"] != Version || workEvidence.Metadata["validation_trusted_origin"] != "coordinator" {
				t.Fatal("work did not record reusable coordinator evidence")
			}
			wt := workspace.WorktreePath(f.root, n)
			if e := os.WriteFile(filepath.Join(wt, "uncommitted.txt"), []byte("checked\n"), 0644); e != nil {
				t.Fatal(e)
			}
			explicitBefore := checkCount(t, counter)
			f.must(t, "issue", "validate", number, "--json", "--progress=false")
			if checkCount(t, counter) != explicitBefore+1 {
				t.Fatal("explicit issue validate reused checks")
			}
			before := checkCount(t, counter)
			source := latestValidation(t, f, n)
			switch kind {
			case "edited":
				if e := os.WriteFile(filepath.Join(wt, "uncommitted.txt"), []byte("edited\n"), 0644); e != nil {
					t.Fatal(e)
				}
			case "version":
				Version = "v2.0.0"
			case "dev":
				Version = "dev"
			case "dirty":
				Version = "v1.2.3+dirty"
			case "empty":
				Version = ""
			case "FAIL", "import", "base", "policy", "claim", "tree", "missing":
				st := f.state(t, n)
				f.manager.BindClaim(n, st.Lease.ClaimID)
				if kind == "FAIL" {
					source.Verdict = "FAIL"
				}
				if kind == "base" || kind == "policy" || kind == "claim" || kind == "tree" {
					source.Metadata["validation_"+kind] = "different"
				}
				if kind == "missing" {
					delete(source.Metadata, "validation_tree")
				}
				writer := f.manager.RecordTrustedValidation
				if kind == "import" {
					writer = f.manager.UpdateArtifact
				}
				if _, e := writer(t.Context(), n, st.Lease.Owner, source); e != nil {
					t.Fatal(e)
				}
			}
			out, diag := submitWithDiagnostics(t, f, "issue", "submit", number, "--json", "--progress=false")
			if !json.Valid([]byte(out)) {
				t.Fatalf("invalid JSON: %s", out)
			}
			expected := before + 1
			if kind == "same" {
				expected = before
			}
			if got := checkCount(t, counter); got != expected {
				t.Fatalf("checks=%d want=%d; %s", got, expected, diag)
			}
			artifact := latestValidation(t, f, n)
			if kind == "same" {
				if !strings.Contains(diag, "Reusing validation") || !strings.Contains(diag, "ago, tree ") {
					t.Fatal(diag)
				}
				if artifact.Metadata["validation_reused"] != "true" {
					t.Fatal("missing reuse metadata")
				}
				pr, e := f.tracker.GetPullRequest(t.Context(), f.state(t, n).PRNumber)
				if e != nil {
					t.Fatal(e)
				}
				if !strings.Contains(pr.Body, "Reusing validation") {
					t.Fatal("PR lacks disclosure")
				}
			} else if artifact.Metadata["validation_reused"] == "true" {
				t.Fatal("mismatch reused")
			}
		})
	}
}

func TestPRSubmitValidationReuse(t *testing.T) {
	for _, kind := range []string{"same", "version", "dev", "dirty", "empty", "import", "FAIL", "base", "policy", "claim", "tree", "missing"} {
		t.Run(kind, func(t *testing.T) {
			old := Version
			Version = "v1.2.3"
			t.Cleanup(func() { Version = old })
			f, counter := countedValidationFixture(t)
			n := f.create(t, "agent")
			number := strconv.Itoa(n)
			f.must(t, "issue", "approve", number)
			f.must(t, "issue", "work", number, "--agent", "mock", "--progress=false")
			f.must(t, "issue", "submit", number)
			prNumber := strconv.Itoa(f.state(t, n).PRNumber)
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf revision > revision.txt")
			f.must(t, "pr", "work", prNumber, "--agent", "mock", "--progress=false")
			explicitBefore := checkCount(t, counter)
			f.must(t, "pr", "validate", prNumber, "--json", "--progress=false")
			if checkCount(t, counter) != explicitBefore+1 {
				t.Fatal("explicit PR validate reused checks")
			}
			before := checkCount(t, counter)
			switch kind {
			case "version":
				Version = "v2.0.0"
			case "dev":
				Version = "dev"
			case "dirty":
				Version = "v1.2.3+dirty"
			case "empty":
				Version = ""
			case "import", "FAIL", "base", "policy", "claim", "tree", "missing":
				st := f.state(t, n)
				f.manager.BindClaim(n, st.Lease.ClaimID)
				source := latestValidation(t, f, n)
				if kind == "FAIL" {
					source.Verdict = "FAIL"
				}
				if kind == "base" || kind == "policy" || kind == "claim" || kind == "tree" {
					source.Metadata["validation_"+kind] = "different"
				}
				if kind == "missing" {
					delete(source.Metadata, "validation_tree")
				}
				writer := f.manager.RecordTrustedValidation
				if kind == "import" {
					writer = f.manager.UpdateArtifact
				}
				if _, e := writer(t.Context(), n, st.Lease.Owner, source); e != nil {
					t.Fatal(e)
				}
			}
			out, diag := submitWithDiagnostics(t, f, "pr", "submit", prNumber, "--json", "--progress=false")
			result := readPROutcome(t, out)
			expected := before + 1
			if kind == "same" {
				expected = before
			}
			if got := checkCount(t, counter); got != expected {
				t.Fatalf("checks=%d want=%d; %s", got, expected, diag)
			}
			if kind == "same" {
				if result.Validation == nil || !result.Validation.Reused || !strings.Contains(diag, "Reusing validation") {
					t.Fatal("missing structured reuse or diagnostics")
				}
				pr, e := f.tracker.GetPullRequest(t.Context(), f.state(t, n).PRNumber)
				if e != nil {
					t.Fatal(e)
				}
				if !strings.Contains(pr.Body, "Reusing validation") {
					t.Fatal("PR lacks disclosure")
				}
			}
		})
	}
}

func TestSubmitValidationReuseFallbackFailureBlocksPublication(t *testing.T) {
	for _, resource := range []string{"issue", "pr"} {
		t.Run(resource, func(t *testing.T) {
			old := Version
			Version = "v1.2.3"
			t.Cleanup(func() { Version = old })
			f, counter := countedValidationFixture(t)
			n := f.create(t, "agent")
			number := strconv.Itoa(n)
			f.must(t, "issue", "approve", number)
			f.must(t, "issue", "work", number, "--agent", "mock", "--progress=false")
			if resource == "pr" {
				f.must(t, "issue", "submit", number)
				number = strconv.Itoa(f.state(t, n).PRNumber)
				f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf revision > revision.txt")
				f.must(t, "pr", "work", number, "--agent", "mock", "--progress=false", "--json")
			}
			before := checkCount(t, counter)
			published := f.state(t, n).HeadSHA
			// Force fallback for a changed coordinator version, then make the
			// trusted check fail through its external fixture control.
			Version = "v2.0.0"
			if e := os.WriteFile(counter+".fail", []byte("fail"), 0644); e != nil {
				t.Fatal(e)
			}

			if _, e := f.run(resource, "submit", number, "--progress=false"); e == nil {
				t.Fatal("published failed fallback")
			}
			if checkCount(t, counter) != before+1 {
				t.Fatal("fallback did not execute checks")
			}
			after := f.state(t, n)
			if after.Lease.Status == "completed" || after.HeadSHA != published {
				t.Fatal("failed fallback changed published state")
			}
		})
	}
}
