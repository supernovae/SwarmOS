package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"github.com/supernovae/swarmos/internal/tracker"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func freshnessGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := gitfixture.Command(t, "", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func freshnessWrite(t *testing.T, root, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestSubmitFreshnessOutcomes(t *testing.T) {
	for _, mode := range []string{"unchanged", "clean", "conflicts", "skipped", "zero-count"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			g := func(args ...string) string { return freshnessGit(t, root, args...) }
			g("init", "-b", "main")
			g("config", "user.name", "Fixture")
			g("config", "user.email", "fixture@example.invalid")
			freshnessWrite(t, root, "file", "base\n")
			g("add", ".")
			g("commit", "-m", "base")
			base := g("rev-parse", "HEAD")
			g("checkout", "-b", "candidate")
			freshnessWrite(t, root, "file", "candidate\n")
			g("commit", "-am", "candidate")
			head := g("rev-parse", "HEAD")
			g("checkout", "main")
			switch mode {
			case "clean":
				for i := 0; i < 2; i++ {
					freshnessWrite(t, root, "other", strconv.Itoa(i))
					g("add", ".")
					g("commit", "-m", "advance")
				}
			case "conflicts":
				freshnessWrite(t, root, "file", "target\n")
				g("commit", "-am", "advance")
			case "skipped":
				g("remote", "add", "origin", filepath.Join(root, "missing"))
			case "zero-count":
				g("commit", "--allow-empty", "-m", "later")
				base = g("rev-parse", "HEAD")
				g("reset", "--hard", "HEAD~1")
			}
			before := g("rev-parse", "HEAD")
			index := g("write-tree")
			got := checkSubmissionFreshness(context.Background(), root, "main", base, head)
			want := mode
			if mode == "zero-count" {
				want = "clean"
			}
			if got.Status != want {
				t.Fatalf("got %+v, want %s", got, want)
			}
			if mode == "clean" && *got.BehindCount != 2 {
				t.Fatalf("count %+v", got)
			}
			if mode == "conflicts" && !reflect.DeepEqual(got.ConflictedPaths, []string{"file"}) {
				t.Fatal(got)
			}
			if mode == "skipped" && (got.BaseDrift != nil || got.BehindCount != nil) {
				t.Fatal(got)
			}
			if before != g("rev-parse", "HEAD") || index != g("write-tree") || g("status", "--porcelain") != "" {
				t.Fatal("inspection mutated candidate")
			}
		})
	}
}
func TestSubmitFreshnessParser(t *testing.T) {
	tree := strings.Repeat("a", 40)
	for _, tc := range []struct {
		data string
		code int
		want []string
		bad  bool
	}{
		{tree + "\x00", 0, []string{}, false},
		{tree + "\x00z\x00a\n\t ü\x00z\x00", 1, []string{"a\n\t ü", "z"}, false},
		{"bad\x00", 0, nil, true}, {tree, 0, nil, true}, {tree + "\x00", 1, nil, true}, {tree + "\x00file\x00", 0, nil, true},
	} {
		got, err := parseSubmissionConflicts([]byte(tc.data), tc.code)
		if (err != nil) != tc.bad || (!tc.bad && !reflect.DeepEqual(got, tc.want)) {
			t.Fatalf("%q: %v %v", tc.data, got, err)
		}
	}
}
func TestSubmitFreshnessGuidance(t *testing.T) {
	f := submissionFreshness{Status: "conflicts", ConflictedPaths: []string{"a\n`<b>"}}
	for _, tc := range []struct {
		opts    RootOptions
		command string
	}{
		{RootOptions{}, "swarm pr rebase 12 --resolve"},
		{RootOptions{Fork: true}, "swarm --fork pr rebase 12 --resolve"},
		{RootOptions{Fork: true, ForkRemote: "personal"}, "swarm --fork --fork-remote personal pr rebase 12 --resolve"},
	} {
		note := f.conflictNote(12, 100, &tc.opts)
		if !strings.Contains(note, tc.command) || !strings.Contains(note, "swarm issue approve 100") || strings.Contains(note, "<b>") {
			t.Fatal(note)
		}
	}
}
func TestSubmitFreshnessJSON(t *testing.T) {
	t.Parallel()
	for _, drift := range []bool{false, true} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("drift=%t/json=%t", drift, asJSON), func(t *testing.T) {
				f := newCLIFixture(t)
				number := f.create(t, "agent")
				n := strconv.Itoa(number)
				f.must(t, "issue", "approve", n)
				f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
				old := f.state(t, number)
				if drift {
					freshnessWrite(t, f.root, "target-only", "advance")
					freshnessGit(t, f.root, "add", "target-only")
					freshnessGit(t, f.root, "commit", "-m", "advance")
				}
				args := []string{"issue", "submit", n}
				if asJSON {
					args = append(args, "--json")
				}
				out := f.must(t, args...)
				state := f.state(t, number)
				if state.BaseSHA != old.BaseSHA || state.HeadSHA != old.HeadSHA {
					t.Fatal("submission changed approved checkpoint")
				}
				if asJSON {
					var result map[string]json.RawMessage
					if err := json.Unmarshal([]byte(out), &result); err != nil {
						t.Fatal(err, out)
					}
					var got submissionFreshness
					if err := json.Unmarshal(result["base_freshness"], &got); err != nil {
						t.Fatal(err)
					}
					want := "unchanged"
					if drift {
						want = "clean"
					}
					if got.Status != want || got.ConflictedPaths == nil {
						t.Fatal(got)
					}
				} else if !drift {
					want := fmt.Sprintf("Submitted draft PR #%d for issue #%d on %s (%s)\n", state.PRNumber, number, state.Branch, state.PRURL)
					if out != want {
						t.Fatalf("%q != %q", out, want)
					}
				} else if !strings.Contains(out, "behind the target by 1 commits") {
					t.Fatal(out)
				}
				retry := f.must(t, args...)
				if strings.Contains(retry, "base_freshness") || strings.Contains(retry, "behind the target") {
					t.Fatal(retry)
				}
			})
		}
	}
}

func TestSubmitFreshnessFetchFailure(t *testing.T) {
	t.Parallel()
	for _, explicit := range []bool{false, true} {
		t.Run(strconv.FormatBool(explicit), func(t *testing.T) {
			f := newCLIFixture(t)
			n := f.create(t, "agent")
			number := strconv.Itoa(n)
			f.must(t, "issue", "approve", number)
			f.must(t, "issue", "work", number, "--agent", "mock", "--headless")
			base := f.state(t, n).BaseSHA
			freshnessGit(t, f.root, "remote", "add", "origin", filepath.Join(f.root, "missing-secret-remote"))
			freshnessGit(t, f.root, "update-ref", "refs/remotes/origin/main", base)
			freshnessGit(t, f.root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
			args := []string{"issue", "submit", number, "--json"}
			if explicit {
				args = append(args, "--base", "main")
			}
			out := f.must(t, args...)
			var got struct {
				Freshness submissionFreshness `json:"base_freshness"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err, out)
			}
			if got.Freshness.Status != "skipped" || got.Freshness.BaseDrift != nil || got.Freshness.BehindCount != nil {
				t.Fatal(out)
			}
			if strings.Contains(out, f.root) || strings.Contains(out, "missing-secret-remote") {
				t.Fatal("diagnostic leak", out)
			}
			if f.state(t, n).PRNumber == 0 {
				t.Fatal("did not publish")
			}
		})
	}
}

func TestSubmitFreshnessConflicts(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			f := newCLIFixture(t)
			// Both branches independently add the same path, producing add/add conflicts.
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'candidate\\n' > conflict.txt")
			number := f.create(t, "agent")
			n := strconv.Itoa(number)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
			state := f.state(t, number)
			if existing {
				if _, err := f.tracker.CreateDraftPR(t.Context(), tracker.DraftPRRequest{IssueNumber: number, Title: "Existing", Body: "Existing", Head: state.Branch, Base: "main"}); err != nil {
					t.Fatal(err)
				}
			}
			freshnessWrite(t, f.root, "conflict.txt", "target\n")
			freshnessGit(t, f.root, "add", "conflict.txt")
			freshnessGit(t, f.root, "commit", "-m", "target")
			out := f.must(t, "issue", "submit", n)
			submitted := f.state(t, number)
			command := fmt.Sprintf("swarm pr rebase %d --resolve", submitted.PRNumber)
			pr, err := f.tracker.FindDraftPR(t.Context(), submitted.Branch, "main")
			if err != nil || pr == nil {
				t.Fatal(err)
			}
			for _, body := range []string{out, pr.Body} {
				if !strings.Contains(body, command) || !strings.Contains(body, "conflict.txt") || !strings.Contains(body, "approval re-pinned") {
					t.Fatal(body)
				}
			}
			if submitted.BaseSHA != state.BaseSHA || submitted.HeadSHA != state.HeadSHA {
				t.Fatal("changed approved state")
			}
		})
	}
}

func TestSubmitFreshnessOriginSelection(t *testing.T) {
	root := t.TempDir()
	g := func(args ...string) string { return freshnessGit(t, root, args...) }
	g("init", "-b", "main")
	g("config", "user.name", "Fixture")
	g("config", "user.email", "fixture@example.invalid")
	freshnessWrite(t, root, "file", "base\n")
	g("add", ".")
	g("commit", "-m", "base")
	base := g("rev-parse", "HEAD")
	origin := filepath.Join(t.TempDir(), "origin.git")
	fork := filepath.Join(t.TempDir(), "fork.git")
	g("clone", "--bare", root, origin)
	g("clone", "--bare", root, fork)
	g("remote", "add", "origin", origin)
	g("remote", "add", "personal", fork)
	g("checkout", "-b", "candidate")
	freshnessWrite(t, root, "file", "candidate\n")
	g("commit", "-am", "candidate")
	head := g("rev-parse", "HEAD")
	g("checkout", "main")
	freshnessWrite(t, root, "other", "upstream\n")
	g("add", ".")
	g("commit", "-m", "upstream")
	g("push", "origin", "main:release")
	got := checkSubmissionFreshness(t.Context(), root, "release", base, head)
	if got.Status != "clean" || *got.BehindCount != 1 {
		t.Fatal(got)
	}
	// Advancing the writable fork cannot affect freshness against unchanged origin/main.
	g("push", "personal", "main:main")
	got = checkSubmissionFreshness(t.Context(), root, "main", base, head)
	if got.Status != "unchanged" {
		t.Fatal(got)
	}
	got = checkSubmissionFreshness(t.Context(), root, "nonexistent", base, head)
	if got.Status != "skipped" {
		t.Fatal(got)
	}
}

type freshnessUpdateFailure struct {
	tracker.Tracker
	updates int
}

func (t *freshnessUpdateFailure) UpdateDraftPR(context.Context, int, string, string) (*tracker.PullRequest, error) {
	t.updates++
	return nil, fmt.Errorf("sensitive diagnostic must not escape")
}
func TestSubmitFreshnessFailedDescriptionUpdate(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			f := newCLIFixture(t)
			f.setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'candidate\\n' > conflict.txt")
			issue := f.create(t, "agent")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
			state := f.state(t, issue)
			if existing {
				if _, err := f.tracker.CreateDraftPR(t.Context(), tracker.DraftPRRequest{IssueNumber: issue, Title: "Existing", Head: state.Branch, Base: "main"}); err != nil {
					t.Fatal(err)
				}
			}
			freshnessWrite(t, f.root, "conflict.txt", "target\n")
			freshnessGit(t, f.root, "add", "conflict.txt")
			freshnessGit(t, f.root, "commit", "-m", "target")
			tr := &freshnessUpdateFailure{Tracker: f.tracker}
			run := func() (string, string) {
				cmd := f.command(&RootOptions{RepoRoot: f.root, Backend: "local", trackerOverride: tr})
				var out, diagnostics bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&diagnostics)
				cmd.SetArgs([]string{"issue", "submit", n, "--json"})
				if err := cmd.ExecuteContext(t.Context()); err != nil {
					t.Fatal(err, out.String(), diagnostics.String())
				}
				return out.String(), diagnostics.String()
			}
			out, diagnostics := run()
			var parsed map[string]json.RawMessage
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				t.Fatal(err, out)
			}
			submitted := f.state(t, issue)
			if submitted.PRNumber == 0 || submitted.Lease.Status != "completed" || tr.updates != 1 {
				t.Fatal("submission failed", submitted, tr.updates)
			}
			command := fmt.Sprintf("swarm pr rebase %d --resolve", submitted.PRNumber)
			if !strings.Contains(diagnostics, "PR description lacks the freshness note; run `"+command+"`") || strings.Contains(diagnostics, "sensitive diagnostic") {
				t.Fatal(diagnostics)
			}
			retry, _ := run()
			if strings.Contains(retry, "base_freshness") || tr.updates != 1 {
				t.Fatal("retry repaired description", retry, tr.updates)
			}
		})
	}
}

func TestSubmitFreshnessInspectionFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("disposable git executable fixture")
	}
	root := t.TempDir()
	g := func(args ...string) string { return freshnessGit(t, root, args...) }
	g("init", "-b", "main")
	g("config", "user.name", "Fixture")
	g("config", "user.email", "fixture@example.invalid")
	freshnessWrite(t, root, "file", "base\n")
	g("add", ".")
	g("commit", "-m", "base")
	base := g("rev-parse", "HEAD")
	freshnessWrite(t, root, "file", "tip\n")
	g("commit", "-am", "tip")
	head := g("rev-parse", "HEAD")
	if got := checkSubmissionFreshness(t.Context(), root, "main", strings.Repeat("0", 40), head); got.Status != "skipped" || got.BehindCount != nil {
		t.Fatal(got)
	}
	// Resolve Git for the shim that exercises production merge-tree failure handling.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = merge-tree ]; then printf 'unsupported private diagnostic' >&2; exit 2; fi\nexec " + submissionShellWord(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	got := checkSubmissionFreshness(t.Context(), root, "main", base, head)
	if got.Status != "skipped" || got.BaseDrift == nil || !*got.BaseDrift || got.BehindCount == nil || *got.BehindCount != 1 {
		t.Fatal(got)
	}
	if strings.Contains(got.advisory(), "private diagnostic") {
		t.Fatal("diagnostic leak")
	}
}
