package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/coordination"
)

// harnessGrantFixture submits a PR for an approved allow_harness_changes
// WorkUnit whose checkpoint edits protected policy, then checks out the
// submitted head in the main checkout as CI would.
func harnessGrantFixture(t *testing.T, setup ...func(*cliFixture)) (f *cliFixture, issue, pr int, base, head string) {
	t.Helper()
	f = newCLIFixture(t)
	for _, fn := range setup {
		fn(f)
	}
	f.create(t, "agent") // PR and issue numbers deliberately differ.
	issue = f.create(t, "agent")
	ctx := context.Background()
	iss, err := f.tracker.GetIssue(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tracker.UpdateIssueBody(ctx, issue, iss.Body+"\n\n### Allow Harness Changes\n\ntrue\n"); err != nil {
		t.Fatal(err)
	}
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf '# approved harness edit\\n' >> .swarm/validation/required-checks.yaml")
	f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
	f.must(t, "issue", "submit", n)
	st := f.state(t, issue)
	if st.PRNumber == 0 || st.PRNumber == issue || st.SubmittedHeadSHA == "" {
		t.Fatalf("fixture did not submit a distinct PR: %+v", st)
	}
	gitRun(t, f.root, "checkout", "-q", "--detach", st.SubmittedHeadSHA)
	return f, issue, st.PRNumber, st.BaseSHA, st.SubmittedHeadSHA
}

func gitRun(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root, "-c", "user.name=T", "-c", "user.email=t@example.invalid"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestCLIRepoValidateHonorsApprovedHarnessGrant(t *testing.T) {
	t.Parallel()
	f, issue, pr, base, head := harnessGrantFixture(t)
	i, p := strconv.Itoa(issue), strconv.Itoa(pr)

	if _, err := f.run("repo", "validate", "--base", base); err == nil {
		t.Fatal("protected change passed without a linked issue")
	}
	out := f.must(t, "repo", "validate", "--base", base, "--issue", i, "--pr", p)
	for _, want := range []string{"NOTE protected changes authorized by approved WorkUnit #" + i, "PASS git-available"} {
		if !strings.Contains(out, want) {
			t.Fatalf("approved grant output missing %q:\n%s", want, out)
		}
	}
	out, err := f.run("repo", "validate", "--base", base, "--issue", i, "--pr", strconv.Itoa(pr+100))
	if err == nil || !strings.Contains(out, "does not authorize protected changes") || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("another PR number was granted: %v\n%s", err, out)
	}

	// GitHub checks out a merge commit whose first parent is the base and
	// whose second parent is the submitted PR head.
	gitRun(t, f.root, "checkout", "-q", "--detach", base)
	gitRun(t, f.root, "merge", "-q", "--no-ff", "-m", "GitHub merge commit", head)
	if out := f.must(t, "repo", "validate", "--base", base, "--issue", i, "--pr", p); !strings.Contains(out, "NOTE protected changes authorized") {
		t.Fatalf("merge checkout not granted:\n%s", out)
	}

	// A new commit on top of the submitted head is not the recorded revision.
	gitRun(t, f.root, "checkout", "-q", "--detach", head)
	gitRun(t, f.root, "commit", "-q", "--allow-empty", "-m", "unreviewed follow-up")
	if out, err := f.run("repo", "validate", "--base", base, "--issue", i, "--pr", p); err == nil {
		t.Fatalf("unsubmitted head was granted:\n%s", out)
	}
	gitRun(t, f.root, "checkout", "-q", "--detach", head)

	// Editing the issue after approval breaks the task digest binding.
	ctx := context.Background()
	iss, err := f.tracker.GetIssue(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tracker.UpdateIssueBody(ctx, issue, iss.Body+"\nEdited after approval.\n"); err != nil {
		t.Fatal(err)
	}
	out, err = f.run("repo", "validate", "--base", base, "--issue", i, "--pr", p)
	if err == nil || !strings.Contains(err.Error(), "protected path") || !strings.Contains(out, "task or policy changed since approval") {
		t.Fatalf("issue edited after approval was granted: %v\n%s", err, out)
	}
}

// Issue text and readiness labels grant nothing without authoritative state.
func TestCLIRepoValidateHarnessGrantNeedsAuthoritativeState(t *testing.T) {
	t.Parallel()
	f, issue, pr, base, _ := harnessGrantFixture(t)
	if err := f.tracker.AddLabels(context.Background(), issue, []string{"swarm:ready"}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.root, ".swarm", "coordination")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordination.NewFileStore(f.root).Load(context.Background(), issue); err == nil {
		t.Fatal("fixture state still present")
	}
	out, err := f.run("repo", "validate", "--base", base, "--issue", strconv.Itoa(issue), "--pr", strconv.Itoa(pr))
	if err == nil || !strings.Contains(err.Error(), "protected path") || !strings.Contains(out, "does not authorize protected changes") {
		t.Fatalf("issue text without state was granted: %v\n%s", err, out)
	}
}

func TestCLIRepoValidateHarnessGrantFlags(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	base := gitRun(t, f.root, "rev-parse", "HEAD")
	grant := filepath.Join(t.TempDir(), "grant.json")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"issue without pr", []string{"--issue", "3"}, "--issue and --pr"},
		{"pr without issue", []string{"--pr", "4"}, "--issue and --pr"},
		{"report mode", []string{"--issue", "3", "--pr", "4", "--protected-paths", "report"}, "only to --protected-paths enforce"},
		{"grant file in report mode", []string{"--grant-file", grant, "--protected-paths", "report"}, "only to --protected-paths enforce"},
		{"grant-only without file", []string{"--issue", "3", "--pr", "4", "--grant-only"}, "--grant-only requires"},
		{"grant-only without issue", []string{"--grant-only", "--grant-file", grant}, "--grant-only requires"},
		{"live and file grant together", []string{"--issue", "3", "--pr", "4", "--grant-file", grant}, "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.run(append([]string{"repo", "validate", "--base", base}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	// Fork mode needs the github backend, so run past the local fixture flags.
	cmd := f.command(nil)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--root", f.root, "--backend", "github", "--fork", "repo", "validate", "--base", base, "--issue", "3", "--pr", "4"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cannot be used with --fork") {
		t.Fatalf("fork grant error = %v (%s)", err, output.String())
	}
}

func TestRepoValidateGrantOnlyRunsNoChecks(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "ran")
	// The trusted check leaves a marker whenever it executes.
	f, issue, pr, base, head := harnessGrantFixture(t, func(f *cliFixture) {
		policy := "required_checks:\n  - name: marker\n    command: touch " + strconv.Quote(marker) + "\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
		if err := os.WriteFile(filepath.Join(f.root, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, f.root, "add", ".")
		gitRun(t, f.root, "commit", "-q", "-m", "marker policy")
	})
	if err := os.Remove(marker); err != nil {
		t.Fatalf("fixture check never ran: %v", err)
	}
	i, p := strconv.Itoa(issue), strconv.Itoa(pr)
	grant := filepath.Join(t.TempDir(), "grant.json")
	out := f.must(t, "repo", "validate", "--base", base, "--issue", i, "--pr", p, "--grant-only", "--grant-file", grant)
	if !strings.Contains(out, "NOTE protected changes authorized") || strings.Contains(out, "PASS") {
		t.Fatalf("grant-only output:\n%s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("grant-only executed a check")
	}
	f.must(t, "repo", "validate", "--base", base, "--grant-file", grant)
	if err := os.Remove(marker); err != nil {
		t.Fatalf("validate step did not run the trusted check: %v", err)
	}
	var record harnessGrantRecord
	data, err := os.ReadFile(grant)
	if err != nil || json.Unmarshal(data, &record) != nil {
		t.Fatalf("grant file: %v %s", err, data)
	}
	if record.Verdict != "granted" || record.Issue != issue || record.PR != pr || record.Base != base || record.CandidateHead != head ||
		record.TaskDigest == "" || record.PolicyDigest == "" || record.PolicyTreeDigest == "" || record.WorkUnit == nil {
		t.Fatalf("incomplete grant record: %+v", record)
	}

	// A refused grant writes no file, removes a stale one, and still exits 0.
	out = f.must(t, "repo", "validate", "--base", base, "--issue", i, "--pr", strconv.Itoa(pr+100), "--grant-only", "--grant-file", grant)
	if !strings.Contains(out, "does not authorize protected changes") {
		t.Fatalf("refused grant-only output:\n%s", out)
	}
	if _, err := os.Stat(grant); !os.IsNotExist(err) {
		t.Fatal("refused grant left a grant file")
	}
}

func TestRepoValidateGrantFileRechecked(t *testing.T) {
	t.Parallel()
	f, issue, pr, base, head := harnessGrantFixture(t)
	grant := filepath.Join(t.TempDir(), "grant.json")
	f.must(t, "repo", "validate", "--base", base, "--issue", strconv.Itoa(issue), "--pr", strconv.Itoa(pr), "--grant-only", "--grant-file", grant)
	data, err := os.ReadFile(grant)
	if err != nil {
		t.Fatal(err)
	}
	// Grant files are verified without credentials, so drop the state the
	// grant step read: only the file can authorize from here on.
	if err := os.RemoveAll(filepath.Join(f.root, ".swarm", "coordination")); err != nil {
		t.Fatal(err)
	}
	if out := f.must(t, "repo", "validate", "--base", base, "--grant-file", grant); !strings.Contains(out, "NOTE protected changes authorized by approved WorkUnit #"+strconv.Itoa(issue)) || !strings.Contains(out, "PASS git-available") {
		t.Fatalf("verified grant file not honored:\n%s", out)
	}
	var record harnessGrantRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(r *harnessGrantRecord) []byte
	}{
		{"other base", func(r *harnessGrantRecord) []byte { r.Base = head; return nil }},
		{"other candidate head", func(r *harnessGrantRecord) []byte { r.CandidateHead = base; return nil }},
		{"other policy digest", func(r *harnessGrantRecord) []byte { r.PolicyDigest = strings.Repeat("0", 64); return nil }},
		{"other policy tree", func(r *harnessGrantRecord) []byte { r.PolicyTreeDigest = strings.Repeat("0", 40); return nil }},
		{"WorkUnit edited", func(r *harnessGrantRecord) []byte { r.WorkUnit.Objective += " and more"; return nil }},
		{"WorkUnit without exception", func(r *harnessGrantRecord) []byte { r.WorkUnit.AllowHarnessChanges = false; return nil }},
		{"refused verdict", func(r *harnessGrantRecord) []byte { r.Verdict = "refused"; return nil }},
		{"malformed", func(*harnessGrantRecord) []byte { return []byte("{not json") }},
		{"missing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "grant.json")
			if tc.mutate != nil {
				var copy harnessGrantRecord
				if err := json.Unmarshal(data, &copy); err != nil {
					t.Fatal(err)
				}
				raw := tc.mutate(&copy)
				if raw == nil {
					raw, _ = json.Marshal(copy)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := f.run("repo", "validate", "--base", base, "--grant-file", path)
			if err == nil || !strings.Contains(err.Error(), "protected path") || !strings.Contains(out, "NOTE harness grant ignored") {
				t.Fatalf("tampered grant file honored: %v\n%s", err, out)
			}
		})
	}
}
