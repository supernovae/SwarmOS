package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
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
	cmd.SetArgs(append([]string{"--root", f.root}, args...))
	if e := cmd.Execute(); e != nil {
		t.Fatalf("submit: %v %s %s", e, out.String(), diag.String())
	}
	return out.String(), diag.String()
}

func TestDirectIssueWorkSubmitValidationReuse(t *testing.T) {
	for _, tc := range []struct {
		name, version, reason string
		checks                int
	}{
		{"eligible", "v1.2.3", "", 1},
		{"dirty", "v1.2.3+dirty", "the CLI build is dirty and ineligible for reuse", 2},
		{"edited", "v1.2.3", "the candidate tree changed since validation", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := Version
			Version = tc.version
			t.Cleanup(func() { Version = old })
			f, counter := countedValidationFixture(t)
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'checked\\n' > candidate.txt")
			f.resolverRunner = func(name string, cfg *contract.SwarmConfig, local bool) (agent.Runner, error) {
				runner, err := agent.NewConfiguredRunnerWithOptions(name, cfg, local, agent.RunnerOptions{LookupEnv: f.env.lookup()})
				return acceptanceFixtureRunner{runner}, err
			}
			n := f.create(t, "agent")
			number := strconv.Itoa(n)
			f.must(t, "issue", "approve", number)
			f.must(t, "issue", "work", number, "--agent", "mock", "--progress=false")
			source := latestValidation(t, f, n)
			md := source.Metadata
			wt := workspace.WorktreePath(f.root, n)
			head := prGit(t, wt, "rev-parse", "HEAD")
			tree := prGit(t, wt, "rev-parse", "HEAD^{tree}")
			claim := f.state(t, n).Lease.ClaimID
			if source.Verdict != "PASS" || md["validation_trusted_origin"] != "coordinator" ||
				md["validation_version"] != tc.version || md["validation_claim"] != claim ||
				md["head_sha"] != head || md["validation_tree"] != tree || md[validation.AcceptanceEvidenceKey] == "" {
				t.Fatal("work did not record trusted passing checks and tester evidence for its checkpoint")
			}
			t.Logf("work checkpoint %s, tree %s", head, tree)
			if got := checkCount(t, counter); got != 1 {
				t.Fatalf("work ran the check %d times, want 1", got)
			}
			if tc.name == "edited" {
				prGit(t, wt, "ls-files", "--error-unmatch", "candidate.txt")
				if err := os.WriteFile(filepath.Join(wt, "candidate.txt"), []byte("edited\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}

			out, diag, err := runProgressCommand(t, f, "issue", "submit", number, "--json", "--progress=true")
			if !json.Valid([]byte(out)) {
				t.Fatalf("invalid JSON stdout: %s", out)
			}
			if got := checkCount(t, counter); got != tc.checks {
				t.Fatalf("total checks = %d, want %d", got, tc.checks)
			}
			state := f.state(t, n)
			submitted := latestValidation(t, f, n)
			t.Logf("submit checkpoint %s, tree %s", submitted.Metadata["head_sha"], submitted.Metadata["validation_tree"])
			if submitted.Verdict != "PASS" || submitted.Metadata["validation_claim"] != claim {
				t.Fatal("submit did not record PASS under the same claim")
			}
			if tc.reason != "" {
				if submitted.Metadata["validation_reused"] == "true" {
					t.Fatal("submit recorded reuse after running fresh checks")
				}
				for _, line := range strings.Split(diag, "\n") {
					if strings.HasPrefix(line, "Running required checks:") && (strings.Contains(line, f.root) || strings.Contains(line, wt) || strings.Contains(line, counter) || strings.Contains(line, tc.version)) {
						t.Errorf("notice disclosed fixture data: %q", line)
					}
				}
				assertProgressOrder(t, diag, "Running required checks: "+tc.reason+"\n", `Check "counted" started`)
			}
			if tc.name == "edited" {
				if err == nil || !strings.Contains(err.Error(), "acceptance evidence blocks") || state.PRNumber != 0 ||
					submitted.Metadata["head_sha"] == head || submitted.Metadata["validation_tree"] == tree {
					t.Fatalf("edited submit did not record the changed candidate and refuse stale evidence: %v", err)
				}
				return
			}
			if err != nil || state.PRNumber == 0 {
				t.Fatalf("submit did not create a draft PR: %v\n%s", err, diag)
			}
			pr, err := f.tracker.GetPullRequest(t.Context(), state.PRNumber)
			if err != nil {
				t.Fatal(err)
			}
			if !pr.Draft || submitted.Metadata["head_sha"] != head || submitted.Metadata["validation_tree"] != tree {
				t.Fatal("submission did not preserve the work checkpoint in a draft PR")
			}
			checkStatus := "PASS"
			if tc.name == "eligible" {
				checkStatus = "REUSED"
				if !strings.Contains(diag, "Reusing validation") || submitted.Metadata["validation_reused"] != "true" {
					t.Fatalf("missing reuse evidence: %s", diag)
				}
				for _, want := range []string{
					"Source head: " + head + ";",
					"Original validation time: " + source.CreatedAt.Format(time.RFC3339Nano) + "; source claim: " + claim + "; candidate tree: " + tree,
				} {
					if !strings.Contains(pr.Body, want) {
						t.Errorf("draft PR missing %q", want)
					}
				}
			} else if !strings.Contains(pr.Body, "reused: false") {
				t.Error("draft PR missing fresh check evidence")
			}
			foundCheck := false
			for _, line := range strings.Split(pr.Body, "\n") {
				if strings.HasPrefix(line, "| counted |") && strings.Contains(line, "| "+checkStatus+" |") {
					foundCheck = true
					break
				}
			}
			if !foundCheck {
				t.Errorf("draft PR missing counted check with status %s", checkStatus)
			}
		})
	}
}

func TestIssueSubmitValidationReuse(t *testing.T) {
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
	baselineCounter, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	restore := snapshotCLIFixture(t, f)
	for _, kind := range []string{"same", "edited", "version", "dev", "dirty", "empty", "FAIL", "import", "base", "policy", "claim", "tree", "missing"} {
		t.Run(kind, func(t *testing.T) {
			restore(t)
			Version = "v1.2.3"
			if err := os.WriteFile(counter, baselineCounter, 0600); err != nil {
				t.Fatal(err)
			}
			explicitBefore := checkCount(t, counter)
			f.must(t, "issue", "validate", number, "--json", "--progress=false")
			if checkCount(t, counter) != explicitBefore+1 {
				t.Fatal("explicit issue validate reused checks")
			}
			seedCompleteIssueAcceptance(t, f, n)
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
			out, diag, submitErr := runProgressCommand(t, f, "issue", "submit", number, "--json", "--progress=false")
			// Submit checkpoints the dirty candidate to a new head. Check reuse
			// remains valid by tree; acceptance stays bound to the old head.
			if submitErr == nil || !strings.Contains(submitErr.Error(), "acceptance evidence blocks") || f.state(t, n).PRNumber != 0 {
				t.Fatalf("stale evidence submitted: %v", submitErr)
			}

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
			} else if artifact.Metadata["validation_reused"] == "true" {
				t.Fatal("mismatch reused")
			}
		})
	}
}

func TestPRSubmitValidationReuse(t *testing.T) {
	old := Version
	Version = "v1.2.3"
	t.Cleanup(func() { Version = old })
	f, counter := countedValidationFixture(t)
	n := f.create(t, "agent")
	number := strconv.Itoa(n)
	f.must(t, "issue", "approve", number)
	f.must(t, "issue", "work", number, "--agent", "mock", "--progress=false")
	seedCompleteIssueAcceptance(t, f, mustIssueNumber(number))
	f.must(t, "issue", "submit", number)
	prNumber := strconv.Itoa(f.state(t, n).PRNumber)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf revision > revision.txt")
	f.must(t, "pr", "work", prNumber, "--agent", "mock", "--progress=false")
	baselineCounter, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	restore := snapshotCLIFixture(t, f)
	for _, kind := range []string{"same", "version", "dev", "dirty", "empty", "import", "FAIL", "base", "policy", "claim", "tree", "missing"} {
		t.Run(kind, func(t *testing.T) {
			restore(t)
			Version = "v1.2.3"
			if err := os.WriteFile(counter, baselineCounter, 0600); err != nil {
				t.Fatal(err)
			}
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
				if !strings.Contains(pr.Body, "Reusing validation") || !strings.Contains(pr.Body, "Submitted head:") || !strings.Contains(pr.Body, "Source head:") || !strings.Contains(pr.Body, "Total wall time:") || !strings.Contains(pr.Body, "Current reuse wall time:") {
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
				seedCompleteIssueAcceptance(t, f, mustIssueNumber(number))
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
			// One original required check and one failed checkpoint diagnosis.
			// Neither execution may publish the failing candidate.
			if checkCount(t, counter) != before+2 {
				t.Fatal("fallback and checkpoint diagnosis did not both execute")
			}
			after := f.state(t, n)
			if after.Lease.Status == "completed" || after.HeadSHA != published {
				t.Fatal("failed fallback changed published state")
			}
		})
	}
}

func TestCheckpointSubmitValidationReuse(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	old := Version
	t.Cleanup(func() { Version = old })
	for _, version := range []string{"v1.2.3", "", "dev", "v1.2.3+dirty"} {
		t.Run(version, func(t *testing.T) {
			Version = version
			f, counter := countedValidationFixture(t)
			n := f.create(t, "agent")
			number := strconv.Itoa(n)
			f.must(t, "issue", "approve", number)
			f.must(t, "issue", "work", number, "--agent", "mock", "--progress=false")
			wt := workspace.WorktreePath(f.root, n)
			before := checkCount(t, counter)
			predecessor := prGit(t, wt, "rev-parse", "HEAD")
			prGit(t, wt, "commit", "--allow-empty", "-m", "recovered candidate")
			candidate := prGit(t, wt, "rev-parse", "HEAD")
			args := []string{"issue", "reconcile", number, "--checkpoint", "--expected-head", predecessor, "--candidate-head", candidate, "--json"}
			stateBefore, _ := json.Marshal(f.state(t, n))
			dryOut, dryDiag, dryErr := runProgressCommand(t, f, append(append([]string{}, args...), "--dry-run")...)
			stateAfter, _ := json.Marshal(f.state(t, n))
			if dryErr != nil || !json.Valid([]byte(dryOut)) || dryDiag != "" || !bytes.Equal(stateBefore, stateAfter) || checkCount(t, counter) != before {
				t.Fatalf("dry run changed state/output/checks: %v %s", dryErr, dryDiag)
			}
			opts := f.options(nil)
			lookup := opts.lookupEnv
			lookupCalls := 0
			opts.lookupEnv = func(key string) (string, bool) {
				if key == "SWARM_EXECUTION_MODE" {
					lookupCalls++
				}
				return lookup(key)
			}
			cmd := newRootCmdWithOptions(opts)
			var out, diag bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&diag)
			if version == "dev" {
				args = append(args, "--progress=false")
			}
			cmd.SetArgs(append([]string{"--root", f.root}, args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("reconcile: %v %s", err, diag.String())
			}
			if !json.Valid(out.Bytes()) {
				t.Fatalf("invalid JSON: %s", out.String())
			}
			if lookupCalls == 0 {
				t.Fatal("validator did not use CLI environment")
			}
			if version == "dev" && diag.Len() != 0 {
				t.Fatal("disabled progress emitted stderr")
			}
			if version != "dev" && (!strings.Contains(diag.String(), "validation") || !strings.Contains(diag.String(), `"counted"`) || !strings.Contains(diag.String(), `"snapshot preparation" PASS`) || !strings.Contains(diag.String(), `"checkpoint publication" PASS`)) {
				t.Fatalf("missing progress: %s", diag.String())
			}
			if got := checkCount(t, counter); got != before+1 {
				t.Fatalf("reconcile count %d want %d", got, before+1)
			}
			source := latestValidation(t, f, n)
			if source.Metadata["validation_trusted_claim"] != f.state(t, n).Lease.ClaimID {
				t.Fatal("unbound reconciliation")
			}
			seedCompleteIssueAcceptance(t, f, n)
			_, submitDiag := submitWithDiagnostics(t, f, "issue", "submit", number, "--json", "--progress=false")
			want := before + 2
			if version == "v1.2.3" {
				want = before + 1
			}
			if got := checkCount(t, counter); got != want {
				t.Fatalf("submit count %d want %d; %s", got, want, submitDiag)
			}
			if version == "v1.2.3" {
				a := latestValidation(t, f, n)
				if a.Metadata["validation_reused"] != "true" || a.Metadata["head_sha"] != candidate || a.Metadata["validation_source_time"] != source.CreatedAt.Format("2006-01-02T15:04:05.999999999Z07:00") {
					t.Fatalf("missing source provenance: %v", a.Metadata)
				}
			} else if source.Metadata["validation_version"] != "" {
				t.Fatal("legacy version gained fingerprint")
			}
		})
	}
}

func TestIssue396ValidationParity(t *testing.T) {
	t.Parallel()
	f, counter := countedValidationFixture(t)
	policy := fmt.Sprintf("required_checks:\n  - name: instructions\n    command: >-\n      printf x >> '%s'; test ! -e CLAUDE.md && ! grep -q swarm:active AGENTS.md\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**', 'AGENTS.md', 'CLAUDE.md']\n", counter)
	for name, body := range map[string]string{".swarm/validation/required-checks.yaml": policy, "AGENTS.md": "Repository guide\n"} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	prGit(t, f.root, "add", ".")
	prGit(t, f.root, "commit", "-m", "instruction check")
	n := f.create(t, "agent")
	number := strconv.Itoa(n)
	f.must(t, "issue", "approve", number)
	f.must(t, "issue", "work", number, "--agent", "mock", "--progress=false")
	if latestValidation(t, f, n).Verdict != "PASS" {
		t.Fatal("work failed")
	}
	wt := workspace.WorktreePath(f.root, n)
	pointer := "<!-- swarm:active:start -->\nTask\n<!-- swarm:active:end -->\n\n"
	inject := func() {
		for name, body := range map[string]string{"AGENTS.md": pointer + "Repository guide\n", "CLAUDE.md": pointer} {
			if err := os.WriteFile(filepath.Join(wt, name), []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	inject()
	before := checkCount(t, counter)
	f.must(t, "issue", "validate", number, "--progress=false")
	if checkCount(t, counter) != before+1 {
		t.Fatal("standalone checks did not execute")
	}
	for name, want := range map[string]string{"AGENTS.md": pointer + "Repository guide\n", "CLAUDE.md": pointer} {
		got, err := os.ReadFile(filepath.Join(wt, name))
		if err != nil || string(got) != want {
			t.Fatalf("standalone restoration %s: %q %v", name, got, err)
		}
	}
	if err := os.WriteFile(filepath.Join(wt, "AGENTS.md"), []byte(pointer+"Repository guide\nUnapproved edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := f.command(nil)
	cmd.SetArgs([]string{"--root", f.root, "issue", "validate", number, "--progress=false"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected edit: %v", err)
	}
	inject()
	f.must(t, "issue", "validate", number, "--progress=false")
	seedCompleteIssueAcceptance(t, f, n)
	submitWithDiagnostics(t, f, "issue", "submit", number, "--json", "--progress=false")
	if latestValidation(t, f, n).Verdict != "PASS" {
		t.Fatal("submit differs from work/validate")
	}
}

// issue470FreshClaim uses real expiry/acquisition, preserving A's evidence.
func issue470FreshClaim(t *testing.T) (*cliFixture, string, int, string) {
	t.Helper()
	f, counter := countedValidationFixture(t)
	policyPath := filepath.Join(f.root, ".swarm/validation/required-checks.yaml")
	policy, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	policy = []byte(strings.Replace(string(policy), "protected_paths:", fmt.Sprintf("  - name: second\n    command: \"printf y >> '%s'\"\n    timeout: 1m\nprotected_paths:", counter), 1))
	if err := os.WriteFile(policyPath, policy, 0644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".")
	prGit(t, f.root, "commit", "-m", "two checks")
	n := f.create(t, "agent")
	number := strconv.Itoa(n)
	f.must(t, "issue", "approve", number)
	f.must(t, "issue", "work", number, "--agent", "mock", "--progress=false")
	a := f.state(t, n)
	f.manager.SetNowFunc(func() time.Time { return a.Lease.ExpiresAt.Add(time.Second) })
	b, err := f.manager.Acquire(t.Context(), n, a.Lease.Owner, "mock", "implement", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Lease.ClaimID == b.Lease.ClaimID {
		t.Fatal("claim did not change")
	}
	f.manager.SetNowFunc(time.Now)
	if err := lease.SaveClaimToken(f.root, n, b.Lease.ClaimID, b.BaseSHA); err != nil {
		t.Fatal(err)
	}
	return f, counter, n, a.Lease.ClaimID
}

func TestIssue470FreshClaimValidationReuse(t *testing.T) {
	old := Version
	Version = "v1.2.3"
	t.Cleanup(func() { Version = old })
	for _, checkpoint := range []bool{false, true} {
		t.Run(strconv.FormatBool(checkpoint), func(t *testing.T) {
			f, counter, n, a := issue470FreshClaim(t)
			number := strconv.Itoa(n)
			before := checkCount(t, counter)
			wt := workspace.WorktreePath(f.root, n)
			workOut, workDiag, workErr := runProgressCommand(t, f, "issue", "work", number, "--agent", "mock", "--progress=false")
			if workErr != nil {
				t.Fatalf("work: %v %s %s", workErr, workOut, workDiag)
			}
			for _, want := range []string{"REUSED counted", "REUSED second"} {
				if !strings.Contains(workOut, want) {
					t.Fatalf("work output missing %s: %s", want, workOut)
				}
			}
			if checkCount(t, counter) != before {
				t.Fatal("work executed checks")
			}
			art := latestValidation(t, f, n)
			if art.Metadata["validation_source_claim"] != a {
				t.Fatalf("lost source: %v", art.Metadata)
			}
			// Real trusted-writer round trip: work recorded A's reused PASS under B.
			st := f.state(t, n)
			opts := validation.ValidateOptions{BaseSHA: st.BaseSHA, Version: Version, ClaimID: st.Lease.ClaimID, DirectIssueReuse: true}
			reused, err := validation.Reuse(t.Context(), wt, opts, st.Lease.Owner, st.Artifacts)
			if err != nil || reused == nil {
				t.Fatalf("B reuse: %v", err)
			}
			if reused.Artifact.Metadata["validation_source_time"] != art.Metadata["validation_source_time"] {
				t.Fatal("time renewed")
			}
			body, err := os.ReadFile(filepath.Join(wt, ".swarm/active/VALIDATION.md"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(body), "| **REUSED** |") != 2 {
				t.Fatalf("missing reused check rows: %s", body)
			}
			for _, value := range []string{a, art.Metadata["validation_source_time"], art.Metadata["validation_tree"], art.Metadata["validation_policy"], "REUSED"} {
				if !strings.Contains(string(body), value) {
					t.Fatalf("report missing %q", value)
				}
			}
			if checkpoint {
				prGit(t, wt, "commit", "--allow-empty", "-m", "same tree")
				st := f.state(t, n)
				f.manager.BindClaim(n, st.Lease.ClaimID)
				if _, err := f.manager.RecordCheckpoint(t.Context(), n, st.Lease.Owner, st.Branch, prGit(t, wt, "rev-parse", "HEAD")); err != nil {
					t.Fatal(err)
				}
			}
			// Rebind the reused check report to the checkpoint head before supplying
			// independent acceptance evidence for that exact head.
			st = f.state(t, n)
			rebound, err := validation.Reuse(t.Context(), wt, opts, st.Lease.Owner, st.Artifacts)
			if err != nil || rebound == nil {
				t.Fatalf("checkpoint reuse: %v", err)
			}
			f.manager.BindClaim(n, st.Lease.ClaimID)
			if _, err = f.manager.RecordTrustedValidation(t.Context(), n, st.Lease.Owner, rebound.Artifact); err != nil {
				t.Fatal(err)
			}

			seedCompleteIssueAcceptance(t, f, n)
			submitWithDiagnostics(t, f, "issue", "submit", number, "--progress=false")
			if checkCount(t, counter) != before {
				t.Fatal("submit executed checks")
			}
			pr, err := f.tracker.GetPullRequest(t.Context(), f.state(t, n).PRNumber)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{a, art.Metadata["validation_source_time"], art.Metadata["validation_tree"], art.Metadata["validation_policy"], "REUSED"} {
				if !strings.Contains(pr.Body, value) {
					t.Fatalf("PR missing %q", value)
				}
			}
		})
	}
}

type issue470CountingTracker struct {
	tracker.Tracker
	creates, updates int
}

func (tr *issue470CountingTracker) CreateDraftPR(ctx context.Context, r tracker.DraftPRRequest) (*tracker.PullRequest, error) {
	tr.creates++
	return tr.Tracker.CreateDraftPR(ctx, r)
}
func (tr *issue470CountingTracker) UpdateDraftPR(ctx context.Context, n int, title, body string) (*tracker.PullRequest, error) {
	tr.updates++
	return tr.Tracker.UpdateDraftPR(ctx, n, title, body)
}

func TestIssue470StaleClaimCannotCheckpointOrSubmit(t *testing.T) {
	old := Version
	Version = "v1.2.3"
	t.Cleanup(func() { Version = old })
	for _, operation := range []string{"submit", "stage"} {
		t.Run(operation, func(t *testing.T) {
			f, _, n, a := issue470FreshClaim(t)
			number := strconv.Itoa(n)
			st := f.state(t, n)
			wt := workspace.WorktreePath(f.root, n)
			reused, err := validation.Reuse(t.Context(), wt, validation.ValidateOptions{BaseSHA: st.BaseSHA, ClaimID: st.Lease.ClaimID, Version: Version, DirectIssueReuse: true}, st.Lease.Owner, st.Artifacts)
			if err != nil || reused == nil {
				t.Fatalf("B cannot reuse: %v", err)
			}
			seedCompleteIssueAcceptance(t, f, n)
			remote := filepath.Join(t.TempDir(), "remote.git")
			prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
			prGit(t, f.root, "remote", "add", "origin", remote)
			prGit(t, f.root, "push", "origin", "main")
			remoteBefore := prGit(t, f.root, "ls-remote", "--refs", "origin")
			if err := os.WriteFile(filepath.Join(wt, "pending.txt"), []byte("checkpointable"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := lease.SaveClaimToken(f.root, n, a, st.BaseSHA); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(f.state(t, n))
			rawBefore, revisionBefore, err := f.manager.Store().Load(t.Context(), n)
			if err != nil {
				t.Fatal(err)
			}
			head := prGit(t, wt, "rev-parse", "HEAD")
			refs := prGit(t, f.root, "show-ref")
			tr := &issue470CountingTracker{Tracker: f.tracker}
			cmd := f.command(&RootOptions{trackerOverride: tr})
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			args := []string{"--root", f.root, "issue", operation, number}
			if operation == "stage" {
				args = append(args, "--phase", "implement")
			}
			cmd.SetArgs(args)
			err = cmd.Execute()
			if !errors.Is(err, lease.ErrClaimFence) {
				t.Fatalf("want fence, got %v: %s", err, output.String())
			}
			after, _ := json.Marshal(f.state(t, n))
			rawAfter, revisionAfter, err := f.manager.Store().Load(t.Context(), n)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rawBefore, rawAfter) || revisionBefore != revisionAfter {
				t.Fatal("CAS changed")
			}
			if remoteBefore != prGit(t, f.root, "ls-remote", "--refs", "origin") {
				t.Fatal("remote refs changed")
			}
			if !bytes.Equal(before, after) || head != prGit(t, wt, "rev-parse", "HEAD") || refs != prGit(t, f.root, "show-ref") || tr.creates != 0 || tr.updates != 0 {
				t.Fatalf("stale effects: state equal=%t HEAD=%s -> %s refs=%s -> %s creates=%d updates=%d", bytes.Equal(before, after), head, prGit(t, wt, "rev-parse", "HEAD"), refs, prGit(t, f.root, "show-ref"), tr.creates, tr.updates)
			}
		})
	}
}
