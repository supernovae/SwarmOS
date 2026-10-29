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
	"sync"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workspace"
)

// resolverScript resolves every listed conflict and copies each snapshot it
// receives into out/<n> so the test can inspect exactly what the agent saw.
// The output directory is quoted into the script in slash form so Windows
// paths survive the shell.
func resolverScript(out string) string {
	return "SWARM_TEST_RESOLVER_OUT='" + strings.ReplaceAll(filepath.ToSlash(out), "'", `'\''`) + "'\n" + `n=$(ls "$SWARM_TEST_RESOLVER_OUT" | wc -l | tr -d ' '); d="$SWARM_TEST_RESOLVER_OUT/$n"; mkdir -p "$d"; find . -mindepth 1 | sort > "$d/find.txt"; cp -R .swarm/active "$d/active"
for p in $(sed -n 's/^## [0-9]*\. ` + "`" + `\(.*\)` + "`" + `$/\1/p' .swarm/active/CONFLICTS.md); do printf 'resolved %s\n' "$p" > "$p"; done`
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
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'PR update\\n' >> "+first)
	f.must(t, "pr", "work", p, "--agent", "mock", "--progress=false")
	f.must(t, "pr", "submit", p)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "")
	if err := os.WriteFile(filepath.Join(f.root, first), []byte("upstream content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", first)
	prGit(t, f.root, "commit", "-m", "Conflicting upstream change")
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	return f, issue, number, []string{first, first}
}

func TestCLIPRRebaseResolveMultiCommitWithAddAdd(t *testing.T) {
	t.Parallel()
	f, issue, number, paths := twoStopPRFixture(t)
	p := strconv.Itoa(number)
	base := prGit(t, f.root, "rev-parse", "HEAD")
	approved := f.state(t, issue)
	out := t.TempDir()
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", resolverScript(out))

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
	// Micro-plans remain evidence only; validation now binds to the final head.
	after := f.state(t, issue)
	validated := false
	for _, a := range after.Artifacts {
		if a.Type == contract.ArtifactValidation && a.Verdict == "PASS" && a.Metadata["head_sha"] == after.PRRevision.LocalHeadSHA {
			validated = true
		}
	}
	if !validated {
		t.Fatal("resolve did not validate its final checkpoint")
	}
	if len(evidence.Plans) != 2 {
		t.Fatalf("micro-plans = %+v", evidence.Plans)
	}
	for _, plan := range evidence.Plans {
		if plan.Body == "" || plan.PlanDigest == "" || plan.Digest == "" {
			t.Fatalf("incomplete plan evidence: %+v", plan)
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
	t.Parallel()
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
		f.setenv("SWARM_MOCK_DEVELOPER_CMD", cases[name])
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
	t.Parallel()
	f, issue, number, paths := twoStopPRFixture(t)
	p := strconv.Itoa(number)
	f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return timeoutRunner{&agent.MockRunner{}}, nil
	}
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
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'pr a\\n' > a.txt; printf 'pr b\\n' > b.txt")
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
	t.Parallel()
	f, issue, p, worktree := oneStopTwoFiles(t)
	handResolve(t, worktree, "a.txt")
	out := t.TempDir()
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", resolverScript(out))
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
	t.Parallel()
	f, issue, p, worktree := oneStopTwoFiles(t)
	handResolve(t, worktree, "a.txt", "b.txt")
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "exit 9") // fails the run if the resolver is invoked
	f.must(t, "pr", "rebase", p, "--continue", "--resolve", "--agent", "mock", "--progress=false")
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), worktree); rebasing {
		t.Fatal("rebase did not complete")
	}
	if res := f.state(t, issue).LastRebase.Resolutions; len(res) != 0 {
		t.Fatalf("hand resolution recorded as agent work: %+v", res)
	}
}

func TestCLIPRWorkAndStageReportPausedRebase(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	f, issue, number, paths := twoStopPRFixture(t)
	p := strconv.Itoa(number)
	worktree := workspace.PRWorktreePath(f.root, number)
	before := f.state(t, issue)
	headBefore := prGit(t, worktree, "rev-parse", "HEAD")
	f.setenv("SWARM_EXECUTION_MODE", "isolated")
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
	f.setenv("SWARM_EXECUTION_MODE", "trusted-local")
	if _, err := f.run("pr", "rebase", p); err == nil {
		t.Fatal("conflicting rebase reported success")
	}
	stopped := prGit(t, worktree, "rev-parse", "REBASE_HEAD")
	data, _ := os.ReadFile(filepath.Join(worktree, paths[0]))
	f.setenv("SWARM_EXECUTION_MODE", "isolated")
	if _, err := f.run("pr", "rebase", p, "--continue", "--resolve", "--agent", "mock"); err == nil {
		t.Fatal("isolated mock resolver accepted on continue")
	}
	again, _ := os.ReadFile(filepath.Join(worktree, paths[0]))
	if prGit(t, worktree, "rev-parse", "REBASE_HEAD") != stopped || string(again) != string(data) || prGit(t, worktree, "diff", "--name-only", "--diff-filter=U") != paths[0] {
		t.Fatal("failed preflight changed the paused rebase")
	}
}

func TestCLIPRResolvedRebaseBoundedRepairAndNoPublication(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent")
	restore := snapshotCLIFixture(t, f)
	for _, kind := range []string{"pass", "repair", "exhausted", "outside", "zero", "custom"} {
		t.Run(kind, func(t *testing.T) {
			restore(t)
			p := strconv.Itoa(number)
			before := f.state(t, issue)
			original, err := f.tracker.GetPullRequest(context.Background(), number)
			if err != nil {
				t.Fatal(err)
			}
			name := strings.Fields(prGit(t, f.root, "diff", "--name-only", before.BaseSHA, before.HeadSHA))[0]
			// Change trusted policy at the target base, requiring explicit task
			// reapproval and a fresh review of the unchanged plans on the retry.
			command := "test \"$(cat " + name + ")\" = fixed"
			if kind == "pass" {
				command = "git --version"
			}
			policy := "required_checks:\n  - name: repaired-behavior\n    command: '" + command + "'\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
			if err = os.WriteFile(filepath.Join(f.root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0644); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(f.root, name), []byte("upstream\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if kind == "zero" || kind == "custom" {
				limit := "0"
				if kind == "custom" {
					limit = "1"
				}
				configPath := filepath.Join(f.root, ".swarm/config.yaml")
				data, e := os.ReadFile(configPath)
				if e != nil {
					t.Fatal(e)
				}
				data = []byte(strings.Replace(string(data), "max_renewals: 3", "max_renewals: 3\n  rebase_repair_limit: "+limit, 1))
				if e = os.WriteFile(configPath, data, 0644); e != nil {
					t.Fatal(e)
				}
			}
			prGit(t, f.root, "add", ".")
			prGit(t, f.root, "commit", "-m", "New target policy and conflicting source")
			f.must(t, "issue", "approve", strconv.Itoa(issue))
			script := "printf 'resolved\\n' > " + name
			switch kind {
			case "repair":
				script = "if [ -f .swarm/active/REPAIR_PATHS.md ]; then printf fixed > " + name + "; else printf resolved > " + name + "; fi"
			case "exhausted", "zero", "custom":
				script = "if [ -f .swarm/active/REPAIR_PATHS.md ]; then printf x >> " + name + "; else printf resolved > " + name + "; fi"
			case "outside":
				script = "if [ -f .swarm/active/REPAIR_PATHS.md ]; then printf fixed > " + name + "; printf unsafe > outside.txt; else printf resolved > " + name + "; fi"
			}
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", script)
			output, runErr := f.run("pr", "rebase", p, "--resolve", "--agent", "mock", "--json")
			if (runErr == nil) != (kind == "pass" || kind == "repair") {
				t.Fatalf("rebase: %v\n%s", runErr, output)
			}
			st := f.state(t, issue)
			want := map[string]int{"pass": 0, "repair": 1, "exhausted": 2, "outside": 1, "zero": 0, "custom": 1}[kind]
			if st.LastRebase == nil || st.LastRebase.RepairAttempts != want {
				t.Fatalf("repair evidence: %+v", st.LastRebase)
			}
			worktree := workspace.PRWorktreePath(f.root, number)
			if _, err = os.Lstat(filepath.Join(worktree, "outside.txt")); !os.IsNotExist(err) {
				t.Fatalf("rejected addition imported: %v", err)
			}
			if kind == "pass" || kind == "repair" {
				result := readPROutcome(t, output)
				if result.Validation == nil || !result.Validation.Passed || result.SubmitCommand != "swarm pr submit "+p || result.Validation.HeadSHA != result.HeadSHA {
					t.Fatalf("not submit ready: %+v", result)
				}
			} else {
				if !strings.Contains(runErr.Error(), "swarm pr validate "+p) || !strings.Contains(runErr.Error(), "Last local checkpoint") {
					t.Fatalf("missing recovery evidence: %v", runErr)
				}
				checkpoint := st.PRRevision.LocalHeadSHA
				if checkpoint != prGit(t, worktree, "rev-parse", "HEAD") {
					t.Fatal("last checkpoint lost")
				}
				if kind == "exhausted" {
					_, retryErr := f.run("pr", "rebase", p, "--resolve", "--agent", "mock", "--progress=false")
					if retryErr == nil || f.state(t, issue).LastRebase.RepairAttempts != 2 || f.state(t, issue).PRRevision.LocalHeadSHA != checkpoint {
						t.Fatalf("retry reset repair budget: %v", retryErr)
					}
				}
			}
			current, err := f.tracker.GetPullRequest(context.Background(), number)
			if err != nil {
				t.Fatal(err)
			}
			if current.HeadSHA != original.HeadSHA || current.Body != original.Body {
				t.Fatal("rebase published the candidate or PR body")
			}
		})
	}
}

func TestCLIPRRebaseRoutineReapproval(t *testing.T) {
	t.Parallel()
	f, issue, number := submittedPRFixture(t, "agent")
	before := f.state(t, issue)
	original, err := f.tracker.GetPullRequest(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.root, "upstream.txt"), []byte("new target\n"), 0644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", "upstream.txt")
	prGit(t, f.root, "commit", "-m", "Advance identical policy target")
	output := f.must(t, "pr", "rebase", strconv.Itoa(number), "--resolve", "--agent", "mock", "--progress=false")
	after := f.state(t, issue)
	if after.BaseSHA == before.BaseSHA || after.TaskDigest != before.TaskDigest || after.PolicyTreeDigest != before.PolicyTreeDigest || after.ApprovedBy != "alice" {
		t.Fatalf("bad reapproval: %+v", after)
	}
	if !strings.Contains(output, "Validation: PASS") || !strings.Contains(output, "swarm pr submit "+strconv.Itoa(number)) {
		t.Fatalf("missing handoff: %s", output)
	}
	comments, err := f.tracker.ListComments(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	audited := false
	for _, c := range comments {
		if strings.Contains(c.Body, "alice re-pinned") && strings.Contains(c.Body, before.BaseSHA) && strings.Contains(c.Body, after.BaseSHA) {
			audited = true
		}
	}
	if !audited {
		t.Fatal("missing actor/base audit")
	}
	current, err := f.tracker.GetPullRequest(context.Background(), number)
	if err != nil {
		t.Fatal(err)
	}
	if current.Body != original.Body || current.HeadSHA != original.HeadSHA {
		t.Fatal("reapproval published PR")
	}
}

func TestCLIPRRebasePolicyReviewStopsBeforeGit(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"human", "rejected"} {
		t.Run(kind, func(t *testing.T) {
			gate := "agent"
			if kind == "human" {
				gate = "human"
			}
			f, issue, number := submittedPRFixture(t, gate)
			path := filepath.Join(f.root, ".swarm/project.md")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, append(data, []byte("\nNew review guidance\n")...), 0644); err != nil {
				t.Fatal(err)
			}
			prGit(t, f.root, "add", ".")
			prGit(t, f.root, "commit", "-m", "Change policy")
			// Changed policy cannot receive routine task reapproval.
			p := strconv.Itoa(number)
			if _, err = f.run("pr", "rebase", p, "--resolve", "--agent", "mock", "--progress=false"); err == nil || !strings.Contains(err.Error(), "swarm issue approve ") {
				t.Fatalf("policy silently reapproved: %v", err)
			}
			f.must(t, "issue", "approve", strconv.Itoa(issue))
			if kind == "rejected" {
				f.setenv("SWARM_MOCK_VERDICT", "CHANGES_REQUESTED")
			}
			_, err = f.run("pr", "rebase", p, "--resolve", "--agent", "mock", "--progress=false")
			if err == nil {
				t.Fatal("rebase passed a closed plan gate")
			}
			if kind == "human" && !strings.Contains(err.Error(), "swarm issue review-plan "+strconv.Itoa(issue)+" --approve") {
				t.Fatalf("missing human step: %v", err)
			}
			if kind == "rejected" && !strings.Contains(err.Error(), "CHANGES_REQUESTED") {
				t.Fatalf("lost rejection: %v", err)
			}
			st := f.state(t, issue)
			if st.LastRebase != nil || st.PRRevision != nil {
				t.Fatalf("changed rebase before review approval: %+v", st.LastRebase)
			}
		})
	}
}

// scriptedResolver answers developer turns itself: planning turn n writes the
// plan plans[n] names ("valid", "malformed", or "none"), and resolve turns
// overwrite each conflicted file. It records every developer request.
type scriptedResolver struct {
	*agent.MockRunner
	plans []string
	mu    sync.Mutex
	plan  []agent.RunRequest
	turns []agent.RunRequest
}

func (s *scriptedResolver) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	if req.Role != "developer" {
		return s.MockRunner.Run(ctx, req)
	}
	manifest, err := os.ReadFile(filepath.Join(req.WorktreePath, ".swarm", "active", "CONFLICTS.md"))
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(manifest), "\n") {
		if _, rest, ok := strings.Cut(line, ". `"); ok && strings.HasPrefix(line, "## ") {
			paths = append(paths, strings.TrimSuffix(rest, "`"))
		}
	}
	s.mu.Lock()
	planning := strings.HasPrefix(req.TaskPrompt, "Write only .swarm/active/REBASE_PLAN.md.")
	kind := "valid"
	if planning {
		if n := len(s.plan); n < len(s.plans) {
			kind = s.plans[n]
		}
		s.plan = append(s.plan, req)
	} else {
		s.turns = append(s.turns, req)
	}
	s.mu.Unlock()
	if planning {
		var plan strings.Builder
		for _, p := range paths {
			ours := "upstream changes"
			if kind == "malformed" {
				ours = ""
			}
			fmt.Fprintf(&plan, "## %s\n- **Ours:** %s\n- **Theirs:**\n  approved PR changes\nResolution: combine\nApproved behavior: kept\n\n", p, ours)
		}
		if kind != "none" {
			if err := os.WriteFile(filepath.Join(req.WorktreePath, ".swarm", "active", "REBASE_PLAN.md"), []byte(plan.String()), 0o644); err != nil {
				return nil, err
			}
		}
	} else {
		for _, p := range paths {
			if err := os.WriteFile(filepath.Join(req.WorktreePath, p), []byte("resolved "+p+"\n"), 0o644); err != nil {
				return nil, err
			}
		}
	}
	var guidance []string
	if req.OperatorInstructions != "" {
		guidance = []string{req.OperatorInstructions}
	}
	return &agent.RunResult{Agent: "mock", Role: req.Role, Attestation: s.Provenance(req.Role, false), Guidance: guidance}, nil
}

func (s *scriptedResolver) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.plan), len(s.turns)
}

func (f *cliFixture) useResolver(s *scriptedResolver) {
	f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) { return s, nil }
}

// recoveryCommand pulls the printed "swarm pr rebase N --resolve" retry out of
// an error and returns its arguments after "swarm".
func recoveryCommand(t *testing.T, err error, p string) []string {
	t.Helper()
	want := "swarm pr rebase " + p + " --resolve"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error lacks %q: %v", want, err)
	}
	return strings.Fields(want)[1:]
}

func TestCLIPRRebaseResolveRecoveryCommandResumes(t *testing.T) {
	t.Parallel()
	f, issue, number, paths := twoStopPRFixture(t)
	p := strconv.Itoa(number)
	worktree := workspace.PRWorktreePath(f.root, number)
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf '<<<<<<< ours\\nx\\n>>>>>>> theirs\\n' > "+paths[0])
	_, err := f.run("pr", "rebase", p, "--resolve", "--agent", "mock", "--progress=false")
	if err == nil || strings.Contains(err.Error(), "retry with --resolve") {
		t.Fatalf("rejected resolution err = %v", err)
	}
	retry := recoveryCommand(t, err, p)
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), worktree); !rebasing {
		t.Fatal("rejected resolution ended the rebase")
	}
	// Without --resolve a paused rebase is still refused.
	if _, err := f.run("pr", "rebase", p); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("plain rebase on a paused rebase: %v", err)
	}

	out := t.TempDir()
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", resolverScript(out))
	f.must(t, append(retry, "--agent", "mock", "--progress=false")...)
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), worktree); rebasing {
		t.Fatal("recovery command did not finish the rebase")
	}
	evidence := f.state(t, issue).LastRebase
	if evidence == nil || evidence.Status != "completed" || len(evidence.Resolutions) != 2 {
		t.Fatalf("resolutions = %+v", evidence)
	}
}

func TestCLIPRRebasePausedHintResolves(t *testing.T) {
	t.Parallel()
	f, issue, p, worktree := oneStopTwoFiles(t)
	_, err := f.run("pr", "work", p, "--agent", "mock", "--progress=false")
	retry := recoveryCommand(t, err, p)
	out := t.TempDir()
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", resolverScript(out))
	f.must(t, append(retry, "--agent", "mock", "--progress=false")...)
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), worktree); rebasing {
		t.Fatal("paused-rebase hint did not finish the rebase")
	}
	if res := f.state(t, issue).LastRebase.Resolutions; len(res) != 1 || !reflect.DeepEqual(res[0].Paths, []string{"a.txt", "b.txt"}) {
		t.Fatalf("resolution = %+v", res)
	}
}

func TestCLIPRRebaseResolveRetriesMalformedMicroPlanOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		plans  []string
		passed bool
	}{
		{name: "malformed then valid", plans: []string{"malformed", "valid"}, passed: true},
		{name: "missing then valid", plans: []string{"none", "valid"}, passed: true},
		{name: "malformed twice", plans: []string{"malformed", "malformed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, issue, p, worktree := oneStopTwoFiles(t)
			runner := &scriptedResolver{MockRunner: &agent.MockRunner{}, plans: tc.plans}
			f.useResolver(runner)
			_, err := f.run("pr", "rebase", p, "--resolve", "--agent", "mock", "--progress=false")
			planning, resolving := runner.counts()
			if planning != 2 {
				t.Fatalf("planning turns = %d, want 2", planning)
			}
			if tc.plans[0] == "malformed" && !strings.Contains(runner.plan[1].TaskPrompt, "rebase plan lacks Ours: for a.txt") {
				t.Fatalf("retry prompt lacks the rejection: %q", runner.plan[1].TaskPrompt)
			}
			_, _, rebasing, _ := workspace.PRHead(context.Background(), worktree)
			resolutions := f.state(t, issue).LastRebase.Resolutions
			if tc.passed {
				if err != nil || rebasing || resolving != 1 || len(resolutions) != 1 {
					t.Fatalf("err=%v rebasing=%v resolve turns=%d resolutions=%+v", err, rebasing, resolving, resolutions)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "lacks Ours:") || !rebasing || resolving != 0 || len(resolutions) != 0 {
				t.Fatalf("err=%v rebasing=%v resolve turns=%d resolutions=%+v", err, rebasing, resolving, resolutions)
			}
			recoveryCommand(t, err, p)
		})
	}
}

func TestCLIPRRebaseResolveMarkersAreNotRetried(t *testing.T) {
	t.Parallel()
	f, _, p, worktree := oneStopTwoFiles(t)
	// A resolve turn that leaves markers stays manual: no automatic retry.
	runner := &scriptedResolver{MockRunner: &agent.MockRunner{}}
	markers := &markerResolver{scriptedResolver: runner}
	f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) { return markers, nil }
	_, err := f.run("pr", "rebase", p, "--resolve", "--agent", "mock", "--progress=false")
	if err == nil || !strings.Contains(err.Error(), "conflict markers remain") {
		t.Fatalf("err = %v", err)
	}
	if planning, resolving := runner.counts(); planning != 1 || resolving != 1 {
		t.Fatalf("turns = %d planning, %d resolve; want 1 and 1", planning, resolving)
	}
	if _, _, rebasing, _ := workspace.PRHead(context.Background(), worktree); !rebasing {
		t.Fatal("marker rejection ended the rebase")
	}
}

// markerResolver leaves conflict markers in every file its resolve turn edits.
type markerResolver struct{ *scriptedResolver }

func (m *markerResolver) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	res, err := m.scriptedResolver.Run(ctx, req)
	if err != nil || req.Role != "developer" || strings.HasPrefix(req.TaskPrompt, "Write only .swarm/active/REBASE_PLAN.md.") {
		return res, err
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(req.WorktreePath, name), []byte("<<<<<<< ours\nx\n>>>>>>> theirs\n"), 0o644); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func TestCLIPRRebaseResolveDeliversInstructions(t *testing.T) {
	t.Parallel()
	f, issue, p, _ := oneStopTwoFiles(t)
	if _, err := f.run("pr", "rebase", p, "--instructions", "x"); err == nil || !strings.Contains(err.Error(), "--instructions requires --resolve") {
		t.Fatalf("--instructions without --resolve: %v", err)
	}
	runner := &scriptedResolver{MockRunner: &agent.MockRunner{}}
	f.useResolver(runner)
	const guidance = "keep upstream imports"
	f.must(t, "pr", "rebase", p, "--resolve", "--instructions", guidance, "--agent", "mock", "--progress=false")
	requests := append(append([]agent.RunRequest(nil), runner.plan...), runner.turns...)
	if len(requests) != 2 {
		t.Fatalf("developer turns = %d, want 2", len(requests))
	}
	for _, req := range requests {
		if req.OperatorInstructions != guidance || !strings.Contains(req.TaskPrompt, "Operator instructions: "+guidance) {
			t.Fatalf("turn lacks instructions: %+v", req)
		}
	}
	evidence := f.state(t, issue).LastRebase
	want := []string{guidance}
	if len(evidence.Plans) != 1 || !reflect.DeepEqual(evidence.Plans[0].Guidance, want) || len(evidence.Resolutions) != 1 || !reflect.DeepEqual(evidence.Resolutions[0].Guidance, want) {
		t.Fatalf("guidance not recorded: plans=%+v resolutions=%+v", evidence.Plans, evidence.Resolutions)
	}
}

func TestCLIPRRebaseOutsideDiffCompilerRepair(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(strconv.FormatBool(optIn), func(t *testing.T) {
			f, issue, number := submittedPRFixture(t, "agent")
			p := strconv.Itoa(number)
			before := f.state(t, issue)
			original, err := f.tracker.GetPullRequest(t.Context(), number)
			if err != nil {
				t.Fatal(err)
			}
			name := strings.Fields(prGit(t, f.root, "diff", "--name-only", before.BaseSHA, before.HeadSHA))[0]
			files := map[string]string{
				name:                                     "upstream\n",
				"go.mod":                                 "module example.com/repair\n\ngo 1.21\n",
				"caller.go":                              "package repair\nvar Caller int = \"broken\"\n",
				".swarm/validation/required-checks.yaml": "required_checks:\n  - name: build\n    command: go build ./...\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n",
			}
			for path, body := range files {
				if err = os.WriteFile(filepath.Join(f.root, path), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			prGit(t, f.root, "add", ".")
			prGit(t, f.root, "commit", "-m", "New target caller and validation policy")
			f.must(t, "issue", "approve", strconv.Itoa(issue))
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "if [ -f .swarm/active/REPAIR_PATHS.md ]; then printf 'package repair\\nvar Caller int = 1\\n' > caller.go; else printf resolved > "+name+"; fi")
			args := []string{"pr", "rebase", p, "--resolve", "--agent", "mock", "--json"}
			if optIn {
				args = append(args, "--allow-repair-outside-diff")
			}
			output, runErr := f.run(args...)
			st := f.state(t, issue)
			if optIn {
				if runErr != nil {
					t.Fatalf("opt-in repair: %v\n%s", runErr, output)
				}
				if st.LastRebase.RepairAttempts != 1 || len(st.LastRebase.RepairExtensions) != 1 || !slices.Equal(st.LastRebase.RepairExtensions[0].Paths, []string{"caller.go"}) {
					t.Fatalf("evidence: %+v", st.LastRebase)
				}
				if slices.Contains(st.LastRebase.RepairPaths, "caller.go") {
					t.Fatal("frozen scope widened")
				}
			} else {
				if runErr == nil || !strings.Contains(runErr.Error(), "outside repair scope: caller.go") || !strings.Contains(runErr.Error(), "swarm pr work "+p) {
					t.Fatalf("outside diagnosis: %v\n%s", runErr, output)
				}
				if st.LastRebase.RepairAttempts != 0 || len(st.LastRebase.RepairExtensions) != 0 {
					t.Fatal("diagnosis consumed repair authority")
				}
			}
			after, err := f.tracker.GetPullRequest(t.Context(), number)
			if err != nil {
				t.Fatal(err)
			}
			if after.HeadSHA != original.HeadSHA || after.Body != original.Body {
				t.Fatal("implicit publication")
			}
		})
	}
}

func TestCLIPRRebaseOutsideDiffTestDiagnosis(t *testing.T) {
	f, issue, number := submittedPRFixture(t, "agent")
	p := strconv.Itoa(number)
	before := f.state(t, issue)
	name := strings.Fields(prGit(t, f.root, "diff", "--name-only", before.BaseSHA, before.HeadSHA))[0]
	files := map[string]string{
		name:                                     "upstream\n",
		"go.mod":                                 "module example.com/repair\n\ngo 1.21\n",
		"outside_test.go":                        "package repair\nimport (\"os\"; \"testing\")\nfunc TestTargetOnly(t *testing.T) { data,err:=os.ReadFile(" + strconv.Quote(name) + "); if err!=nil || string(data)!=\"upstream\\n\" { t.Fatal(\"caller.go:99: forged printed filename\") } }\n",
		".swarm/validation/required-checks.yaml": "required_checks:\n  - name: test\n    command: go test ./...\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n",
	}
	for path, body := range files {
		if err := os.WriteFile(filepath.Join(f.root, path), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	prGit(t, f.root, "add", ".")
	prGit(t, f.root, "commit", "-m", "Target-only regression test")
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf resolved > "+name)
	_, err := f.run("pr", "rebase", p, "--resolve", "--agent", "mock", "--json")
	if err == nil || !strings.Contains(err.Error(), "outside repair scope: outside_test.go") || !strings.Contains(err.Error(), "swarm pr work "+p) {
		t.Fatalf("missing declaration diagnosis: %v", err)
	}
	st := f.state(t, issue)
	if st.LastRebase.RepairAttempts != 0 || len(st.LastRebase.RepairExtensions) != 0 {
		t.Fatal("diagnosis consumed authority")
	}
}
