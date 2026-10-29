package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Retarget only the disposable local tracker record, with no concurrent readers.
func retargetIssue102(t *testing.T, root string, number int, branch string) {
	t.Helper()
	path := filepath.Join(root, ".swarm/local-tracker/state.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	var prs map[string]map[string]json.RawMessage
	if err := json.Unmarshal(state["pull_requests"], &prs); err != nil {
		t.Fatal(err)
	}
	prs[strconv.Itoa(number)]["base"], err = json.Marshal(branch)
	if err != nil {
		t.Fatal(err)
	}
	state["pull_requests"], err = json.Marshal(prs)
	if err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestIssue102LocalApprovalMatrix(t *testing.T) {
	for _, scenario := range []string{"default", "config", "override", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCLIFixture(t)
			number := f.create(t, "none")
			n := strconv.Itoa(number)
			initial := prGit(t, f.root, "rev-parse", "HEAD")
			prGit(t, f.root, "branch", "release/1.x")
			prGit(t, f.root, "branch", "lts")
			if scenario != "default" {
				path := filepath.Join(f.root, ".swarm/config.yaml")
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append([]byte("base_branch: release/1.x\n"), b...), 0600); err != nil {
					t.Fatal(err)
				}
				prGit(t, f.root, "add", ".swarm/config.yaml")
				prGit(t, f.root, "commit", "-m", "select release")
			}
			if scenario == "override" || scenario == "missing" {
				issue, err := f.tracker.GetIssue(context.Background(), number)
				if err != nil {
					t.Fatal(err)
				}
				branch := "lts"
				if scenario == "missing" {
					branch = "missing-102"
				}
				body := issue.Body + "\n### Base Branch\n\n" + branch + "\n"
				if _, err := f.tracker.UpdateIssueBody(context.Background(), number, body); err != nil {
					t.Fatal(err)
				}
			}
			out, err := f.run("issue", "approve", n)
			if scenario == "missing" {
				if err == nil || !strings.Contains(err.Error(), "missing-102") {
					t.Fatalf("missing accepted: %v %s", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("approve: %v %s", err, out)
			}
			state := f.state(t, number)
			expected := ""
			if scenario == "config" {
				expected = "release/1.x"
			}
			if scenario == "override" {
				expected = "lts"
			}
			if state.BaseBranch != expected || state.BaseSHA != initial {
				t.Fatalf("wrong target: %+v", state)
			}
			f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
			before := f.state(t, number)
			stateBefore, err := json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			refsBefore := prGit(t, f.root, "show-ref")
			wt := workspace.WorktreePath(f.root, number)
			headBefore := prGit(t, wt, "rev-parse", "HEAD")
			trackerPath := filepath.Join(f.root, ".swarm/local-tracker/state.json")
			trackerBefore, err := os.ReadFile(trackerPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, base := range []string{"", "different-102"} {
				if out, err := f.run("issue", "submit", n, "--base", base); err == nil || !strings.Contains(err.Error(), "approved branch") {
					t.Fatalf("mismatch accepted: %v %s", err, out)
				}
			}
			after := f.state(t, number)
			stateAfter, err := json.Marshal(after)
			if err != nil {
				t.Fatal(err)
			}
			trackerAfter, err := os.ReadFile(trackerPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(stateBefore) != string(stateAfter) || string(trackerBefore) != string(trackerAfter) || refsBefore != prGit(t, f.root, "show-ref") || headBefore != prGit(t, wt, "rev-parse", "HEAD") {
				t.Fatal("base mismatch changed state, projections, refs or checkpoint")
			}
			if after.PRNumber != 0 || after.HeadSHA != before.HeadSHA {
				t.Fatal("mismatch published")
			}
			f.must(t, "issue", "submit", n)
			state = f.state(t, number)
			pr, err := f.tracker.GetPullRequest(context.Background(), state.PRNumber)
			if err != nil {
				t.Fatal(err)
			}
			if expected == "" {
				expected = "main"
			}
			if pr.Base != expected {
				t.Fatalf("PR target %s want %s", pr.Base, expected)
			}
			// Completed retries must still enforce the explicit flag.
			if _, err := f.run("issue", "submit", n, "--base", ""); err == nil {
				t.Fatal("completed retry bypassed base check")
			}
		})
	}
}

func TestIssue102LocalRetargetBeforeMutation(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	before, _ := json.Marshal(f.state(t, issue))
	retargetIssue102(t, f.root, number, "other-102")
	for _, args := range [][]string{{"pr", "status", strconv.Itoa(number)}, {"pr", "rebase", strconv.Itoa(number)}, {"pr", "rebase", strconv.Itoa(number), "--abort"}, {"pr", "rebase", strconv.Itoa(number), "--continue"}, {"pr", "rebase", strconv.Itoa(number), "--resolve"}} {
		if out, err := f.run(args...); err == nil || !strings.Contains(err.Error(), "other-102") || !strings.Contains(err.Error(), "main") {
			t.Fatalf("retarget accepted: %v %s", err, out)
		}
	}
	after, _ := json.Marshal(f.state(t, issue))
	if string(before) != string(after) {
		t.Fatal("retarget refusal changed coordination state")
	}
}

func TestIssue102DeletedRemoteTargetRefusesCachedSubmissionAndPR(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent", func(f *cliFixture) {
		prGit(t, f.root, "branch", "release")
		path := filepath.Join(f.root, ".swarm/config.yaml")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append([]byte("base_branch: release\n"), b...), 0600); err != nil {
			t.Fatal(err)
		}
		prGit(t, f.root, "add", ".swarm/config.yaml")
		prGit(t, f.root, "commit", "-m", "select release")
	})
	remote := filepath.Join(t.TempDir(), "upstream.git")
	prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
	prGit(t, f.root, "remote", "add", "origin", remote)
	prGit(t, f.root, "push", "origin", "main", "release")
	f.must(t, "pr", "status", strconv.Itoa(number)) // Fill the tracking cache.
	prGit(t, remote, "update-ref", "-d", "refs/heads/release")
	before, _ := json.Marshal(f.state(t, issue))
	for _, args := range [][]string{{"issue", "approve", strconv.Itoa(issue)}, {"issue", "submit", strconv.Itoa(issue)}, {"pr", "status", strconv.Itoa(number)}, {"pr", "rebase", strconv.Itoa(number)}} {
		if out, err := f.run(args...); err == nil || !strings.Contains(err.Error(), "release") {
			t.Fatalf("cached target accepted: %v %s", err, out)
		}
	}
	after, _ := json.Marshal(f.state(t, issue))
	if string(before) != string(after) {
		t.Fatal("missing target mutated state")
	}
}

func TestIssue102PRTracksReleaseNotDefault(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent", func(f *cliFixture) {
		prGit(t, f.root, "branch", "release")
		path := filepath.Join(f.root, ".swarm/config.yaml")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append([]byte("base_branch: release\n"), b...), 0600); err != nil {
			t.Fatal(err)
		}
		prGit(t, f.root, "add", ".swarm/config.yaml")
		prGit(t, f.root, "commit", "-m", "select release")
	})
	before := f.state(t, issue)
	prGit(t, f.root, "commit", "--allow-empty", "-m", "default moved")
	result := readPROutcome(t, f.must(t, "pr", "status", strconv.Itoa(number), "--json"))
	if result.PR.BaseSHA != before.BaseSHA {
		t.Fatal("release status followed default")
	}
	prGit(t, f.root, "update-ref", "refs/heads/release", prGit(t, f.root, "rev-parse", "HEAD"))
	result = readPROutcome(t, f.must(t, "pr", "status", strconv.Itoa(number), "--json"))
	if result.PR.BaseSHA == before.BaseSHA {
		t.Fatal("release movement not observed")
	}
	if out, err := f.run("pr", "rebase", strconv.Itoa(number)); err == nil || !strings.Contains(err.Error(), "approve") {
		t.Fatalf("rebase ignored moved release: %v %s", err, out)
	}
}

func TestTester102ExplicitTargetGrantRoundTrip(t *testing.T) {
	for _, scenario := range []string{"main", "release", "override", "unset"} {
		t.Run(scenario, func(t *testing.T) {
			setup := func(f *cliFixture) {
				if scenario == "unset" {
					return
				}
				path := filepath.Join(f.root, ".swarm/config.yaml")
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				target := scenario
				if scenario == "override" {
					target = "release"
				}
				if target == "release" {
					gitRun(t, f.root, "checkout", "-b", "release")
					gitRun(t, f.root, "commit", "--allow-empty", "-m", "distinct release tip")
					gitRun(t, f.root, "checkout", "main")
				}
				if err := os.WriteFile(path, append([]byte("base_branch: "+target+"\n"), original...), 0600); err != nil {
					t.Fatal(err)
				}
				gitRun(t, f.root, "add", ".swarm/config.yaml")
				gitRun(t, f.root, "commit", "-m", "trusted target selection")
			}
			var f *cliFixture
			var issue, pr int
			var base string
			if scenario != "override" {
				f, issue, pr, base, _ = harnessGrantFixture(t, setup)
			} else {
				f = newCLIFixture(t)
				setup(f)
				issue = f.create(t, "agent")
				iss, err := f.tracker.GetIssue(context.Background(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.tracker.UpdateIssueBody(context.Background(), issue, iss.Body+"\n### Base Branch\n\nrelease\n\n### Allow Harness Changes\n\ntrue\n"); err != nil {
					t.Fatal(err)
				}
				// Make the default selection conflict with the explicit WorkUnit target.
				path := filepath.Join(f.root, ".swarm/config.yaml")
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(b), "base_branch: release", "base_branch: main", 1)), 0600); err != nil {
					t.Fatal(err)
				}
				gitRun(t, f.root, "add", ".swarm/config.yaml")
				gitRun(t, f.root, "commit", "-m", "conflicting default selection")
				n := strconv.Itoa(issue)
				f.must(t, "issue", "approve", n)
				f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf '# approved harness edit\\n' >> .swarm/validation/required-checks.yaml")
				f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
				f.must(t, "issue", "submit", n, "--base", "release")
				st := f.state(t, issue)
				pr, base = st.PRNumber, st.BaseSHA
				gitRun(t, f.root, "checkout", "--detach", st.SubmittedHeadSHA)
			}
			expected := scenario
			if scenario == "unset" {
				expected = ""
			}
			if scenario == "override" {
				expected = "release"
			}
			if st := f.state(t, issue); st.BaseBranch != expected || st.BaseSHA != base {
				t.Fatalf("wrong authoritative target: %+v", st)
			}
			state := f.state(t, issue)
			if scenario != "override" && state.BaseBranch != "" {
				iss, err := f.tracker.GetIssue(context.Background(), issue)
				if err != nil {
					t.Fatal(err)
				}
				wu, err := contract.ParseWorkUnitFromIssue(issue, iss.Title, iss.Body)
				if err != nil {
					t.Fatal(err)
				}
				if wu.BaseBranch != "" {
					t.Fatal("repository target was not inherited")
				}
			}
			if scenario == "release" || scenario == "override" {
				if base == gitRun(t, f.root, "rev-parse", "refs/heads/main") || base != gitRun(t, f.root, "rev-parse", "refs/heads/release") {
					t.Fatal("release did not pin distinct tip")
				}
				wt := workspace.WorktreePath(f.root, issue)
				if got := gitRun(t, wt, "merge-base", "HEAD", base); got != base {
					t.Fatal("worktree not based on approved release")
				}
				cfg, err := harness.LoadConfigAt(f.root, base)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.BaseBranch != "" {
					t.Fatal("target policy was redirected by default config")
				}
			}
			createdPR, err := f.tracker.GetPullRequest(context.Background(), pr)
			if err != nil {
				t.Fatal(err)
			}
			target := expected
			if target == "" {
				target = "main"
			}
			if createdPR.Base != target {
				t.Fatalf("published target %q want %q", createdPR.Base, target)
			}
			gitRun(t, f.root, "branch", "same-tip", base)
			grant := filepath.Join(t.TempDir(), "grant.json")
			f.must(t, "repo", "validate", "--base", base, "--issue", strconv.Itoa(issue), "--pr", strconv.Itoa(pr), "--grant-only", "--grant-file", grant)
			data, err := os.ReadFile(grant)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			var recorded string
			if raw := fields["base_branch"]; raw != nil {
				if err := json.Unmarshal(raw, &recorded); err != nil {
					t.Fatal(err)
				}
			}
			// Check consumption first so the old reader reproduces the policy mismatch.
			if err := os.RemoveAll(filepath.Join(f.root, ".swarm/coordination")); err != nil {
				t.Fatal(err)
			}
			out := f.must(t, "repo", "validate", "--base", base, "--grant-file", grant)
			if !strings.Contains(out, "NOTE protected changes authorized") || !strings.Contains(out, "PASS git-available") {
				t.Fatalf("grant not accepted: %s", out)
			}
			if recorded != expected {
				t.Fatalf("recorded branch %q want %q", recorded, expected)
			}
			for _, mutation := range []string{"drop", "clear", "swap", "invalid"} {
				t.Run(mutation, func(t *testing.T) {
					var copy map[string]json.RawMessage
					if err := json.Unmarshal(data, &copy); err != nil {
						t.Fatal(err)
					}
					switch mutation {
					case "drop":
						delete(copy, "base_branch")
					case "clear":
						copy["base_branch"] = json.RawMessage(`""`)
					case "swap":
						copy["base_branch"] = json.RawMessage(`"same-tip"`)
					case "invalid":
						copy["base_branch"] = json.RawMessage(`"refs/tags/v1"`)
					}
					if scenario == "unset" && (mutation == "drop" || mutation == "clear") {
						return
					}
					raw, err := json.Marshal(copy)
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(t.TempDir(), "tampered.json")
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
					out, err := f.run("repo", "validate", "--base", base, "--grant-file", path)
					if err == nil || !strings.Contains(out, "NOTE harness grant ignored") {
						t.Fatalf("tamper accepted: %v %s", err, out)
					}
				})
			}
		})
	}
}

func TestIssue102LegacyGrantGolden(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grant.json")
	record := &harnessGrantRecord{Verdict: "granted", Issue: 102, PR: 7, Base: "base", CandidateHead: "head", ApprovedBase: "approved", TaskDigest: "task", PolicyDigest: "policy", PolicyTreeDigest: "tree"}
	if err := writeHarnessGrantFile(path, record); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{
  "verdict": "granted",
  "issue": 102,
  "pr": 7,
  "base": "base",
  "candidate_head": "head",
  "approved_base": "approved",
  "task_digest": "task",
  "policy_digest": "policy",
  "policy_tree_digest": "tree",
  "work_unit": null
}
`
	if string(b) != want {
		t.Fatalf("legacy grant bytes changed: %s", b)
	}
}

type issue102LateRetargetTracker struct {
	tracker.Tracker
	reads int
}

func (tr *issue102LateRetargetTracker) GetPullRequest(ctx context.Context, n int) (*tracker.PullRequest, error) {
	pr, err := tr.Tracker.GetPullRequest(ctx, n)
	tr.reads++
	if err == nil && tr.reads == 2 {
		copy := *pr
		copy.Base = "main"
		return &copy, nil
	}
	return pr, err
}

func TestIssue102ExplicitReleaseReapprovalAndRebase(t *testing.T) {
	for _, lateRetarget := range []bool{false, true} {
		t.Run(strconv.FormatBool(lateRetarget), func(t *testing.T) {
			f, issue, number := submittedPRFixture(t, "none", func(f *cliFixture) {
				path := filepath.Join(f.root, ".swarm/config.yaml")
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append([]byte("base_branch: release\n"), b...), 0600); err != nil {
					t.Fatal(err)
				}
				prGit(t, f.root, "add", ".swarm/config.yaml")
				prGit(t, f.root, "commit", "-m", "release policy")
				prGit(t, f.root, "branch", "release")
			})
			before := f.state(t, issue)
			prGit(t, f.root, "checkout", "release")
			prGit(t, f.root, "commit", "--allow-empty", "-m", "release advanced")
			target := prGit(t, f.root, "rev-parse", "HEAD")
			prGit(t, f.root, "checkout", "main")
			pr, err := f.tracker.GetPullRequest(context.Background(), number)
			if err != nil {
				t.Fatal(err)
			}
			manager := f.manager
			if lateRetarget {
				manager = lease.NewManager(&issue102LateRetargetTracker{Tracker: f.tracker}, contract.SecurityConfig{})
				manager.SetStore(f.root, f.manager.Store())
			}
			st, err := manager.ReapproveRebase(context.Background(), issue, number, before.BaseSHA, target, pr.HeadSHA)
			if lateRetarget {
				if err == nil || !strings.Contains(err.Error(), "approved branch") {
					t.Fatalf("late retarget accepted: %v", err)
				}
				after := f.state(t, issue)
				b, e := json.Marshal(before)
				if e != nil {
					t.Fatal(e)
				}
				a, e := json.Marshal(after)
				if e != nil {
					t.Fatal(e)
				}
				if string(a) != string(b) {
					t.Fatal("late retarget changed authoritative state")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := harness.LoadConfigAt(f.root, target)
			if err != nil {
				t.Fatal(err)
			}
			if st.BaseBranch != "release" || st.BaseSHA != target || st.PolicyDigest != contract.DigestApprovedBranchPolicy(target, cfg, "release") {
				t.Fatalf("wrong reapproval: %+v", st)
			}
			f.must(t, "pr", "rebase", strconv.Itoa(number))
			result := readPROutcome(t, f.must(t, "pr", "status", strconv.Itoa(number), "--json"))
			if result.PR.Base != "release" || result.PR.BaseSHA != target {
				t.Fatalf("wrong status: %+v", result.PR)
			}
		})
	}
}

func TestIssue102DistinctTargetPolicyAndBundle(t *testing.T) {
	f := newCLIFixture(t)
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.root, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	policy := func(marker, command, pattern string) string {
		return "required_checks:\n  - name: " + marker + "\n    command: " + command + "\n    timeout: 1m\nprotected_paths: ['.swarm/**', '" + pattern + "']\n"
	}
	gitRun(t, f.root, "checkout", "-b", "release")
	write(".swarm/validation/required-checks.yaml", policy("release-check-102", "test -f release-proof-102", "release-locked-102/**"))
	for _, role := range []string{"planner", "developer", "tester", "reviewer"} {
		write(".swarm/prompts/"+role+".md", "# Release role\nRELEASE_ROLE_102\n")
	}
	write("release-proof-102", "release\n")
	gitRun(t, f.root, "add", ".")
	gitRun(t, f.root, "commit", "-m", "distinct release policy")
	target := gitRun(t, f.root, "rev-parse", "HEAD")
	gitRun(t, f.root, "checkout", "main")
	configPath := filepath.Join(f.root, ".swarm/config.yaml")
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	write(".swarm/config.yaml", "base_branch: release\n"+string(original))
	write(".swarm/validation/required-checks.yaml", policy("default-check-102", "false", "default-locked-102/**"))
	for _, role := range []string{"planner", "developer", "tester", "reviewer"} {
		write(".swarm/prompts/"+role+".md", "DEFAULT_ROLE_102\n")
	}
	gitRun(t, f.root, "add", ".")
	gitRun(t, f.root, "commit", "-m", "contradictory default policy")
	issue := f.create(t, "none")
	n := strconv.Itoa(issue)
	f.must(t, "issue", "approve", n)
	// Dirty candidate bytes disagree with both committed policies.
	write(".swarm/config.yaml", "base_branch: main\n"+string(original))
	write(".swarm/validation/required-checks.yaml", policy("candidate-check-102", "false", "candidate-locked-102/**"))
	write(".swarm/prompts/developer.md", "CANDIDATE_ROLE_102\n")
	f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
	st := f.state(t, issue)
	if st.BaseSHA != target || st.BaseBranch != "release" {
		t.Fatalf("wrong pin: %+v", st)
	}
	wt := workspace.WorktreePath(f.root, issue)
	if gitRun(t, wt, "merge-base", "HEAD", target) != target {
		t.Fatal("checkout did not use release")
	}
	bundle, err := os.ReadFile(filepath.Join(wt, ".swarm/active/SYSTEM_PROMPT.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"RELEASE_ROLE_102", "release-check-102", "release-locked-102/**"} {
		if !strings.Contains(string(bundle), want) {
			t.Fatalf("bundle missing %s: %s", want, bundle)
		}
	}
	for _, bad := range []string{"DEFAULT_ROLE_102", "CANDIDATE_ROLE_102", "default-check-102", "candidate-check-102", "default-locked-102", "candidate-locked-102"} {
		if strings.Contains(string(bundle), bad) {
			t.Fatalf("bundle trusted %s", bad)
		}
	}
	// Execute the selected check, then demonstrate its protected pattern is enforced.
	gitRun(t, f.root, "restore", ".swarm/config.yaml", ".swarm/prompts/developer.md", ".swarm/validation/required-checks.yaml")
	gitRun(t, f.root, "checkout", "--detach", st.HeadSHA)
	out := f.must(t, "repo", "validate", "--base", target)
	if !strings.Contains(out, "PASS release-check-102") || strings.Contains(out, "default-check-102") || strings.Contains(out, "candidate-check-102") {
		t.Fatalf("wrong checks executed: %s", out)
	}
	if err := os.MkdirAll(filepath.Join(f.root, "release-locked-102"), 0700); err != nil {
		t.Fatal(err)
	}
	write("release-locked-102/change", "protected\n")
	gitRun(t, f.root, "add", "release-locked-102/change")
	gitRun(t, f.root, "commit", "-m", "target protected edit")
	out, err = f.run("repo", "validate", "--base", target)
	if err == nil || !strings.Contains(out+fmt.Sprint(err), "release-locked-102/change") {
		t.Fatalf("target pattern not enforced: %v %s", err, out)
	}
}
