package cli

import (
	"context"
	"encoding/json"
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
			for _, base := range []string{"", "different-102"} {
				if out, err := f.run("issue", "submit", n, "--base", base); err == nil || !strings.Contains(err.Error(), "approved branch") {
					t.Fatalf("mismatch accepted: %v %s", err, out)
				}
			}
			after := f.state(t, number)
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
