package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
)

const developerOutputRerun = " --phase implement --developer-instructions 'Record notes, test results, and blocked checks in .swarm/active/IMPLEMENTATION.md. Do not change other .swarm/active/ files except LESSONS.md.'"

func TestDeveloperOutputRejectionGuidance(t *testing.T) {
	t.Parallel()
	const validation = ".swarm/active/VALIDATION.md"
	for _, tc := range []struct {
		name  string
		role  string
		paths []string
		// hint applies without harness changes; harnessHint when they are allowed.
		hint, harnessHint bool
	}{
		{"validation only", "developer", []string{validation}, true, true},
		{"duplicate validation", "developer", []string{validation, validation}, true, true},
		{"accepted edits plus validation", "developer", []string{"src/main.go", ".swarm/active/IMPLEMENTATION.md", ".swarm/active/LESSONS.md", validation}, true, true},
		{"tester", "tester", []string{".swarm/active/IMPLEMENTATION.md"}, false, false},
		{"plan", "developer", []string{".swarm/active/PLAN.md"}, false, false},
		{"test plan", "developer", []string{".swarm/active/TEST_PLAN.md"}, false, false},
		{"workunit", "developer", []string{".swarm/active/WORKUNIT.md"}, false, false},
		{"system prompt", "developer", []string{".swarm/active/SYSTEM_PROMPT.md"}, false, false},
		{"unknown active file", "developer", []string{".swarm/active/unknown"}, false, false},
		{"validation with approved input", "developer", []string{validation, ".swarm/active/PLAN.md"}, false, false},
		{"validation with protected path", "developer", []string{validation, "policy/checks"}, false, true},
	} {
		for _, harness := range []bool{false, true} {
			for _, fork := range []bool{false, true} {
				err := writableImportPolicy(tc.role, tc.paths, []string{"policy/**"}, harness, fork)
				var rejection *importRejection
				if !errors.As(err, &rejection) {
					t.Fatalf("%s harness=%v fork=%v: expected rejection, got %v", tc.name, harness, fork, err)
				}
				hint := tc.hint
				if harness {
					hint = tc.harnessHint
				}
				if got := rejection.developerOutputOnly(); got != hint {
					t.Fatalf("%s harness=%v fork=%v: developerOutputOnly=%v want %v (%v)", tc.name, harness, fork, got, hint, rejection.Paths)
				}
				reason := importRejectionReason(rejection, 7, 0)
				if hint != strings.Contains(reason, "--developer-instructions") || hint != strings.Contains(reason, "record results in .swarm/active/IMPLEMENTATION.md") {
					t.Fatalf("%s harness=%v fork=%v: reason %q", tc.name, harness, fork, reason)
				}
			}
		}
	}
	if (&importRejection{Role: "developer"}).developerOutputOnly() {
		t.Fatal("empty rejection must not get developer output guidance")
	}
}

func TestImportRejectionReasonRouting(t *testing.T) {
	t.Parallel()
	hinted := &importRejection{Paths: []string{".swarm/active/VALIDATION.md"}, Role: "developer"}
	generic := &importRejection{Paths: []string{".swarm/active/PLAN.md"}, Role: "developer"}
	for _, tc := range []struct {
		rejection *importRejection
		issue, pr int
		want      string
	}{
		{hinted, 7, 0, "rerun with swarm issue work 7" + developerOutputRerun},
		{hinted, 7, 42, "rerun with swarm pr work 42" + developerOutputRerun},
		{generic, 7, 0, "rejected agent changes to: .swarm/active/PLAN.md; returned changes were not imported; worktree was left unchanged; rerun with swarm issue work 7 --phase implement"},
		{generic, 7, 42, "rejected agent changes to: .swarm/active/PLAN.md; returned changes were not imported; worktree was left unchanged; rerun with swarm pr work 42 --phase implement"},
	} {
		got := importRejectionReason(tc.rejection, tc.issue, tc.pr)
		if !strings.HasSuffix(got, tc.want) {
			t.Fatalf("got %q want suffix %q", got, tc.want)
		}
	}
}

const rejectedTestPatch = "diff --git a/src/main.go b/src/main.go\nnew file mode 100644\n--- /dev/null\n+++ b/src/main.go\n@@ -0,0 +1 @@\n+package main\n"

// rejectingRunner returns the developer's import check like the sandbox does,
// handing a recoverable rejection's patch to the coordinator first. deleted
// lists the subset of paths the run deleted.
func rejectingRunner(paths []string, deleted ...string) func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
	mock := &agent.MockRunner{}
	return func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			if req.Role != "developer" {
				return mock.Run(ctx, req)
			}
			err := req.CheckImport(ctx, paths, deleted)
			var recoverable sandbox.RecoverableImport
			if req.PreserveRejected != nil && errors.As(err, &recoverable) && recoverable.RecoverableImport() {
				req.PreserveRejected(ctx, err, []byte(rejectedTestPatch))
			}
			return nil, err
		}}, nil
	}
}

func recoveryDirEntries(t *testing.T, state string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "swarm", "recovery"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var patches []os.DirEntry
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".patch") {
			patches = append(patches, entry)
		}
	}
	return patches
}

func TestDeveloperOutputImportRecoveryGuidance(t *testing.T) {
	e, issue := budgetEngine(t)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	e.NewRunner = rejectingRunner([]string{"src/main.go", ".swarm/active/IMPLEMENTATION.md", ".swarm/active/VALIDATION.md"})
	opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true}
	res, err := e.ExecuteWork(t.Context(), opts)
	var rejection *importRejection
	if !errors.As(err, &rejection) {
		t.Fatalf("expected rejection: %v", err)
	}
	st, loadErr := e.LeaseManager.GetCurrentState(t.Context(), issue.Number)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	patches := recoveryDirEntries(t, state)
	if len(patches) != 1 {
		t.Fatalf("saved %d recovery patches", len(patches))
	}
	name := patches[0].Name()
	n := strconv.Itoa(issue.Number)
	rerun := "swarm issue work " + n + developerOutputRerun
	for _, text := range []string{"rejected agent changes to: .swarm/active/VALIDATION.md;", "worktree was left unchanged", "developers record results in .swarm/active/IMPLEMENTATION.md, not .swarm/active/VALIDATION.md", rerun, "recovery patch " + name, "saved privately to your state directory", "a maintainer may inspect the saved text and provide reviewed guidance"} {
		if !strings.Contains(err.Error(), text) || !strings.Contains(st.NextPhaseReason, text) {
			t.Fatalf("missing %q: %v %+v", text, err, st)
		}
	}
	if strings.Contains(err.Error(), "recover-import") || strings.Contains(st.NextPhaseReason, "recover-import") {
		t.Fatal("unsupported recovery command advertised")
	}
	// Only the local error names the private path; the projected reason does not.
	path := filepath.Join(state, "swarm", "recovery", name)
	if !strings.Contains(err.Error(), path) || strings.Contains(st.NextPhaseReason, state) {
		t.Fatalf("path placement: %v / %s", err, st.NextPhaseReason)
	}
	if !strings.HasPrefix(st.NextPhaseReason, "rejected agent changes to:") {
		t.Fatalf("reason prefix changed: %s", st.NextPhaseReason)
	}
	header, body, loadErr := agent.LoadRecoveryPatch(name)
	if loadErr != nil || string(body) != rejectedTestPatch || header.Issue != issue.Number || header.PR != 0 || header.Role != "developer" || len(header.HeadSHA) != 40 || !reflect.DeepEqual(header.Rejected, []string{".swarm/active/VALIDATION.md"}) {
		t.Fatalf("saved patch: %+v %q %v", header, body, loadErr)
	}
	if st.Lease.NextPhase != "implement" || res.ClaimBudgetPause != nil {
		t.Fatalf("bad recovery: %+v", res)
	}
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactValidation || a.Type == contract.ArtifactImplementation {
			t.Fatalf("rejected run recorded artifact: %+v", a)
		}
	}
}

func TestRejectedImportNonRecoverableSavesNothing(t *testing.T) {
	for _, path := range []string{".swarm/active/PLAN.md", ".swarm/active/WORKUNIT.md", ".swarm/active/TEST_PLAN.md", ".swarm/validation/required-checks.yaml"} {
		t.Run(path, func(t *testing.T) {
			e, issue := budgetEngine(t)
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			e.NewRunner = rejectingRunner([]string{"src/main.go", ".swarm/active/VALIDATION.md", path})
			_, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
			var rejection *importRejection
			if !errors.As(err, &rejection) || rejection.Recoverable {
				t.Fatalf("expected a non-recoverable rejection: %v", err)
			}
			if patches := recoveryDirEntries(t, state); len(patches) != 0 {
				t.Fatalf("saved %v", patches)
			}
			st, loadErr := e.LeaseManager.GetCurrentState(t.Context(), issue.Number)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if strings.Contains(err.Error(), "recover-import") || strings.Contains(st.NextPhaseReason, "recover") {
				t.Fatalf("offered recovery: %v", err)
			}
		})
	}
}

func TestImportRejectionReasonWithPatch(t *testing.T) {
	t.Parallel()
	const name = "rejected-0123456789abcdef0123456789abcdef.patch"
	saved := &importRejection{Paths: []string{".swarm/active/REVIEW.md"}, Role: "tester", Recoverable: true, Patch: "/home/someone/.local/state/swarm/recovery/" + name}
	unsaved := &importRejection{Paths: []string{".swarm/active/REVIEW.md"}, Role: "tester", Recoverable: true, PatchNote: "a complete recovery patch was unavailable"}
	// This command/flag contract is matched by TestCLIImportRecoveryCommands.
	for _, route := range []struct {
		pr      int
		command string
	}{{0, "swarm issue work 7"}, {42, "swarm pr work 42"}} {
		for _, rejection := range []*importRejection{saved, unsaved, {Paths: saved.Paths, Role: "tester", Recoverable: true}} {
			got := importRejectionReason(rejection, 7, route.pr)
			want := route.command + " --phase implement"
			if rejection == saved {
				want += "; recovery patch " + name + " was saved privately to your state directory; a maintainer may inspect the saved text and provide reviewed guidance through the existing work instruction flags"
			} else if rejection == unsaved {
				want += "; no recovery patch: a complete recovery patch was unavailable"
			}
			if !strings.HasSuffix(got, want) || !strings.Contains(got, "worktree was left unchanged") || strings.Contains(got, "/home/") || strings.Contains(got, "recover-import") || strings.Contains(got, "keep the other changes") {
				t.Fatalf("got %q want suffix %q", got, want)
			}
		}
	}

}

var errStopAfterImport = errors.New("stop after import check")

func TestDeletionImportRejection(t *testing.T) {
	const handler = "internal/api/handler.go"
	for _, tc := range []struct {
		name, plan string
		paths      []string
		deleted    bool // want an unplanned-deletion rejection
		protected  bool // want the existing protected-path rejection
	}{
		{name: "unmentioned", paths: []string{"src/main.go", handler}, deleted: true},
		{name: "plan calls it new", plan: "- Create new file `" + handler + "`", paths: []string{"src/main.go", handler}, deleted: true},
		{name: "directive", plan: "- Delete `" + handler + "` (replaced by src/main.go)", paths: []string{"src/main.go", handler}},
		{name: "protected path first", paths: []string{handler, ".swarm/validation/required-checks.yaml"}, protected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, issue := budgetEngine(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("SWARM_MOCK_PLAN_APPEND", tc.plan)
			var importErr error
			mock := &agent.MockRunner{}
			e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					if req.Role != "developer" {
						return mock.Run(ctx, req)
					}
					if importErr = req.CheckImport(ctx, tc.paths, []string{handler}); importErr != nil {
						return nil, importErr
					}
					return nil, errStopAfterImport
				}}, nil
			}
			res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
			if !tc.deleted && !tc.protected {
				if importErr != nil || !errors.Is(err, errStopAfterImport) {
					t.Fatalf("planned deletion rejected: %v / %v", importErr, err)
				}
				return
			}
			var rejection *importRejection
			if !errors.As(err, &rejection) || rejection.Deleted != tc.deleted || rejection.Recoverable {
				t.Fatalf("rejection: %v", err)
			}
			st, loadErr := e.LeaseManager.GetCurrentState(t.Context(), issue.Number)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if st.Lease.NextPhase != "implement" || st.HeadSHA != "" {
				t.Fatalf("checkpoint advanced: %+v", st)
			}
			for _, a := range st.Artifacts {
				if a.Type == contract.ArtifactImplementation {
					t.Fatalf("rejected run recorded artifact: %+v", a)
				}
			}
			if _, statErr := os.Stat(filepath.Join(res.WorktreePath, filepath.FromSlash(handler))); statErr != nil {
				t.Fatalf("worktree changed: %v", statErr)
			}
			if tc.deleted {
				for _, text := range []string{handler + " (unplanned deletion of approved-base files)", ".swarm/active/REPAIR.md", "`- Delete path/to/file.go`", "rerun with swarm issue work " + strconv.Itoa(issue.Number) + " --phase implement"} {
					if !strings.Contains(st.NextPhaseReason, text) || !strings.Contains(err.Error(), text) {
						t.Fatalf("missing %q: %v / %s", text, err, st.NextPhaseReason)
					}
				}
				if strings.Contains(err.Error(), "src/main.go") {
					t.Fatalf("edit named as deletion: %v", err)
				}
			}
		})
	}
}

func TestCompletedTesterRejectionReason(t *testing.T) {
	for _, pr := range []int{0, 42} {
		e, issue := budgetEngine(t)
		mock := &agent.MockRunner{}
		e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
			return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
				if req.Role != "tester" {
					return mock.Run(ctx, req)
				}
				err := req.CheckImport(ctx, []string{".swarm/active/IMPLEMENTATION.md"}, nil)
				return &agent.RunResult{Role: "tester"}, &sandbox.CompletedImportError{Cause: err}
			}}, nil
		}
		// PR routing is tested directly: a PR ExecuteWork requires its revision state.
		res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
		var rejection *importRejection
		if !errors.As(err, &rejection) || !rejection.Completed {
			t.Fatalf("classification: %v", err)
		}
		reason := importRejectionReason(rejection, issue.Number, pr, true)
		rerun := "swarm issue work " + strconv.Itoa(issue.Number)
		if pr != 0 {
			rerun = "swarm pr work 42"
		}
		for _, text := range []string{reason, err.Error(), res.State.NextPhaseReason} {
			for _, want := range []string{"tester run completed", ".swarm/active/IMPLEMENTATION.md", "--phase test", "--tester-instructions"} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %s: %s", want, text)
				}
			}
			for _, forbidden := range []string{"agent failed", "binary or oversized changes"} {
				if strings.Contains(text, forbidden) {
					t.Fatal(text)
				}
			}
		}
		if !strings.Contains(reason, rerun) {
			t.Fatal(reason)
		}
	}
}

func TestCompletedTesterScratchRejectionReason(t *testing.T) {
	for _, pr := range []int{0, 42} {
		e, issue := budgetEngine(t)
		mock := &agent.MockRunner{}
		e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
			return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
				if req.Role != "tester" {
					return mock.Run(ctx, req)
				}
				err := req.CheckImport(ctx, []string{".swarm/active/VALIDATION_CHECK.log"}, nil)
				return &agent.RunResult{Role: "tester"}, &sandbox.CompletedImportError{Cause: err}
			}}, nil
		}
		// PR routing is tested directly: a PR ExecuteWork requires its revision state.
		res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
		var rejection *importRejection
		if !errors.As(err, &rejection) || !rejection.Completed {
			t.Fatalf("classification: %v", err)
		}
		if rejection.OutputOnly || rejection.Recoverable || !rejection.ActiveDirectoryImport() {
			t.Fatalf("recovery broadened: %+v", rejection)
		}
		reason := importRejectionReason(rejection, issue.Number, pr, true)
		rerun := "swarm issue work " + strconv.Itoa(issue.Number)
		if pr != 0 {
			rerun = "swarm pr work 42"
		}
		for _, text := range []string{reason, err.Error(), res.State.NextPhaseReason} {
			for _, want := range []string{"tester run completed", ".swarm/active/VALIDATION_CHECK.log", "--phase test", "--tester-instructions", "TMPDIR", "worktree was left unchanged"} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %s: %s", want, text)
				}
			}
			for _, forbidden := range []string{"agent failed", "binary or oversized changes", "recover-import"} {
				if strings.Contains(text, forbidden) {
					t.Fatal(text)
				}
			}
		}
		if strings.Count(reason, "'") != 2 {
			t.Fatalf("unsafe quoted guidance: %s", reason)
		}
		if !strings.Contains(reason, rerun) {
			t.Fatal(reason)
		}
	}
}
