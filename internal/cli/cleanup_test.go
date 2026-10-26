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
	f, n, _ := cleanupLocalFixture(t)
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
}
