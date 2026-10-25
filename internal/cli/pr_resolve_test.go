package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workspace"
)

// resolverScript resolves every listed conflict and copies each snapshot it
// receives into out/<n> so the test can inspect exactly what the agent saw.
func resolverScript(out string) string {
	return fmt.Sprintf(`n=$(ls %[1]s | wc -l); d=%[1]s/$n; mkdir -p "$d"; find . -mindepth 1 | sort > "$d/find.txt"; cp -R .swarm/active "$d/active"
for p in $(sed -n 's/^## [0-9]*\. `+"`"+`\(.*\)`+"`"+`$/\1/p' .swarm/active/CONFLICTS.md); do printf 'resolved %%s\n' "$p" > "$p"; done`, out)
}

// twoStopPRFixture returns a submitted PR with two commits that both conflict
// with a new approved base: first an add/add conflict on the file the PR
// created, then a three-way content conflict when its second commit is replayed.
func twoStopPRFixture(t *testing.T) (*cliFixture, int, int, []string) {
	t.Helper()
	f, issue, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	before := f.state(t, issue)
	first := strings.Fields(prGit(t, f.root, "diff", "--name-only", before.BaseSHA, before.HeadSHA))[0]
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR update\\n' >> "+first)
	f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false")
	f.must(t, "pr", "submit", p)
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "")
	if err := os.WriteFile(filepath.Join(f.root, first), []byte("upstream content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", first)
	prGit(t, f.root, "commit", "-m", "Conflicting upstream change")
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	return f, issue, number, []string{first, first}
}

func TestCLIPRRebaseResolveMultiCommitWithAddAdd(t *testing.T) {
	f, issue, number, paths := twoStopPRFixture(t)
	p := strconv.Itoa(number)
	base := prGit(t, f.root, "rev-parse", "HEAD")
	approved := f.state(t, issue)
	out := t.TempDir()
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", resolverScript(out))

	rebased := readPROutcome(t, f.must(t, "pr", "rebase", p, "--resolve", "--agent", "mock", "--json"))
	worktree := workspace.PRWorktreePath(f.root, number)
	if rebased.Status != "pending" {
		t.Fatalf("resolve did not finish the rebase: %+v", rebased)
	}
	prGit(t, worktree, "merge-base", "--is-ancestor", base, "HEAD")
	if data, err := os.ReadFile(filepath.Join(worktree, paths[0])); err != nil || string(data) != "resolved "+paths[0]+"\n" {
		t.Fatalf("%s = %q, %v", paths[0], data, err)
	}

	// One turn per stop, each seeing only its conflicted file and coordinator context.
	turns, _ := os.ReadDir(out)
	if len(turns) != 2 {
		t.Fatalf("resolver turns = %d, want 2", len(turns))
	}
	for i, want := range paths {
		listing, err := os.ReadFile(filepath.Join(out, strconv.Itoa(i), "find.txt"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range strings.Fields(string(listing)) {
			entry = strings.TrimPrefix(entry, "./")
			if entry == ".git" || strings.HasPrefix(entry, ".git/") {
				t.Fatalf("resolver snapshot exposed Git metadata: %s", listing)
			}
			if entry != want && !strings.HasPrefix(want, entry+"/") && entry != ".swarm" && !strings.HasPrefix(entry, ".swarm/active") {
				t.Fatalf("turn %d saw unexpected entry %q:\n%s", i, entry, listing)
			}
		}
		sides := filepath.Join(out, strconv.Itoa(i), "active", "conflicts", "1")
		ours, _ := os.ReadFile(filepath.Join(sides, "ours"))
		theirs, _ := os.ReadFile(filepath.Join(sides, "theirs"))
		wantOurs := "upstream content\n"
		if i == 1 {
			wantOurs = "resolved " + want + "\n" // the first turn's accepted resolution
		}
		if string(ours) != wantOurs || len(theirs) == 0 || strings.HasPrefix(string(theirs), "upstream") {
			t.Fatalf("turn %d sides ours=%q theirs=%q", i, ours, theirs)
		}
		// Turn 0 is add/add (no common ancestor); turn 1 is a three-way conflict.
		_, baseErr := os.Stat(filepath.Join(sides, "base"))
		if addAdd := i == 0; addAdd != os.IsNotExist(baseErr) {
			t.Fatalf("turn %d base presence wrong: %v", i, baseErr)
		}
		for _, name := range []string{"WORKUNIT.md", "PLAN.md", "SYSTEM_PROMPT.md", "CONFLICTS.md"} {
			if _, err := os.Stat(filepath.Join(out, strconv.Itoa(i), "active", name)); err != nil {
				t.Fatalf("turn %d missing %s", i, name)
			}
		}
	}

	evidence := f.state(t, issue).LastRebase
	if evidence == nil || evidence.Status != "completed" || len(evidence.Resolutions) != 2 {
		t.Fatalf("resolutions not recorded: %+v", evidence)
	}
	for i, res := range evidence.Resolutions {
		if res.Agent != "mock" || res.Reported || !reflect.DeepEqual(res.Paths, []string{paths[i]}) || len(res.Commit) != 40 {
			t.Fatalf("resolution %d = %+v", i, res)
		}
	}
	// Resolution is code evidence only: no validation, and plan approval untouched.
	after := f.state(t, issue)
	for _, a := range after.Artifacts {
		if a.Type == contract.ArtifactValidation {
			t.Fatal("resolve recorded validation evidence")
		}
	}
	review := func(st []contract.Artifact) []contract.Artifact {
		var out []contract.Artifact
		for _, a := range st {
			if a.Type == contract.ArtifactReview {
				out = append(out, a)
			}
		}
		return out
	}
	if !reflect.DeepEqual(review(approved.Artifacts), review(after.Artifacts)) {
		t.Fatal("resolve changed plan review evidence")
	}
	if status := f.must(t, "pr", "status", p); !strings.Contains(status, "Resolved by mock") {
		t.Fatalf("status lacks resolution provenance:\n%s", status)
	}
	f.must(t, "pr", "submit", p)
	updated, err := f.tracker.GetPullRequest(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated.Body, "Resolved by mock") {
		t.Fatal("PR update lacks resolution provenance")
	}
}

func TestCLIPRRebaseResolveRejectsUnsafeResults(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	st := f.state(t, issue)
	conflicting := strings.Fields(prGit(t, f.root, "diff", "--name-only", st.BaseSHA, st.HeadSHA))[0]
	if err := os.WriteFile(filepath.Join(f.root, conflicting), []byte("upstream content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", conflicting)
	prGit(t, f.root, "commit", "-m", "Conflicting upstream change")
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	worktree := workspace.PRWorktreePath(f.root, number)

	cases := map[string]string{
		"new file":      "printf x > extra.txt",
		"new directory": "mkdir newdir",
		"symlink":       "ln -s " + conflicting + " link",
		"markers":       "printf '<<<<<<< ours\\nx\\n>>>>>>> theirs\\n' > " + conflicting,
		"deleted":       "rm " + conflicting,
		"agent failure": "exit 3",
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Setenv("SWARM_MOCK_DEVELOPER_CMD", cases[name])
		_, err := f.run("pr", "rebase", p, "--resolve", "--agent", "mock", "--progress=false")
		if err == nil || !strings.Contains(err.Error(), conflicting) || !strings.Contains(err.Error(), "--abort") {
			t.Fatalf("%s: err = %v", name, err)
		}
		if _, _, rebasing, e := workspace.PRHead(context.Background(), worktree); e != nil || !rebasing {
			t.Fatalf("%s: rebase no longer paused", name)
		}
		data, readErr := os.ReadFile(filepath.Join(worktree, conflicting))
		if readErr != nil || !workspace.HasConflictMarkers(data) {
			t.Fatalf("%s: conflict file changed: %q", name, data)
		}
		if unmerged := prGit(t, worktree, "diff", "--name-only", "--diff-filter=U"); unmerged != conflicting {
			t.Fatalf("%s: conflict index changed: %q", name, unmerged)
		}
		for _, stray := range []string{"extra.txt", "newdir", "link"} {
			if _, err := os.Lstat(filepath.Join(worktree, stray)); err == nil {
				t.Fatalf("%s: %s imported into the worktree", name, stray)
			}
		}
		if res := f.state(t, issue).LastRebase.Resolutions; len(res) != 0 {
			t.Fatalf("%s: rejected turn recorded a resolution: %+v", name, res)
		}
		f.must(t, "pr", "rebase", p, "--abort")
		if head := prGit(t, worktree, "rev-parse", "HEAD"); head != st.HeadSHA {
			t.Fatalf("%s: abort left head at %s", name, head)
		}
	}
}

// timeoutRunner simulates a harness timeout whose diagnostic names its workspace.
type timeoutRunner struct{ *agent.MockRunner }

func (timeoutRunner) Run(_ context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	_ = os.MkdirAll(filepath.Join(req.WorktreePath, ".swarm", "active-output"), 0o755)
	return nil, fmt.Errorf("developer agent wall timeout after configured limit 1s; recovery patch saved in %s/.swarm/active-output: %w", req.WorktreePath, context.DeadlineExceeded)
}

func TestCLIPRRebaseResolveTimeoutLeavesRebasePaused(t *testing.T) {
	f, issue, number, paths := twoStopPRFixture(t)
	p := strconv.Itoa(number)
	testResolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return timeoutRunner{&agent.MockRunner{}}, nil
	}
	t.Cleanup(func() { testResolverRunner = nil })
	_, err := f.run("pr", "rebase", p, "--resolve", "--agent", "mock", "--progress=false")
	if err == nil || !strings.Contains(err.Error(), "timeout") || !strings.Contains(err.Error(), paths[0]) {
		t.Fatalf("timeout err = %v", err)
	}
	if strings.Contains(err.Error(), "swarm-resolve-") || strings.Contains(err.Error(), os.TempDir()) {
		t.Fatalf("error leaks the resolver workspace path: %v", err)
	}
	worktree := workspace.PRWorktreePath(f.root, number)
	if _, statErr := os.Stat(filepath.Join(worktree, ".swarm", "active-output")); statErr == nil {
		t.Fatal("timeout recovery output landed in the PR worktree")
	}
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), worktree); !rebasing {
		t.Fatal("timeout ended the rebase")
	}
	if res := f.state(t, issue).LastRebase.Resolutions; len(res) != 0 {
		t.Fatalf("timeout recorded a resolution: %+v", res)
	}
}

// oneStopTwoFiles pauses a rebase at one PR commit with add/add conflicts on
// a.txt and b.txt.
func oneStopTwoFiles(t *testing.T) (*cliFixture, int, string, string) {
	t.Helper()
	f, issue, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'pr a\\n' > a.txt; printf 'pr b\\n' > b.txt")
	f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false")
	f.must(t, "pr", "submit", p)
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte("upstream "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		prGit(t, f.root, "add", name)
	}
	prGit(t, f.root, "commit", "-m", "Upstream adds the same files")
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	if _, err := f.run("pr", "rebase", p); err == nil {
		t.Fatal("conflicting rebase reported success")
	}
	worktree := workspace.PRWorktreePath(f.root, number)
	if got := prGit(t, worktree, "diff", "--name-only", "--diff-filter=U"); got != "a.txt\nb.txt" {
		t.Fatalf("expected two add/add conflicts in one stop, got %q", got)
	}
	return f, issue, p, worktree
}

func handResolve(t *testing.T, worktree string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("hand resolved "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		prGit(t, worktree, "add", name)
	}
}

func TestCLIPRRebaseContinueResolveKeepsHandStagedFiles(t *testing.T) {
	f, issue, p, worktree := oneStopTwoFiles(t)
	handResolve(t, worktree, "a.txt")
	out := t.TempDir()
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", resolverScript(out))
	f.must(t, "pr", "rebase", p, "--continue", "--resolve", "--agent", "mock", "--progress=false")
	listing, err := os.ReadFile(filepath.Join(out, "0", "find.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(listing), "./a.txt") || !strings.Contains(string(listing), "./b.txt") {
		t.Fatalf("resolver saw the wrong files:\n%s", listing)
	}
	if a, _ := os.ReadFile(filepath.Join(worktree, "a.txt")); string(a) != "hand resolved a.txt\n" {
		t.Fatalf("hand-staged file changed: %q", a)
	}
	if b, _ := os.ReadFile(filepath.Join(worktree, "b.txt")); string(b) != "resolved b.txt\n" {
		t.Fatalf("resolver output not imported: %q", b)
	}
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), worktree); rebasing {
		t.Fatal("rebase did not complete")
	}
	res := f.state(t, issue).LastRebase.Resolutions
	if len(res) != 1 || !reflect.DeepEqual(res[0].Paths, []string{"b.txt"}) {
		t.Fatalf("resolution = %+v", res)
	}
}

func TestCLIPRRebaseContinueResolveWithEverythingStagedSkipsTheAgent(t *testing.T) {
	f, issue, p, worktree := oneStopTwoFiles(t)
	handResolve(t, worktree, "a.txt", "b.txt")
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "exit 9") // fails the run if the resolver is invoked
	f.must(t, "pr", "rebase", p, "--continue", "--resolve", "--agent", "mock", "--progress=false")
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), worktree); rebasing {
		t.Fatal("rebase did not complete")
	}
	if res := f.state(t, issue).LastRebase.Resolutions; len(res) != 0 {
		t.Fatalf("hand resolution recorded as agent work: %+v", res)
	}
}

func TestCLIPRWorkAndStageReportPausedRebase(t *testing.T) {
	f, issue, number, paths := twoStopPRFixture(t)
	p := strconv.Itoa(number)
	if _, err := f.run("pr", "rebase", p); err == nil {
		t.Fatal("conflicting rebase reported success")
	}
	if !strings.Contains(f.state(t, issue).LastRebase.Status, "in-progress") {
		t.Fatal("paused rebase not recorded")
	}
	for _, args := range [][]string{{"pr", "work", p, "--agent", "mock", "--progress=false"}, {"pr", "stage", p, "--phase", "implement"}} {
		_, err := f.run(args...)
		if err == nil || !strings.Contains(err.Error(), paths[0]) || !strings.Contains(err.Error(), "--resolve") || !strings.Contains(err.Error(), "--abort") || !strings.Contains(err.Error(), "--continue") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

func TestCLIPRRebaseResolveFailsClosedWithoutIsolation(t *testing.T) {
	f, issue, number, paths := twoStopPRFixture(t)
	p := strconv.Itoa(number)
	worktree := workspace.PRWorktreePath(f.root, number)
	before := f.state(t, issue)
	headBefore := prGit(t, worktree, "rev-parse", "HEAD")
	t.Setenv("SWARM_EXECUTION_MODE", "isolated")
	_, err := f.run("pr", "rebase", p, "--resolve", "--agent", "mock")
	if err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("isolated mock resolver err = %v", err)
	}
	if after := f.state(t, issue); !reflect.DeepEqual(before, after) {
		t.Fatal("failed preflight changed authoritative state")
	}
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), worktree); rebasing || prGit(t, worktree, "rev-parse", "HEAD") != headBefore {
		t.Fatal("failed preflight touched the PR worktree")
	}
	if _, err := f.run("pr", "rebase", p, "--resolve", "--developer-agent", "no-such-agent"); err == nil {
		t.Fatal("unknown developer agent accepted")
	}

	// A paused rebase stays byte-for-byte intact when --continue --resolve cannot preflight.
	t.Setenv("SWARM_EXECUTION_MODE", "trusted-local")
	if _, err := f.run("pr", "rebase", p); err == nil {
		t.Fatal("conflicting rebase reported success")
	}
	stopped := prGit(t, worktree, "rev-parse", "REBASE_HEAD")
	data, _ := os.ReadFile(filepath.Join(worktree, paths[0]))
	t.Setenv("SWARM_EXECUTION_MODE", "isolated")
	if _, err := f.run("pr", "rebase", p, "--continue", "--resolve", "--agent", "mock"); err == nil {
		t.Fatal("isolated mock resolver accepted on continue")
	}
	again, _ := os.ReadFile(filepath.Join(worktree, paths[0]))
	if prGit(t, worktree, "rev-parse", "REBASE_HEAD") != stopped || string(again) != string(data) || prGit(t, worktree, "diff", "--name-only", "--diff-filter=U") != paths[0] {
		t.Fatal("failed preflight changed the paused rebase")
	}
}
