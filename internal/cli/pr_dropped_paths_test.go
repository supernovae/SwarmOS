package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/workspace"
)

// twoFilePRFixture submits a PR that adds a.txt and b.txt.
func twoFilePRFixture(t *testing.T) (*cliFixture, int, int) {
	t.Helper()
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "printf 'pr a\\n' > a.txt; printf 'pr b\\n' > b.txt")
	f, issue, number := submittedPRFixture(t, "agent")
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "")
	st := f.state(t, issue)
	if got := strings.Fields(prGit(t, f.root, "diff", "--name-only", st.BaseSHA, st.HeadSHA)); !reflect.DeepEqual(got, []string{"a.txt", "b.txt"}) {
		t.Fatalf("fixture PR changed %q", got)
	}
	return f, issue, number
}

func TestCLIPRRebaseReportsDroppedPathsFromConflictResolution(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolved string
		dropped  []string
	}{
		{"resolved to base drops path", "upstream a\n", []string{"a.txt"}},
		{"every path kept", "resolved a\n", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, issue, number := twoFilePRFixture(t)
			p := strconv.Itoa(number)
			original, err := f.tracker.GetPullRequest(context.Background(), number)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(f.root, "a.txt"), []byte("upstream a\n"), 0644); err != nil {
				t.Fatal(err)
			}
			prGit(t, f.root, "add", "a.txt")
			prGit(t, f.root, "commit", "-m", "Conflicting upstream a.txt")
			f.must(t, "issue", "approve", strconv.Itoa(issue))
			if _, err = f.run("pr", "rebase", p); err == nil {
				t.Fatal("conflicting rebase reported success")
			}
			if out := f.must(t, "pr", "status", p); strings.Contains(out, "no longer changed") {
				t.Fatalf("paused rebase reported dropped paths:\n%s", out)
			}
			worktree := workspace.PRWorktreePath(f.root, number)
			if err = os.WriteFile(filepath.Join(worktree, "a.txt"), []byte(tc.resolved), 0644); err != nil {
				t.Fatal(err)
			}
			// A path newly changed during the rebase is never reported as dropped.
			if err = os.WriteFile(filepath.Join(worktree, "new.txt"), []byte("added during rebase\n"), 0644); err != nil {
				t.Fatal(err)
			}
			prGit(t, worktree, "add", "a.txt", "new.txt")
			rebased := readPROutcome(t, f.must(t, "pr", "rebase", p, "--continue", "--json"))
			evidence := rebased.State.LastRebase
			if evidence == nil || evidence.Status != "completed" || !evidence.DroppedPathsVerified || !reflect.DeepEqual(append([]string{}, evidence.DroppedPaths...), tc.dropped) {
				t.Fatalf("rebase JSON evidence: %+v", evidence)
			}
			stored := f.state(t, issue).LastRebase
			if !reflect.DeepEqual(stored, evidence) || stored.NewHead != rebased.HeadSHA {
				t.Fatalf("stored evidence %+v differs from JSON %+v", stored, evidence)
			}
			status := readPROutcome(t, f.must(t, "pr", "status", p, "--json"))
			if !reflect.DeepEqual(status.State.LastRebase, stored) {
				t.Fatalf("status JSON evidence: %+v", status.State.LastRebase)
			}
			text := f.must(t, "pr", "status", p)
			if len(tc.dropped) == 0 {
				if strings.Contains(text, "no longer changed") || strings.Contains(text, "--abort") {
					t.Fatalf("kept paths printed a warning:\n%s", text)
				}
			} else {
				for _, want := range []string{"Warning: no longer changed: a.txt\n", "swarm pr work " + p, "swarm pr rebase " + p + " --abort", "paused rebase only"} {
					if !strings.Contains(text, want) {
						t.Fatalf("status lacks %q:\n%s", want, text)
					}
				}
				for _, unwanted := range []string{"no longer changed: b.txt", "no longer changed: new.txt"} {
					if strings.Contains(text, unwanted) {
						t.Fatalf("status reported %q:\n%s", unwanted, text)
					}
				}
			}
			current, err := f.tracker.GetPullRequest(context.Background(), number)
			if err != nil {
				t.Fatal(err)
			}
			if current.HeadSHA != original.HeadSHA {
				t.Fatal("rebase published the candidate")
			}
		})
	}
}

func TestCLIPRResolvedRebaseReportsPathDroppedByRepair(t *testing.T) {
	f, issue, number := twoFilePRFixture(t)
	p := strconv.Itoa(number)
	before := f.state(t, issue)
	policy := "required_checks:\n  - name: repaired-behavior\n    command: 'test ! -e b.txt'\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	if err := os.WriteFile(filepath.Join(f.root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "a.txt"), []byte("upstream a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "add", ".")
	prGit(t, f.root, "commit", "-m", "New target policy and conflicting a.txt")
	f.must(t, "issue", "approve", strconv.Itoa(issue))
	// The resolver keeps both PR paths; the repair removes b.txt's change.
	t.Setenv("SWARM_MOCK_DEVELOPER_CMD", "if [ -f .swarm/active/REPAIR_PATHS.md ]; then rm b.txt; else printf 'resolved a\\n' > a.txt; fi")
	out := f.must(t, "pr", "rebase", p, "--resolve", "--agent", "mock", "--progress=false")
	for _, want := range []string{"Warning: no longer changed: b.txt\n", "swarm pr work " + p, "swarm pr rebase " + p + " --abort", "Validation: PASS"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rebase output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "no longer changed: a.txt") {
		t.Fatalf("kept path reported:\n%s", out)
	}
	st := f.state(t, issue)
	r := st.LastRebase
	if r == nil || r.RepairAttempts != 1 || !r.DroppedPathsVerified || !reflect.DeepEqual(r.DroppedPaths, []string{"b.txt"}) {
		t.Fatalf("repair evidence: %+v", r)
	}
	if r.OldHead != before.HeadSHA || r.NewHead != st.PRRevision.LocalHeadSHA {
		t.Fatalf("evidence tuple %s -> %s, want %s -> %s", r.OldHead, r.NewHead, before.HeadSHA, st.PRRevision.LocalHeadSHA)
	}
	status := readPROutcome(t, f.must(t, "pr", "status", p, "--json"))
	if !reflect.DeepEqual(status.State.LastRebase.DroppedPaths, []string{"b.txt"}) {
		t.Fatalf("status JSON: %+v", status.State.LastRebase)
	}
}
