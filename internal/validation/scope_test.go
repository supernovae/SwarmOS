package validation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

func setupScopedModuleFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                         "module example.com/scoped\n\ngo 1.24\n",
		"cmd/changelog/main.go":          "package main\nimport _ \"example.com/scoped/internal/changelog\"\nfunc main() {}\n",
		"internal/changelog/check.go":    "package changelog\nfunc Check() {}\n",
		"internal/gitcredential/git.go":  "package gitcredential\nfunc Run() {}\n",
		"internal/safefs/safefs.go":      "package safefs\nfunc Open() {}\n",
		"internal/harness/scaffold.go":   "package harness\nimport _ \"example.com/scoped/internal/safefs\"\nfunc Init() {}\n",
		"internal/lease/manager.go":      "package lease\nfunc Acquire() {}\n",
		"internal/agent/runner.go":       "package agent\nfunc Run() {}\n",
		"internal/cli/root.go":           "package cli\nimport _ \"example.com/scoped/internal/lease\"\nfunc Execute() {}\n",
		"internal/fork/coordinator.go":   "package fork\nfunc Coordinate() {}\n",
		"internal/sandbox/native.go":     "package sandbox\nfunc Box() {}\n",
		"internal/validation/val.go":     "package validation\nimport _ \"example.com/scoped/internal/harness\"\nfunc Validate() {}\n",
		"internal/coordination/store.go": "package coordination\nfunc CAS() {}\n",
		"internal/contract/types.go":     "package contract\ntype WorkUnit struct{}\n",
	}
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestScopeChecksTierAndDiffFiltering(t *testing.T) {
	t.Parallel()
	root := setupScopedModuleFixture(t)
	spec, err := harness.LoadValidationSpec(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	namesOf := func(checks []contract.CheckSpec) []string {
		out := make([]string, len(checks))
		for i, c := range checks {
			out[i] = c.Name
		}
		return out
	}
	cmdMap := func(checks []contract.CheckSpec) map[string]string {
		out := make(map[string]string, len(checks))
		for _, c := range checks {
			out[c.Name] = c.Command
		}
		return out
	}

	t.Run("docs_only_skips_go_in_local_and_ci", func(t *testing.T) {
		for _, ci := range []bool{false, true} {
			active, note := scopeChecks(root, spec.RequiredChecks, []string{"README.md", "docs/architecture.md"}, ci)
			if len(active) != 0 {
				t.Fatalf("ci=%t: expected 0 checks for docs-only diff, got %v", ci, namesOf(active))
			}
			if !strings.Contains(note, "skipped outside diff scope") {
				t.Fatalf("ci=%t: expected scope note, got %q", ci, note)
			}
		}
	})

	t.Run("changelog_fragment_runs_only_changelog_regressions", func(t *testing.T) {
		active, _ := scopeChecks(root, spec.RequiredChecks, []string{"changes/unreleased/555.md"}, false)
		if got := namesOf(active); !reflect.DeepEqual(got, []string{"changelog-regressions"}) {
			t.Fatalf("got %v, want [changelog-regressions]", got)
		}
	})

	t.Run("leaf_package_local_narrows_to_changed_and_direct_importers", func(t *testing.T) {
		active, note := scopeChecks(root, spec.RequiredChecks, []string{"internal/changelog/check.go"}, false)
		want := []string{"fmt", "build", "lint", "staticcheck", "cli-smoke-security", "changelog-regressions"}
		if got := namesOf(active); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v (note=%s)", got, want, note)
		}
		cmds := cmdMap(active)
		if cmds["build"] != "go build ./cmd/changelog ./internal/changelog" {
			t.Fatalf("unexpected narrowed build command: %q", cmds["build"])
		}
		if cmds["lint"] != "go vet ./cmd/changelog ./internal/changelog" {
			t.Fatalf("unexpected narrowed lint command: %q", cmds["lint"])
		}
		if cmds["staticcheck"] != "go tool staticcheck ./cmd/changelog ./internal/changelog" {
			t.Fatalf("unexpected narrowed staticcheck command: %q", cmds["staticcheck"])
		}
		if !strings.Contains(note, "deferred to CI (cli-regressions)") {
			t.Fatalf("missing CI deferral note: %q", note)
		}
	})

	t.Run("one_hop_importer_triggers_dependent_check_only", func(t *testing.T) {
		// internal/safefs is imported directly by internal/harness (1-hop),
		// NOT by internal/validation (2-hop).
		active, _ := scopeChecks(root, spec.RequiredChecks, []string{"internal/safefs/safefs.go"}, false)
		want := []string{"fmt", "build", "lint", "staticcheck", "cli-smoke-security", "filesystem-security", "core-regressions"}
		if got := namesOf(active); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		cmds := cmdMap(active)
		if cmds["filesystem-security"] != "go test -timeout=5m ./internal/safefs -count=1" {
			t.Fatalf("unexpected filesystem-security command: %q", cmds["filesystem-security"])
		}
		if cmds["core-regressions"] != "go test -timeout=10m ./internal/harness" {
			t.Fatalf("unexpected core-regressions command: %q", cmds["core-regressions"])
		}
	})

	t.Run("ci_mode_runs_full_policy_on_go_diff", func(t *testing.T) {
		active, note := scopeChecks(root, spec.RequiredChecks, []string{"internal/changelog/check.go"}, true)
		if len(active) != len(spec.RequiredChecks) {
			t.Fatalf("CI should run all %d checks on Go diff, got %d (%v)", len(spec.RequiredChecks), len(active), namesOf(active))
		}
		if note != "" {
			t.Fatalf("unexpected scope note in full CI run: %q", note)
		}
		cmds := cmdMap(active)
		if cmds["build"] != "go build ./..." {
			t.Fatalf("CI should not narrow build command, got %q", cmds["build"])
		}
	})

	t.Run("empty_modified_runs_full_policy_in_ci", func(t *testing.T) {
		active, note := scopeChecks(root, spec.RequiredChecks, nil, true)
		if len(active) != len(spec.RequiredChecks) || note != "" {
			t.Fatalf("push to main (empty diff) must run all checks: got %d, note=%q", len(active), note)
		}
	})
}

func TestValidateDocsOnlyStillEnforcesProtectedPathsAndIgnoresCandidatePolicy(t *testing.T) {
	repo := setupGitRepoForValidation(t)
	policy := strings.Join([]string{
		"required_checks:",
		"  - name: heavy-go",
		"    command: 'exit 9'",
		"    timeout: 1m",
		"    scope: affected",
		"  - name: ci-only",
		"    command: 'exit 8'",
		"    timeout: 1m",
		"    tier: ci",
		"protected_paths:",
		"  - .swarm/validation/**",
		"  - AGENTS.md",
	}, "\n") + "\n"
	base := commitPolicy(t, repo, policy)
	if err := os.MkdirAll(filepath.Join(repo, ".swarm", "active"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, art := range []string{"PLAN.md", "TEST_PLAN.md"} {
		if err := os.WriteFile(filepath.Join(repo, ".swarm", "active", art), []byte("# Plan\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Pure docs edit passes local validation without invoking heavy-go or ci-only.
	commitRepoChange(t, repo, "README.md", "# Updated Docs\n")
	report, err := Validate(context.Background(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base})
	if err != nil || report == nil || !report.Passed || len(report.CheckOutcomes) != 0 {
		t.Fatalf("docs-only local validation failed or ran skipped checks: report=%+v err=%v", report, err)
	}
	if !strings.Contains(report.Artifact.Body, "deferred to CI (ci-only)") || !strings.Contains(report.Artifact.Body, "skipped outside diff scope (heavy-go)") {
		t.Fatalf("missing scope explanation in report body:\n%s", report.Artifact.Body)
	}

	// Protected path edit on a docs-only branch is still rejected before check scoping.
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(context.Background(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base}); !errors.Is(err, ErrProtectedPathModified) {
		t.Fatalf("expected ErrProtectedPathModified, got %v", err)
	}
}
