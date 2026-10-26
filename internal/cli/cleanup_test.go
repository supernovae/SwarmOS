package cli

import (
	"context"
	"encoding/json"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCLIReconcileAndCleanupPreviewReadOnly(t *testing.T) {
	f, n, _ := submittedPRFixture(t, "agent")
	number := strconv.Itoa(n)
	before := f.state(t, n)
	raw := f.must(t, "issue", "cleanup", number, "--json")
	var p struct {
		Resources []struct {
			Kind     string `json:"kind"`
			Eligible bool   `json:"eligible"`
		} `json:"resources"`
	}
	if e := json.Unmarshal([]byte(raw), &p); e != nil {
		t.Fatal(e)
	}
	if len(p.Resources) != 7 {
		t.Fatalf("missing resources: %s", raw)
	}
	for _, r := range p.Resources {
		if r.Eligible {
			t.Fatal("open PR resource eligible")
		}
	}
	if after := f.state(t, n); !reflect.DeepEqual(before, after) {
		t.Fatal("preview changed state")
	}
	f.must(t, "issue", "reconcile", number, "--json")
	after := f.state(t, n)
	if after.PROutcome == nil || after.PROutcome.Status != "open" || after.HeadSHA != before.HeadSHA || !reflect.DeepEqual(after.Artifacts, before.Artifacts) || !reflect.DeepEqual(after.Attestations, before.Attestations) {
		t.Fatal("reconciliation lost submission evidence")
	}
	f.must(t, "issue", "reconcile", number)
	if again := f.state(t, n); !reflect.DeepEqual(after, again) {
		t.Fatal("repeat reconciliation changed state")
	}
}

func TestCLICleanupApplyRefusalHasJSONFailure(t *testing.T) {
	f, n, _ := submittedPRFixture(t, "agent")
	raw, err := f.run("issue", "cleanup", strconv.Itoa(n), "--apply", "--json")
	if err == nil {
		t.Fatal("open PR cleanup succeeded")
	}
	var document struct {
		Kind      string `json:"kind"`
		Status    string `json:"status"`
		Error     string `json:"error"`
		Resources []any  `json:"resources"`
	}
	// Cobra may append human-readable diagnostics; the command's first output
	// document must still contain the full preview and refusal.
	if e := json.NewDecoder(strings.NewReader(raw)).Decode(&document); e != nil {
		t.Fatalf("missing JSON failure: %s %v", raw, e)
	}
	if document.Kind != "cleanup" || document.Status != "failed" || document.Error == "" || len(document.Resources) != 7 {
		t.Fatalf("refusal not actionable: %s", raw)
	}
}

// seedLocalCleanupMergedPR preserves the real local tracker database and only
// supplies the merged observation that its production API cannot yet record.
func seedLocalCleanupMergedPR(t *testing.T, f *cliFixture, number int) {
	t.Helper()
	path := filepath.Join(f.root, ".swarm/local-tracker/state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	var prs map[string]*tracker.PullRequest
	if err = json.Unmarshal(state["pull_requests"], &prs); err != nil {
		t.Fatal(err)
	}
	pr := prs[strconv.Itoa(number)]
	if pr.HeadRepo != "" || pr.BaseRepo != "" {
		t.Fatal("local fixture has repository identities")
	}
	pr.State = "closed"
	pr.Merged = true
	now := time.Now().UTC()
	pr.MergedAt = &now
	pr.HeadSHA = prGit(t, f.root, "rev-parse", "refs/swarm/pr-head/"+strconv.Itoa(number))
	pr.MergeCommitSHA = prGit(t, f.root, "rev-parse", "main")
	state["pull_requests"], _ = json.Marshal(prs)
	raw, _ = json.Marshal(state)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if remotes := prGit(t, f.root, "remote"); remotes != "" {
		t.Fatalf("unexpected remote %s", remotes)
	}
	observed, err := f.tracker.GetPullRequest(context.Background(), number)
	if err != nil || !observed.Merged || observed.HeadRepo != "" || observed.BaseRepo != "" {
		t.Fatalf("merged evidence: %+v %v", observed, err)
	}
}

func cleanupLocalFixture(t *testing.T) (*cliFixture, int, int) {
	t.Helper()
	f, n, pr := submittedPRFixture(t, "agent")
	seedLocalCleanupMergedPR(t, f, pr)
	st := f.state(t, n)
	prGit(t, f.root, "worktree", "add", "-b", workspace.PRBranchName(pr), workspace.PRWorktreePath(f.root, pr), st.HeadSHA)
	return f, n, pr
}

func localCleanupApply(t *testing.T, f *cliFixture, n int) lease.CleanupPreview {
	t.Helper()
	raw := f.must(t, "issue", "cleanup", strconv.Itoa(n), "--apply", "--json")
	var p lease.CleanupPreview
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return p
}

func TestCleanupLocalCleanRetire(t *testing.T) {
	f, n, pr := cleanupLocalFixture(t)
	st := f.state(t, n)
	evidence := prGit(t, f.root, "rev-parse", "refs/swarm/pr-head/"+strconv.Itoa(pr))
	p := localCleanupApply(t, f, n)
	if p.RemoteApplicable || p.Resources[0].Absent || p.Resources[0].Retired || !strings.Contains(p.Resources[0].Reason, "not applicable") {
		t.Fatalf("remote: %+v", p)
	}
	count := 0
	for _, r := range p.Resources {
		if r.Retired {
			count++
		}
	}
	if count != 4 {
		t.Fatalf("expected four retirements: %+v", p.Resources)
	}
	for _, path := range []string{workspace.WorktreePath(f.root, n), workspace.PRWorktreePath(f.root, pr)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("worktree survives: %s %v", path, err)
		}
	}
	if got := prGit(t, f.root, "rev-parse", "refs/swarm/pr-head/"+strconv.Itoa(pr)); got != evidence {
		t.Fatal("PR evidence changed")
	}
	if f.state(t, n).HeadSHA != st.HeadSHA {
		t.Fatal("submission changed")
	}
	localCleanupApply(t, f, n)
}

func TestCleanupLocalDirtyRetention(t *testing.T) {
	for _, kind := range []string{"untracked", "tracked", "staged", "different-head", "newline", "staged-cancelled", "hidden", "managed-staged", "deleted", "renamed"} {
		t.Run(kind, func(t *testing.T) {
			f, n, _ := cleanupLocalFixture(t)
			path := workspace.WorktreePath(f.root, n)
			name := "user file"
			if kind == "newline" {
				name = "user\nfile"
			}
			if kind == "tracked" || kind == "staged" || kind == "staged-cancelled" || kind == "deleted" || kind == "renamed" {
				name = "AGENTS.md"
			}
			if kind == "hidden" {
				name = ".hidden"
			}
			if kind == "managed-staged" {
				name = ".swarm/active/staged"
				if err := os.MkdirAll(filepath.Dir(filepath.Join(path, name)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			original, _ := os.ReadFile(filepath.Join(path, name))

			if kind == "different-head" {
				prGit(t, path, "commit", "--allow-empty", "-m", "changed")
			} else {
				if err := os.WriteFile(filepath.Join(path, name), []byte("preserve me"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "staged" || kind == "staged-cancelled" || kind == "managed-staged" {
					prGit(t, path, "add", "-f", name)
				}
				if kind == "staged-cancelled" {
					if err := os.WriteFile(filepath.Join(path, name), original, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "deleted" {
					if err := os.Remove(filepath.Join(path, name)); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "renamed" {
					if err := os.Rename(filepath.Join(path, name), filepath.Join(path, "renamed")); err != nil {
						t.Fatal(err)
					}
					prGit(t, path, "add", "-A")
				}
			}
			p := localCleanupApply(t, f, n)
			for _, r := range p.Resources {
				if r.Name == path && (r.Retired || r.Reason == "") {
					t.Fatalf("dirty retired %+v", r)
				}
			}
			if kind == "renamed" {
				name = "renamed"
			}
			if kind != "different-head" && kind != "deleted" {
				b, err := os.ReadFile(filepath.Join(path, name))
				expected := []byte("preserve me")
				if kind == "staged-cancelled" {
					expected = original
				}
				if err != nil || string(b) != string(expected) {
					t.Fatal("lost dirty file")
				}
			}
			prGit(t, f.root, "rev-parse", "refs/heads/"+f.state(t, n).Branch)
		})
	}
}

func TestCleanupLocalIgnoredRetention(t *testing.T) {
	for _, name := range []string{"ignored", ".swarm/unexpected", "hidden dir/ignored"} {
		t.Run(name, func(t *testing.T) {
			f, n, _ := cleanupLocalFixture(t)
			path := workspace.WorktreePath(f.root, n)
			exclude := prGit(t, f.root, "rev-parse", "--git-path", "info/exclude")
			if !filepath.IsAbs(exclude) {
				exclude = filepath.Join(f.root, exclude)
			}
			file, err := os.OpenFile(exclude, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = file.WriteString("\n" + name + "\n")
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(path, name)
			if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(target, []byte("ignored artifact"), 0600); err != nil {
				t.Fatal(err)
			}
			localCleanupApply(t, f, n)
			b, err := os.ReadFile(target)
			if err != nil || string(b) != "ignored artifact" {
				t.Fatal("ignored file lost")
			}
		})
	}
}

func TestCleanupLocalPreviewReadOnly(t *testing.T) {
	f, n, _ := cleanupLocalFixture(t)
	before := f.state(t, n)
	refs := prGit(t, f.root, "show-ref")
	trees := prGit(t, f.root, "worktree", "list", "--porcelain")
	for _, args := range [][]string{{"issue", "cleanup", strconv.Itoa(n)}, {"issue", "cleanup", strconv.Itoa(n), "--json"}} {
		f.must(t, args...)
		f.must(t, args...)
	}
	if !reflect.DeepEqual(before, f.state(t, n)) || refs != prGit(t, f.root, "show-ref") || trees != prGit(t, f.root, "worktree", "list", "--porcelain") {
		t.Fatal("preview mutated resources")
	}
}

func TestCleanupLocalActiveClaimRefusal(t *testing.T) {
	for _, kind := range []string{"claim", "revision"} {
		t.Run(kind, func(t *testing.T) {
			f, n, _ := cleanupLocalFixture(t)
			st := f.state(t, n)
			if kind == "claim" {
				st.Lease.Status = "active"
			} else {
				st.PRRevision = &lease.PRRevision{Number: st.PRNumber}
			}
			raw, _ := json.Marshal(st)
			store := coordination.NewFileStore(f.root)
			_, rev, err := store.Load(context.Background(), n)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.CompareAndSwap(context.Background(), n, rev, raw); err != nil {
				t.Fatal(err)
			}
			before := f.state(t, n)
			refs := prGit(t, f.root, "show-ref")
			_, err = f.run("issue", "cleanup", strconv.Itoa(n), "--apply")
			if err == nil {
				t.Fatal("active cleanup succeeded")
			}
			if !reflect.DeepEqual(before, f.state(t, n)) || refs != prGit(t, f.root, "show-ref") {
				t.Fatal("refusal mutated state")
			}
		})
	}
}

func TestCleanupLocalOpenPRRefusal(t *testing.T) {
	for _, kind := range []string{"linked", "issue", "pr", "normalized", "unrelated"} {
		t.Run(kind, func(t *testing.T) {
			f, n, pr := cleanupLocalFixture(t)
			st := f.state(t, n)
			path := filepath.Join(f.root, ".swarm/local-tracker/state.json")
			raw, _ := os.ReadFile(path)
			var state map[string]json.RawMessage
			_ = json.Unmarshal(raw, &state)
			var prs map[string]*tracker.PullRequest
			_ = json.Unmarshal(state["pull_requests"], &prs)
			if kind == "linked" {
				prs[strconv.Itoa(pr)].State = "open"
				prs[strconv.Itoa(pr)].Merged = false
				prs[strconv.Itoa(pr)].MergedAt = nil
			} else {
				head := st.Branch
				if kind == "pr" {
					head = workspace.PRBranchName(pr)
				}
				if kind == "normalized" {
					head = "local:refs/heads/" + workspace.PRBranchName(pr)
				}
				if kind == "unrelated" {
					head = "unrelated"
				}
				prs["99"] = &tracker.PullRequest{Number: 99, State: "open", Head: head}
			}
			state["pull_requests"], _ = json.Marshal(prs)
			raw, _ = json.Marshal(state)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before := f.state(t, n)
			refs := prGit(t, f.root, "show-ref")
			_, err := f.run("issue", "cleanup", strconv.Itoa(n), "--apply")
			if kind == "unrelated" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("open PR allowed")
			}
			if !reflect.DeepEqual(before, f.state(t, n)) || refs != prGit(t, f.root, "show-ref") {
				t.Fatal("refusal mutated state")
			}
		})
	}
}

func TestCleanupLocalRetireIdempotentEvidence(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(strconv.FormatBool(absent), func(t *testing.T) {
			f, n, pr := cleanupLocalFixture(t)
			if absent {
				prGit(t, f.root, "update-ref", "-d", "refs/swarm/pr-head/"+strconv.Itoa(pr))
			}
			localCleanupApply(t, f, n)
			localCleanupApply(t, f, n)
		})
	}
}

func TestCleanupLocalOutcomeHeadChangedRefusal(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(strconv.FormatBool(absent), func(t *testing.T) {
			f, n, pr := cleanupLocalFixture(t)
			st := f.state(t, n)
			head := prGit(t, f.root, "rev-parse", "main")
			if absent {
				prGit(t, f.root, "update-ref", "-d", "refs/swarm/pr-head/"+strconv.Itoa(pr))
				prGit(t, f.root, "update-ref", "refs/heads/"+st.Branch, head)
			} else {
				prGit(t, f.root, "update-ref", "refs/swarm/pr-head/"+strconv.Itoa(pr), head)
			}
			refs := prGit(t, f.root, "show-ref")
			_, err := f.run("issue", "cleanup", strconv.Itoa(n), "--apply")
			if err == nil {
				t.Fatal("changed head accepted")
			}
			if refs != prGit(t, f.root, "show-ref") {
				t.Fatal("refusal changed refs")
			}
		})
	}
}

// Intercept the actual Git command, after production inspection, without adding
// a test hook to the production API. The wrapper runs a one-shot ref movement.
func TestCleanupLocalMovedTipCAS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git wrapper")
	}
	for _, stage := range []string{"before-cas", "before-removal"} {
		t.Run(stage, func(t *testing.T) {
			f, n, _ := cleanupLocalFixture(t)
			st := f.state(t, n)
			path := workspace.WorktreePath(f.root, n)
			tree := prGit(t, f.root, "rev-parse", st.HeadSHA+"^{tree}")
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			c := exec.Command(realGit, "-C", f.root, "commit-tree", tree, "-p", st.HeadSHA, "-m", "moved")
			out, err := c.Output()
			if err != nil {
				t.Fatal(err)
			}
			moved := strings.TrimSpace(string(out))
			bin := t.TempDir()
			marker := filepath.Join(bin, "seen")
			trigger := ""
			if stage == "before-cas" {
				trigger = "[ \"$1\" = update-ref ] && [ \"$2\" = -d ] && [ \"$3\" = " + shellCleanupQuote("refs/heads/"+st.Branch) + " ]"
			} else {
				trigger = "[ \"$PWD\" = " + shellCleanupQuote(path) + " ] && [ \"$1\" = rev-parse ] && [ \"$2\" = HEAD ]"
			}
			script := "#!/bin/sh\nif " + trigger + "; then\n"
			if stage == "before-removal" {
				script += " if [ -f " + shellCleanupQuote(marker) + " ]; then\n"
			} else {
				script += " if [ ! -f " + shellCleanupQuote(marker) + " ]; then\n"
			}
			script += "  " + shellCleanupQuote(realGit) + " -C " + shellCleanupQuote(f.root) + " update-ref " + shellCleanupQuote("refs/heads/"+st.Branch) + " " + shellCleanupQuote(moved) + "\n fi\n : > " + shellCleanupQuote(marker) + "\nfi\nexec " + shellCleanupQuote(realGit) + " \"$@\"\n"
			if err = os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			p := localCleanupApply(t, f, n)
			if got := prGit(t, f.root, "rev-parse", "refs/heads/"+st.Branch); got != moved {
				t.Fatalf("moved tip lost %s", got)
			}
			var branch lease.CleanupResource
			for _, r := range p.Resources {
				if r.Kind == "local-code-branch" && r.Name == st.Branch {
					branch = r
				}
			}
			if branch.Retired || branch.ExpectedHead != st.HeadSHA || !strings.Contains(branch.Reason, "retained") {
				t.Fatalf("CAS result: %+v", branch)
			}
			_, err = os.Stat(path)
			if stage == "before-removal" && err != nil {
				t.Fatal("associated worktree removed")
			}
			if stage == "before-cas" && !os.IsNotExist(err) {
				t.Fatal("CAS barrier ran before worktree removal")
			}
		})
	}
}

func shellCleanupQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func TestCleanupLocalRemovalFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git wrapper")
	}
	f, n, _ := cleanupLocalFixture(t)
	st := f.state(t, n)
	path := workspace.WorktreePath(f.root, n)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = worktree ] && [ \"$2\" = remove ] && [ \"$3\" = " + shellCleanupQuote(path) + " ]; then exit 1; fi\nexec " + shellCleanupQuote(realGit) + " \"$@\"\n"
	if err = os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := localCleanupApply(t, f, n)
	for _, r := range p.Resources {
		if (r.Name == path || r.Name == st.Branch) && r.Kind != "remote-code-branch" && (r.Retired || !strings.Contains(r.Reason, "retained")) {
			t.Fatalf("inaccurate partial result: %+v", r)
		}
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("failed removal lost worktree")
	}
	prGit(t, f.root, "rev-parse", "refs/heads/"+st.Branch)
	if f.state(t, n).Cleanup != nil {
		t.Fatal("synchronous failure left reservation")
	}
}

func TestCleanupLocalPolicyTransitions(t *testing.T) {
	for _, kind := range []string{"claim", "revision", "issue-branch", "pr-branch"} {
		t.Run(kind, func(t *testing.T) {
			f, n, pr := cleanupLocalFixture(t)
			ctx := context.Background()
			preview, err := lease.InspectCleanup(ctx, f.root, f.tracker, f.manager, n)
			if err != nil || preview.Blocked != "" {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			st := f.state(t, n)
			if kind == "claim" || kind == "revision" {
				if kind == "claim" {
					st.Lease.Status = "active"
				} else {
					st.PRRevision = &lease.PRRevision{Number: pr}
				}
				store := coordination.NewFileStore(f.root)
				_, rev, err := store.Load(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(st)
				if _, err = store.CompareAndSwap(ctx, n, rev, raw); err != nil {
					t.Fatal(err)
				}
			} else {
				path := filepath.Join(f.root, ".swarm/local-tracker/state.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var state map[string]json.RawMessage
				var prs map[string]*tracker.PullRequest
				if err = json.Unmarshal(raw, &state); err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(state["pull_requests"], &prs); err != nil {
					t.Fatal(err)
				}
				head := st.Branch
				if kind == "pr-branch" {
					head = workspace.PRBranchName(pr)
				}
				prs["99"] = &tracker.PullRequest{Number: 99, State: "open", Head: "local:refs/heads/" + head}
				state["pull_requests"], _ = json.Marshal(prs)
				raw, _ = json.Marshal(state)
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := f.state(t, n)
			refs := prGit(t, f.root, "show-ref")
			trees := prGit(t, f.root, "worktree", "list", "--porcelain")
			if _, err = f.manager.BeginCleanup(ctx, n, preview.Outcome); err == nil {
				t.Fatal("stale preview allowed reservation")
			}
			if !reflect.DeepEqual(before, f.state(t, n)) || refs != prGit(t, f.root, "show-ref") || trees != prGit(t, f.root, "worktree", "list", "--porcelain") {
				t.Fatal("policy transition refusal mutated resources")
			}
		})
	}
}

func TestCleanupLocalRelativeRootPreview(t *testing.T) {
	f, n, pr := cleanupLocalFixture(t)
	before := f.state(t, n)
	refs := prGit(t, f.root, "show-ref")
	trees := prGit(t, f.root, "worktree", "list", "--porcelain")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(f.root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Error(err)
		}
	})
	for _, args := range [][]string{{"--root", "."}, nil} {
		cmd := NewRootCmd()
		var output strings.Builder
		cmd.SetOut(&output)
		cmd.SetArgs(append(args, "--backend", "local", "issue", "cleanup", strconv.Itoa(n), "--json"))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var preview lease.CleanupPreview
		if err := json.Unmarshal([]byte(output.String()), &preview); err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, r := range preview.Resources {
			if r.Kind == "worktree" || r.Kind == "local-code-branch" {
				count++
				if !r.Eligible || r.Absent || r.Retired {
					t.Errorf("relative root %v: resource not eligible: %+v", args, r)
				}
			}
		}
		if count != 4 {
			t.Fatalf("missing local resources: %+v", preview)
		}
	}
	if !reflect.DeepEqual(before, f.state(t, n)) || refs != prGit(t, f.root, "show-ref") || trees != prGit(t, f.root, "worktree", "list", "--porcelain") {
		t.Fatal("preview mutated resources")
	}

	cmd := NewRootCmd()
	var output strings.Builder
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--root", ".", "--backend", "local", "issue", "cleanup", strconv.Itoa(n), "--apply", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result lease.CleanupPreview
	if err := json.Unmarshal([]byte(output.String()), &result); err != nil {
		t.Fatal(err)
	}
	for _, r := range result.Resources {
		if (r.Kind == "worktree" || r.Kind == "local-code-branch") && !r.Retired {
			t.Fatalf("resource not retired: %+v", r)
		}
	}
	assertCleanupLocalsRetired(t, f, n, pr, before.Branch)
}

func assertCleanupLocalsRetired(t *testing.T, f *cliFixture, n, pr int, branch string) {
	t.Helper()
	for _, path := range []string{workspace.WorktreePath(f.root, n), workspace.PRWorktreePath(f.root, pr)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("worktree remains %s: %v", path, err)
		}
	}
	for _, branch := range []string{branch, workspace.PRBranchName(pr)} {
		if err := exec.Command("git", "-C", f.root, "show-ref", "--verify", "refs/heads/"+branch).Run(); err == nil {
			t.Fatalf("branch remains: %s", branch)
		}
	}
	if f.state(t, n).Cleanup != nil {
		t.Fatal("cleanup reservation remains")
	}
}

func TestCleanupLocalRelativeRootRecoverFailedApply(t *testing.T) {
	f, n, _ := cleanupLocalFixture(t)
	ctx := context.Background()
	preview, err := lease.InspectCleanup(ctx, f.root, f.tracker, f.manager, n)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := f.manager.BeginCleanup(ctx, n, preview.Outcome)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an interrupted apply after its resource identity check fails.
	for i := range preview.Resources {
		if preview.Resources[i].Kind == "worktree" {
			preview.Resources[i].Name += "-unexpected"
			break
		}
	}
	if err := f.manager.RetireCleanupLocalResources(ctx, n, reservation.OperationID, preview); err == nil {
		t.Fatal("expected failed apply")
	}
	if f.state(t, n).Cleanup == nil {
		t.Fatal("missing interrupted reservation")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(f.root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Error(err)
		}
	})
	cmd := NewRootCmd()
	var output strings.Builder
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--root", ".", "--backend", "local", "issue", "cleanup", strconv.Itoa(n), "--recover-reservation", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	st := f.state(t, n)
	if st.Cleanup != nil {
		t.Fatal("reservation remains after recovery")
	}
	if len(st.CleanupHistory) == 0 || st.CleanupHistory[len(st.CleanupHistory)-1].OperationID != reservation.OperationID {
		t.Fatal("recovered reservation not recorded")
	}
	if err := f.manager.RetireCleanupLocalResources(ctx, n, reservation.OperationID, preview); err == nil {
		t.Fatal("recovered token still usable")
	}
}

func revisedCleanupFixture(t *testing.T) (*cliFixture, int, int, string) {
	t.Helper()
	f, n, pr := submittedPRFixture(t, "agent")
	old := f.state(t, n).HeadSHA
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR update\\n' > pr-update.txt")
	f.must(t, "pr", "work", strconv.Itoa(pr), "--agent", "mock", "--instructions", "Revise implementation", "--progress=false")
	f.must(t, "pr", "validate", strconv.Itoa(pr))
	f.must(t, "pr", "submit", strconv.Itoa(pr))
	seedLocalCleanupMergedPR(t, f, pr)
	return f, n, pr, old
}

func TestCleanupLocalRevisedPRRetire(t *testing.T) {
	f, n, _, old := revisedCleanupFixture(t)
	before := f.state(t, n)
	if before.HeadSHA == old {
		t.Fatal("PR did not advance")
	}
	recorded := false
	for _, h := range before.CheckpointHeads {
		if h == old {
			recorded = true
		}
	}
	if !recorded {
		t.Fatal("FileStore lost predecessor checkpoint")
	}
	if ok, err := workspace.BaseIsAncestor(context.Background(), f.root, old, before.HeadSHA); err != nil || !ok {
		t.Fatalf("not an ancestor: %v", err)
	}
	raw := f.must(t, "issue", "cleanup", strconv.Itoa(n), "--json")
	var p lease.CleanupPreview
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Resources {
		if r.Name == workspace.WorktreePath(f.root, n) || (r.Kind == "local-code-branch" && r.Name == before.Branch) {
			if !r.Eligible || r.ExpectedHead != old {
				t.Fatalf("ancestor not eligible: %+v", r)
			}
		}
	}
	if !reflect.DeepEqual(before, f.state(t, n)) {
		t.Fatal("preview changed state")
	}
	p = localCleanupApply(t, f, n)
	for _, r := range p.Resources {
		if r.Name == workspace.WorktreePath(f.root, n) && !r.Retired {
			t.Fatalf("ancestor retained: %+v", r)
		}
	}
	localCleanupApply(t, f, n)
}

func TestCleanupLocalDirtyLineageAncestor(t *testing.T) {
	for _, kind := range []string{"tracked", "staged", "untracked", "ignored"} {
		t.Run(kind, func(t *testing.T) {
			f, n, _, _ := revisedCleanupFixture(t)
			path := workspace.WorktreePath(f.root, n)
			name := "user-file"
			if kind == "tracked" || kind == "staged" {
				name = "AGENTS.md"
			}
			if kind == "ignored" {
				name = ".swarm/user-file"
				fpath := filepath.Join(t.TempDir(), "ignore")
				prGit(t, path, "config", "core.excludesFile", fpath)
				if err := os.WriteFile(fpath, []byte(".swarm/\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(path, name), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "staged" {
				prGit(t, path, "add", name)
			}
			p := localCleanupApply(t, f, n)
			for _, r := range p.Resources {
				if (r.Name == path || r.Name == f.state(t, n).Branch) && r.Retired {
					t.Fatal("dirty ancestor retired")
				}
			}
			if b, err := os.ReadFile(filepath.Join(path, name)); err != nil || string(b) != "keep" {
				t.Fatal("lost user file")
			}
		})
	}
}

func TestCleanupLocalNonLineageAncestor(t *testing.T) {
	f, n, pr := submittedPRFixture(t, "agent")
	path := workspace.WorktreePath(f.root, n)
	prGit(t, path, "commit", "--allow-empty", "-m", "unrecorded")
	unknown := prGit(t, path, "rev-parse", "HEAD")
	prGit(t, path, "commit", "--allow-empty", "-m", "final")
	final := prGit(t, path, "rev-parse", "HEAD")
	st := f.state(t, n)
	st.CheckpointHeads = []string{st.HeadSHA, final}
	st.HeadSHA, st.SubmittedHeadSHA = final, final
	store := coordination.NewFileStore(f.root)
	_, rev, err := store.Load(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	if _, err = store.CompareAndSwap(context.Background(), n, rev, raw); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "update-ref", "refs/swarm/pr-head/"+strconv.Itoa(pr), final)
	seedLocalCleanupMergedPR(t, f, pr)
	prGit(t, path, "reset", "--hard", unknown)
	p, err := lease.InspectCleanup(context.Background(), f.root, f.tracker, f.manager, n)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Resources {
		if r.Name == path && (r.Eligible || !strings.Contains(r.Reason, "lineage")) {
			t.Fatalf("unrecorded ancestor: %+v", r)
		}
	}
	// Forging preview eligibility must not bypass fresh apply authorization.
	reservation, err := f.manager.BeginCleanup(context.Background(), n, p.Outcome)
	if err != nil {
		t.Fatal(err)
	}
	for i := range p.Resources {
		r := &p.Resources[i]
		if r.Name == path {
			r.Eligible = true
			r.ExpectedHead = unknown
		}
	}
	if err = f.manager.RetireCleanupLocalResources(context.Background(), n, reservation.OperationID, p); err != nil {
		t.Fatal(err)
	}
	if got := prGit(t, path, "rev-parse", "HEAD"); got != unknown {
		t.Fatal("unrecorded ancestor changed")
	}
}

func TestCleanupLocalLineageMovedTip(t *testing.T) {
	f, n, _, old := revisedCleanupFixture(t)
	ctx := context.Background()
	p, err := lease.InspectCleanup(ctx, f.root, f.tracker, f.manager, n)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := f.manager.BeginCleanup(ctx, n, p.Outcome)
	if err != nil {
		t.Fatal(err)
	}
	path := workspace.WorktreePath(f.root, n)
	prGit(t, path, "commit", "--allow-empty", "-m", "moved after preview")
	moved := prGit(t, path, "rev-parse", "HEAD")
	if err = f.manager.RetireCleanupLocalResources(ctx, n, reservation.OperationID, p); err != nil {
		t.Fatal(err)
	}
	if got := prGit(t, path, "rev-parse", "HEAD"); got != moved {
		t.Fatal("adopted or removed new tip")
	}
	for _, r := range p.Resources {
		if r.Name == path && (r.Retired || r.ExpectedHead != old) {
			t.Fatalf("lost captured tip: %+v", r)
		}
	}
}

// rebasedCleanupFixture publishes count real PR rebases onto advanced bases
// and returns the original issue checkpoint.
func rebasedCleanupFixture(t *testing.T, count int) (*cliFixture, int, int, string) {
	t.Helper()
	f, n, pr := submittedPRFixture(t, "agent")
	p := strconv.Itoa(pr)
	old := f.state(t, n).HeadSHA
	for i := 0; i < count; i++ {
		name := "upstream-" + strconv.Itoa(i) + ".txt"
		if err := os.WriteFile(filepath.Join(f.root, name), []byte("new upstream base\n"), 0644); err != nil {
			t.Fatal(err)
		}
		prGit(t, f.root, "add", name)
		prGit(t, f.root, "commit", "-m", "Advance approved base")
		f.must(t, "issue", "approve", strconv.Itoa(n))
		f.must(t, "pr", "rebase", p)
		f.must(t, "pr", "validate", p)
		f.must(t, "pr", "submit", p)
	}
	seedLocalCleanupMergedPR(t, f, pr)
	st := f.state(t, n)
	if ok, err := workspace.BaseIsAncestor(context.Background(), f.root, old, st.SubmittedHeadSHA); err != nil || ok {
		t.Fatalf("rebase kept the original checkpoint as an ancestor: %v", err)
	}
	if got := prGit(t, workspace.WorktreePath(f.root, n), "rev-parse", "HEAD"); got != old {
		t.Fatalf("issue worktree moved: %s", got)
	}
	return f, n, pr, old
}

func assertCleanupIssueResources(t *testing.T, f *cliFixture, n int, p lease.CleanupPreview, eligible bool, head string) {
	t.Helper()
	path, branch := workspace.WorktreePath(f.root, n), f.state(t, n).Branch
	seen := 0
	for _, r := range p.Resources {
		if r.Name == path || (r.Kind == "local-code-branch" && r.Name == branch) {
			seen++
			if r.Eligible != eligible || (eligible && r.ExpectedHead != head) || (!eligible && (r.Retired || r.Reason == "")) {
				t.Fatalf("issue resource: %+v", r)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("missing issue resources: %+v", p.Resources)
	}
}

func TestCleanupLocalVerifiedRebaseRetire(t *testing.T) {
	f, n, pr, old := rebasedCleanupFixture(t, 1)
	before := f.state(t, n)
	if r := before.LastRebase; r == nil || r.OldHead != old || r.NewHead != before.SubmittedHeadSHA || !r.Verified || r.PatchIdentity != "unchanged" {
		t.Fatalf("rebase evidence: %+v", r)
	}
	if len(before.RebaseTransitions) != 1 {
		t.Fatalf("transition not persisted: %+v", before.RebaseTransitions)
	}
	p, err := lease.InspectCleanup(context.Background(), f.root, f.tracker, f.manager, n)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanupIssueResources(t, f, n, *p, true, old)
	if !reflect.DeepEqual(before, f.state(t, n)) {
		t.Fatal("preview changed state")
	}
	localCleanupApply(t, f, n)
	assertCleanupLocalsRetired(t, f, n, pr, before.Branch)
	if f.state(t, n).SubmittedHeadSHA != before.SubmittedHeadSHA {
		t.Fatal("submission changed")
	}
	localCleanupApply(t, f, n)
}

func TestCleanupLocalChainedVerifiedRebasesRetire(t *testing.T) {
	f, n, pr, old := rebasedCleanupFixture(t, 2)
	before := f.state(t, n)
	if before.LastRebase == nil || before.LastRebase.OldHead == old {
		t.Fatalf("second rebase did not replace LastRebase: %+v", before.LastRebase)
	}
	if len(before.RebaseTransitions) != 2 || before.RebaseTransitions[0].OldHead != old || before.RebaseTransitions[0].NewHead != before.RebaseTransitions[1].OldHead || before.RebaseTransitions[1].NewHead != before.SubmittedHeadSHA {
		t.Fatalf("chain not persisted: %+v", before.RebaseTransitions)
	}
	p, err := lease.InspectCleanup(context.Background(), f.root, f.tracker, f.manager, n)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanupIssueResources(t, f, n, *p, true, old)
	localCleanupApply(t, f, n)
	assertCleanupLocalsRetired(t, f, n, pr, before.Branch)
}

func TestCleanupRebaseIdentityRetention(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(r *coordination.CheckpointRebase, old, final string)
	}{
		{"unverified", func(r *coordination.CheckpointRebase, _, _ string) { r.Verified = false }},
		{"changed", func(r *coordination.CheckpointRebase, _, _ string) { r.PatchIdentity = "changed" }},
		{"unavailable", func(r *coordination.CheckpointRebase, _, _ string) { r.PatchIdentity = "unavailable" }},
		{"empty identity", func(r *coordination.CheckpointRebase, _, _ string) { r.PatchIdentity = "" }},
		{"in-progress", func(r *coordination.CheckpointRebase, _, _ string) { r.Status = "in-progress" }},
		{"aborted", func(r *coordination.CheckpointRebase, _, _ string) { r.Status = "aborted" }},
		{"missing endpoint", func(r *coordination.CheckpointRebase, _, _ string) { r.NewHead = "" }},
		{"reversed", func(r *coordination.CheckpointRebase, old, final string) { r.OldHead, r.NewHead = final, old }},
		{"disconnected", func(r *coordination.CheckpointRebase, _, _ string) { r.OldHead = strings.Repeat("c", 40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, n, _, old := rebasedCleanupFixture(t, 1)
			store := coordination.NewFileStore(f.root)
			st := f.state(t, n)
			tc.mutate(&st.RebaseTransitions[0], old, st.SubmittedHeadSHA)
			st.LastRebase = &lease.RebaseEvidence{OldHead: st.RebaseTransitions[0].OldHead, NewHead: st.RebaseTransitions[0].NewHead, OldBase: st.RebaseTransitions[0].OldBase, NewBase: st.RebaseTransitions[0].NewBase, Status: st.RebaseTransitions[0].Status, PatchIdentity: st.RebaseTransitions[0].PatchIdentity, Verified: st.RebaseTransitions[0].Verified}
			_, rev, err := store.Load(context.Background(), n)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(st)
			if _, err = store.CompareAndSwap(context.Background(), n, rev, raw); err != nil {
				t.Fatal(err)
			}
			p, err := lease.InspectCleanup(context.Background(), f.root, f.tracker, f.manager, n)
			if err != nil {
				t.Fatal(err)
			}
			assertCleanupIssueResources(t, f, n, *p, false, "")
			p2 := localCleanupApply(t, f, n)
			assertCleanupIssueResources(t, f, n, p2, false, "")
			path := workspace.WorktreePath(f.root, n)
			if got := prGit(t, path, "rev-parse", "HEAD"); got != old {
				t.Fatal("retained worktree changed")
			}
			prGit(t, f.root, "show-ref", "--verify", "refs/heads/"+st.Branch)
		})
	}
}

func TestCleanupLocalDirtyVerifiedRebase(t *testing.T) {
	for _, kind := range []string{"tracked", "staged", "untracked", "ignored", "after-preview"} {
		t.Run(kind, func(t *testing.T) {
			f, n, _, old := rebasedCleanupFixture(t, 1)
			path := workspace.WorktreePath(f.root, n)
			branch := f.state(t, n).Branch
			name := "user-file"
			if kind == "tracked" || kind == "staged" {
				name = "AGENTS.md"
			}
			if kind == "ignored" {
				name = ".swarm/user-file"
				fpath := filepath.Join(t.TempDir(), "ignore")
				prGit(t, path, "config", "core.excludesFile", fpath)
				if err := os.WriteFile(fpath, []byte(".swarm/\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			var preview *lease.CleanupPreview
			var operation string
			if kind == "after-preview" {
				p, err := lease.InspectCleanup(ctx, f.root, f.tracker, f.manager, n)
				if err != nil {
					t.Fatal(err)
				}
				assertCleanupIssueResources(t, f, n, *p, true, old)
				reservation, err := f.manager.BeginCleanup(ctx, n, p.Outcome)
				if err != nil {
					t.Fatal(err)
				}
				preview, operation = p, reservation.OperationID
			}
			if err := os.WriteFile(filepath.Join(path, name), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "staged" {
				prGit(t, path, "add", name)
			}
			var p lease.CleanupPreview
			if preview != nil {
				if err := f.manager.RetireCleanupLocalResources(ctx, n, operation, preview); err != nil {
					t.Fatal(err)
				}
				p = *preview
			} else {
				p = localCleanupApply(t, f, n)
			}
			for _, r := range p.Resources {
				if (r.Name == path || r.Name == branch) && r.Retired {
					t.Fatalf("dirty rebased checkpoint retired: %+v", r)
				}
			}
			if b, err := os.ReadFile(filepath.Join(path, name)); err != nil || string(b) != "keep" {
				t.Fatal("lost user file")
			}
			if got := prGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
				t.Fatalf("issue branch not checked out: %s", got)
			}
		})
	}
}

func TestCleanupLocalVerifiedRebaseEvidenceChangedBeforeApply(t *testing.T) {
	f, n, _, old := rebasedCleanupFixture(t, 1)
	ctx := context.Background()
	p, err := lease.InspectCleanup(ctx, f.root, f.tracker, f.manager, n)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanupIssueResources(t, f, n, *p, true, old)
	reservation, err := f.manager.BeginCleanup(ctx, n, p.Outcome)
	if err != nil {
		t.Fatal(err)
	}
	// Evidence withdrawn after preview must not authorize deletion.
	store := coordination.NewFileStore(f.root)
	raw, rev, err := store.Load(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	var st lease.IssueState
	if err = json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	st.RebaseTransitions[0].PatchIdentity = "changed"
	st.LastRebase.PatchIdentity = "changed"
	raw, _ = json.Marshal(&st)
	if _, err = store.CompareAndSwap(ctx, n, rev, raw); err != nil {
		t.Fatal(err)
	}
	if err = f.manager.RetireCleanupLocalResources(ctx, n, reservation.OperationID, p); err != nil {
		t.Fatal(err)
	}
	path := workspace.WorktreePath(f.root, n)
	if got := prGit(t, path, "rev-parse", "HEAD"); got != old {
		t.Fatal("stale preview retired the issue worktree")
	}
	prGit(t, f.root, "show-ref", "--verify", "refs/heads/"+st.Branch)
}
