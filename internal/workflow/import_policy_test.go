package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
)

func TestWritableImportPolicy(t *testing.T) {
	for _, role := range []string{"developer", "tester"} {
		for _, harness := range []bool{false, true} {
			for _, fork := range []bool{false, true} {
				t.Run(role+string(rune('0'+boolInt(harness)*2+boolInt(fork))), func(t *testing.T) {
					output, other := "IMPLEMENTATION.md", "VALIDATION.md"
					if role == "tester" {
						output, other = other, output
					}
					paths := []string{"src/main.go", ".swarm/active/" + output, ".swarm/active/LESSONS.md"}
					if err := writableImportPolicy(role, paths, []string{"policy/**"}, harness, fork); err != nil {
						t.Fatal(err)
					}
					paths = []string{".swarm/active/PLAN.md", ".swarm/active/TEST_PLAN.md", ".swarm/active/WORKUNIT.md", ".swarm/active/SYSTEM_PROMPT.md", ".swarm/active/unknown", ".swarm/active/" + other}
					paths = append(paths, "policy/checks", ".swarm/config.yaml", ".github/workflows/ci.yml", paths[0])
					err := writableImportPolicy(role, paths, []string{"policy/**"}, harness, fork)
					var rejected *importRejection
					if !errors.As(err, &rejected) {
						t.Fatal(err)
					}
					want := []string{}
					want = append(want, paths[:6]...)
					if !harness {
						want = append(want, "policy/checks")
					}
					if fork {
						want = append(want, ".swarm/config.yaml", ".github/workflows/ci.yml")
					}
					sort.Strings(want)
					if !reflect.DeepEqual(rejected.Paths, want) {
						t.Fatalf("got %v want %v", rejected.Paths, want)
					}
				})
			}
		}
	}
}
func TestImportPolicyRecoverable(t *testing.T) {
	const (
		implementation = ".swarm/active/IMPLEMENTATION.md"
		validation     = ".swarm/active/VALIDATION.md"
		review         = ".swarm/active/REVIEW.md"
	)
	patterns := []string{"policy/**", ".swarm/validation/**"}
	for _, tc := range []struct {
		name        string
		role        string
		paths       []string
		fork        bool
		recoverable bool
	}{
		{"developer validation", "developer", []string{"src/a.go", implementation, ".swarm/active/LESSONS.md", validation}, false, true},
		{"tester implementation", "tester", []string{"src/a.go", validation, implementation}, false, true},
		{"developer review", "developer", []string{review}, false, true},
		{"duplicate output", "developer", []string{validation, validation}, false, true},
		{"plan", "developer", []string{validation, ".swarm/active/PLAN.md"}, false, false},
		{"workunit", "developer", []string{validation, ".swarm/active/WORKUNIT.md"}, false, false},
		{"test plan", "developer", []string{validation, ".swarm/active/TEST_PLAN.md"}, false, false},
		{"system prompt", "developer", []string{validation, ".swarm/active/SYSTEM_PROMPT.md"}, false, false},
		{"workunit json", "developer", []string{validation, ".swarm/active/workunit.json"}, false, false},
		{"guidance", "developer", []string{validation, ".swarm/active/guidance/x"}, false, false},
		{"protected policy", "developer", []string{validation, ".swarm/validation/required-checks.yaml"}, false, false},
		{"protected pattern", "developer", []string{validation, "policy/checks"}, false, false},
		{"fork", "developer", []string{validation}, true, false},
	} {
		err := writableImportPolicy(tc.role, tc.paths, patterns, false, tc.fork)
		var rejection *importRejection
		if !errors.As(err, &rejection) {
			t.Fatalf("%s: expected rejection, got %v", tc.name, err)
		}
		if rejection.Recoverable != tc.recoverable || rejection.RecoverableImport() != tc.recoverable {
			t.Fatalf("%s: recoverable=%v want %v (%v)", tc.name, rejection.Recoverable, tc.recoverable, rejection.Paths)
		}
	}
	// A role output under a protected pattern is a protected-path rejection.
	if err := writableImportPolicy("developer", []string{validation}, []string{".swarm/**"}, false, false); err.(*importRejection).Recoverable {
		t.Fatal("protected role output was recoverable")
	}
	for _, tc := range []struct {
		rejected []string
		want     bool
	}{
		{[]string{validation}, true},
		{[]string{validation, review}, true},
		{[]string{implementation}, false}, // the developer may write it; nothing to drop
		{[]string{".swarm/active/PLAN.md"}, false},
		{[]string{".swarm/active/WORKUNIT.md"}, false},
		{[]string{"policy/checks"}, false},
		{[]string{validation, "src/a.go"}, false},
		{nil, false},
	} {
		if got := RecoverableRejection("developer", tc.rejected, patterns, false); got != tc.want {
			t.Fatalf("RecoverableRejection(%v) = %v", tc.rejected, got)
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestImportRejectionRecovery(t *testing.T) {
	for _, role := range []string{"developer", "tester"} {
		t.Run(role, func(t *testing.T) {
			e, issue := budgetEngine(t)
			mock := &agent.MockRunner{}
			e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					if req.Role == role {
						return nil, req.CheckImport(ctx, []string{".swarm/active/TEST_PLAN.md", ".swarm/active/PLAN.md"})
					}
					return mock.Run(ctx, req)
				}}, nil
			}
			opts := WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true}
			res, err := e.ExecuteWork(t.Context(), opts)
			var rejection *importRejection
			if !errors.As(err, &rejection) {
				t.Fatalf("expected rejection: %v", err)
			}
			state, loadErr := e.LeaseManager.GetCurrentState(t.Context(), issue.Number)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			for _, text := range []string{"PLAN.md", "TEST_PLAN.md", "worktree was left unchanged", "--phase implement"} {
				if !strings.Contains(err.Error(), text) || !strings.Contains(state.NextPhaseReason, text) {
					t.Fatalf("missing %q: %v %+v", text, err, state)
				}
			}
			if state.Lease.NextPhase != "implement" || res.ClaimBudgetPause != nil {
				t.Fatalf("bad recovery: %+v", res)
			}
			for _, a := range state.Artifacts {
				if a.Type == contract.ArtifactValidation || role == "developer" && a.Type == contract.ArtifactImplementation {
					t.Fatalf("rejected run recorded artifact: %+v", a)
				}
			}
			for _, name := range []string{"PLAN.md", "TEST_PLAN.md", "WORKUNIT.md"} {
				if err := os.WriteFile(filepath.Join(res.WorktreePath, ".swarm/active", name), []byte("corrupt before resume"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
				return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
					if req.Role == "tester" {
						for _, name := range []string{"PLAN.md", "TEST_PLAN.md", "WORKUNIT.md"} {
							data, err := os.ReadFile(filepath.Join(req.WorktreePath, ".swarm/active", name))
							if err != nil || strings.Contains(string(data), "corrupt before resume") {
								t.Fatalf("resumed tester read corrupt %s: %v", name, err)
							}
						}
					}
					return mock.Run(ctx, req)
				}}, nil
			}
			opts.Phase = "implement"
			retry, err := e.ExecuteWork(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if !retry.ValidationPassed || retry.State.NextPhaseReason != "" {
				t.Fatalf("retry: %+v", retry)
			}
		})
	}
}

func TestTesterReadsApprovedInputs(t *testing.T) {
	e, issue := budgetEngine(t)
	mock := &agent.MockRunner{}
	seen := false
	e.NewRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
		return &budgetTestRunner{run: func(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
			result, err := mock.Run(ctx, req)
			if err != nil {
				return result, err
			}
			if req.Role == "developer" {
				// Model an old/local corrupted input independently of the import boundary.
				for _, name := range []string{"PLAN.md", "TEST_PLAN.md", "WORKUNIT.md"} {
					if err := os.WriteFile(filepath.Join(req.WorktreePath, ".swarm/active", name), []byte("corrupt"), 0644); err != nil {
						return nil, err
					}
				}
			}
			if req.Role == "tester" {
				seen = true
				state, err := e.LeaseManager.GetCurrentState(ctx, issue.Number)
				if err != nil {
					return nil, err
				}
				for _, a := range state.Artifacts {
					name := ""
					if a.Type == contract.ArtifactDesign {
						name = "PLAN.md"
					}
					if a.Type == contract.ArtifactTestPlan {
						name = "TEST_PLAN.md"
					}
					if name != "" {
						data, err := os.ReadFile(filepath.Join(req.WorktreePath, ".swarm/active", name))
						if err != nil || string(data) != canonicalReport(a.Body) {
							t.Fatalf("untrusted %s: %q %v", name, data, err)
						}
					}
				}
				data, err := os.ReadFile(filepath.Join(req.WorktreePath, ".swarm/active/WORKUNIT.md"))
				if err != nil || !strings.Contains(string(data), "Preserve work") {
					t.Fatalf("untrusted workunit: %q %v", data, err)
				}
			}
			return result, nil
		}}, nil
	}
	res, err := e.ExecuteWork(t.Context(), WorkOptions{RepoRoot: e.repoRoot, IssueNumber: issue.Number, Owner: "maintainer", AgentName: "mock", Phase: "all", Headless: true})
	if err != nil || !seen || !res.ValidationPassed {
		t.Fatalf("tester: %v seen=%t", err, seen)
	}
}

func TestApprovedInputsRefuseMissingStateAndSymlinks(t *testing.T) {
	for _, kind := range []string{"missing", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			active := filepath.Join(dir, ".swarm/active")
			if err := os.MkdirAll(active, 0755); err != nil {
				t.Fatal(err)
			}
			state := &lease.IssueState{Artifacts: []contract.Artifact{{Type: contract.ArtifactDesign, Body: "approved plan"}, {Type: contract.ArtifactTestPlan, Body: "approved tests"}}}
			if kind == "missing" {
				state.Artifacts = nil
			} else {
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte("unchanged"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(active, "PLAN.md")); err != nil {
					t.Fatal(err)
				}
			}
			if err := ensurePlanArtifactsInWorktree(dir, state); err == nil {
				t.Fatal("accepted unsafe approved inputs")
			}
			if kind == "symlink" {
				data, _ := os.ReadFile(filepath.Join(dir, "target"))
				if string(data) != "unchanged" {
					t.Fatal("followed symlink")
				}
			}
		})
	}
}

func TestWritableImportPolicyUsesTrustedPatterns(t *testing.T) {
	// A returned policy cannot remove the patterns captured from the approved base.
	paths := []string{".swarm/validation/required-checks.yaml", "AGENTS.md", ".swarm/active/IMPLEMENTATION.md"}
	err := writableImportPolicy("developer", paths, []string{".swarm/validation/**", "AGENTS.md"}, false, false)
	var rejection *importRejection
	if !errors.As(err, &rejection) || !reflect.DeepEqual(rejection.Paths, []string{".swarm/validation/required-checks.yaml", "AGENTS.md"}) {
		t.Fatalf("trusted policy: %v", err)
	}
	if err := writableImportPolicy("developer", paths, []string{".swarm/validation/**", "AGENTS.md"}, true, false); err != nil {
		t.Fatal(err)
	}
	if err := writableImportPolicy("developer", []string{".swarm/active/IMPLEMENTATION.md.extra"}, nil, true, true); err == nil {
		t.Fatal("output prefix authorized an unrelated file")
	}
}
