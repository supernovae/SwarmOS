package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
)

var guidanceRoles = []string{"planner", "reviewer", "developer", "tester"}

// guidancePolicy gives every always-inline document a unique marker.
var guidancePolicy = map[string]string{
	".swarm/project.md":          "# Project\n\nproject-policy-marker\n",
	".swarm/conventions.md":      "# Conventions\n\nconventions-policy-marker\n",
	".swarm/goals.md":            "# Goals\n\ngoals-policy-marker\n",
	".swarm/SKILLS.md":           "# Manual\n\nmanual-policy-marker\n",
	".swarm/rules/default.md":    "# Rules\n\nrules-policy-marker\n",
	".swarm/rules/nested/sec.md": "# Safety\n\nsafety-rule-marker\n",
	".swarm/architecture.md":     "# Architecture\n\narchitecture-small-marker\n",
}

// guidanceRepo commits the scaffolded harness with files applied on top; an
// empty value deletes the file. It returns the repository and base commit.
func guidanceRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runWorkspaceGit(t, repo, "init", "-b", "main")
	runWorkspaceGit(t, repo, "config", "user.name", "Test")
	runWorkspaceGit(t, repo, "config", "user.email", "test@example.test")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: repo}); err != nil {
		t.Fatal(err)
	}
	validation := filepath.Join(repo, ".swarm/validation/required-checks.yaml")
	raw, err := os.ReadFile(validation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(validation, append(raw, []byte("\n# validation-policy-marker\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	merged := map[string]string{}
	for name, body := range guidancePolicy {
		merged[name] = body
	}
	for name, body := range files {
		merged[name] = body
	}
	return repo, commitGuidance(t, repo, merged)
}

func commitGuidance(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if body == "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runWorkspaceGit(t, repo, "add", "-A", ".swarm")
	runWorkspaceGit(t, repo, "commit", "--allow-empty", "-m", "guidance")
	return workspaceGitOutput(t, repo, "rev-parse", "HEAD")
}

func guidanceWorkUnit(marker string) *contract.WorkUnit {
	return &contract.WorkUnit{ID: "ISSUE-9", IssueNumber: 9, Title: marker + "-title", Objective: marker + "-objective", Constraints: []string{marker + "-constraint"}, Acceptance: []string{marker + "-acceptance"}, Workflow: "feature", Gate: contract.GateAgent}
}

func compileGuidance(t *testing.T, wt, base, role string, wu *contract.WorkUnit, arts []contract.Artifact) (string, string) {
	t.Helper()
	if err := CompileHarnessBundleAt(wt, wu, &contract.WorkflowSpec{Name: "feature"}, arts, role, base); err != nil {
		t.Fatal(err)
	}
	return readActive(t, wt, "SYSTEM_PROMPT.md"), readActive(t, wt, "WORKUNIT.md")
}

func readActive(t *testing.T, wt, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(wt, ".swarm/active", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func activeExists(t *testing.T, wt, name string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(wt, ".swarm/active", filepath.FromSlash(name)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// activeText concatenates every generated file under .swarm/active.
func activeText(t *testing.T, wt string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.WalkDir(filepath.Join(wt, ".swarm/active"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		sb.Write(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

func TestBundleGuidanceAllRoles(t *testing.T) {
	t.Parallel()
	skills := map[string]string{
		".swarm/skills/dev.md":              "---\nroles: [developer]\ndescription: Developer helper\n---\n# Dev skill\n\ndev-body-marker\n",
		".swarm/skills/testing.md":          "---\nroles: [tester]\nworkflows: [feature]\n---\n# Testing skill\n\ntesting-body-marker\n",
		".swarm/skills/all.md":              "---\nroles: [\"*\"]\n---\n# Everyone\n\nall-body-marker\n",
		".swarm/skills/bugfix-only.md":      "---\nworkflows: [bugfix]\n---\n# Bugfix\n\nbugfix-body-marker\n",
		".swarm/skills/nested/reference.md": "# Reference\n\nreference-body-marker\n",
	}
	selected := map[string][]string{
		"planner":   {"all-body-marker"},
		"reviewer":  {"all-body-marker"},
		"developer": {"all-body-marker", "dev-body-marker"},
		"tester":    {"all-body-marker", "testing-body-marker"},
	}
	repo, base := guidanceRepo(t, skills)
	for _, role := range guidanceRoles {
		t.Run(role, func(t *testing.T) {
			sys, _ := compileGuidance(t, repo, base, role, guidanceWorkUnit("roles"), nil)
			for _, marker := range []string{"project-policy-marker", "conventions-policy-marker", "goals-policy-marker", "manual-policy-marker", "rules-policy-marker", "safety-rule-marker", "validation-policy-marker", "architecture-small-marker"} {
				if strings.Count(sys, marker) != 1 {
					t.Fatalf("%s must be inline once for %s", marker, role)
				}
			}
			for path, body := range skills {
				if strings.Count(sys, "`"+path+"`") != 1 {
					t.Fatalf("skill index must name %s once", path)
				}
				if copy := readActive(t, repo, "guidance/skills/"+strings.TrimPrefix(path, ".swarm/skills/")); copy != body {
					t.Fatalf("approved copy of %s = %q", path, copy)
				}
			}
			if !strings.Contains(sys, "`.swarm/skills/triage-and-plan.md`") || strings.Contains(sys, "## 1. Interactive Roadmap Authoring") {
				t.Fatal("unannotated scaffold skill should be index-only")
			}
			for _, marker := range []string{"dev-body-marker", "testing-body-marker", "all-body-marker", "bugfix-body-marker", "reference-body-marker"} {
				want := 0
				for _, s := range selected[role] {
					if s == marker {
						want = 1
					}
				}
				if got := strings.Count(sys, marker); got != want {
					t.Fatalf("%s inlined %d times for %s, want %d", marker, got, role, want)
				}
			}
			if strings.Contains(sys, "roles: [") {
				t.Fatal("front matter leaked into the prompt")
			}
		})
	}
}

func TestBundleTwentySkillsSize(t *testing.T) {
	t.Parallel()
	skills := map[string]string{}
	var legacySkills int
	for i := 0; i < 20; i++ {
		front := ""
		if i < 2 {
			front = "---\nroles: [developer]\n---\n"
		}
		body := fmt.Sprintf("%s# Skill %02d\n\nskill-%02d-marker\n\n%s\n", front, i, i, strings.Repeat("Guidance sentence for an optional workflow. ", 50))
		skills[fmt.Sprintf(".swarm/skills/skill-%02d.md", i)] = body
		// The legacy composition inlined every skill file, trimmed, plus a blank line.
		legacySkills += len(strings.TrimSpace(body)) + 2
	}
	bareRepo, bareBase := guidanceRepo(t, map[string]string{".swarm/skills/triage-and-plan.md": ""})
	bare, _ := compileGuidance(t, bareRepo, bareBase, "developer", guidanceWorkUnit("size"), nil)
	skills[".swarm/skills/triage-and-plan.md"] = ""
	repo, base := guidanceRepo(t, skills)
	sys, _ := compileGuidance(t, repo, base, "developer", guidanceWorkUnit("size"), nil)
	legacy := len(bare) + legacySkills
	t.Logf("developer prompt: %d bytes with index, %d bytes with every skill inlined", len(sys), legacy)
	if len(sys) >= legacy {
		t.Fatalf("developer prompt did not shrink: %d >= %d", len(sys), legacy)
	}
	for i := 0; i < 20; i++ {
		if strings.Count(sys, fmt.Sprintf("`.swarm/skills/skill-%02d.md`", i)) != 1 {
			t.Fatalf("skill %02d is not named in the index", i)
		}
		if got, want := strings.Contains(sys, fmt.Sprintf("skill-%02d-marker", i)), i < 2; got != want {
			t.Fatalf("skill %02d inlined = %v, want %v", i, got, want)
		}
	}
}

func TestBundleGuidanceApprovedBase(t *testing.T) {
	t.Parallel()
	approved := "---\nroles: [developer]\n---\n# Shared\n\napproved-skill-marker\n"
	repo, base := guidanceRepo(t, map[string]string{".swarm/skills/shared.md": approved})
	wt, _, err := ProvisionAt(repo, 9, "Guidance", base, "", "")
	if err != nil {
		t.Fatal(err)
	}
	candidate := map[string]string{
		".swarm/skills/shared.md":        "---\nroles: [\"*\"]\ndescription: candidate-description-marker\n---\n# Candidate title marker\n\ncandidate-skill-marker\n",
		".swarm/skills/candidate.md":     "---\nroles: [\"*\"]\n---\n# Candidate only\n\ncandidate-only-marker\n",
		".swarm/rules/default.md":        "candidate-rule-marker\n",
		".swarm/architecture.md":         "# Candidate\n\ncandidate-architecture-marker\n" + strings.Repeat("c", 2*architectureInlineLimit),
		".swarm/skills/nested/extra.md":  "candidate-nested-marker\n",
		".swarm/rules/candidate-rule.md": "candidate-extra-rule-marker\n",
	}
	for name, body := range candidate {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(wt, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		checkpointWrite(t, wt, name, body)
	}
	for _, step := range []string{"uncommitted", "committed beyond base"} {
		t.Run(step, func(t *testing.T) {
			if step != "uncommitted" {
				runWorkspaceGit(t, wt, "add", "-A", ".swarm/skills", ".swarm/rules", ".swarm/architecture.md")
				runWorkspaceGit(t, wt, "commit", "-m", "candidate guidance")
			}
			for _, role := range guidanceRoles {
				sys, _ := compileGuidance(t, wt, base, role, guidanceWorkUnit("base"), nil)
				all := activeText(t, wt)
				for _, marker := range []string{"candidate-description-marker", "Candidate title marker", "candidate-skill-marker", "candidate-only-marker", "candidate-rule-marker", "candidate-architecture-marker", "candidate-nested-marker", "candidate-extra-rule-marker"} {
					if strings.Contains(all, marker) {
						t.Fatalf("%s: candidate guidance %q reached .swarm/active", role, marker)
					}
				}
				if strings.Contains(sys, "candidate.md") || !strings.Contains(sys, "rules-policy-marker") || !strings.Contains(sys, "architecture-small-marker") {
					t.Fatalf("%s: prompt does not reflect the approved base:\n%s", role, sys)
				}
				if strings.Contains(sys, "approved-skill-marker") != (role == "developer") {
					t.Fatalf("%s: approved skill selection changed", role)
				}
				if readActive(t, wt, "guidance/skills/shared.md") != approved || activeExists(t, wt, "ARCHITECTURE.md") {
					t.Fatalf("%s: approved snapshots differ from the base", role)
				}
			}
		})
	}
}

func TestBundleGuidanceBaseTransition(t *testing.T) {
	t.Parallel()
	large := "# Architecture A\n\n## Coordination A\n\narchitecture-a-marker\n" + strings.Repeat("a", 2*architectureInlineLimit)
	repo, baseA := guidanceRepo(t, map[string]string{
		".swarm/architecture.md":       large,
		".swarm/skills/x.md":           "# Skill X\n\nskill-x-marker\n",
		".swarm/skills/nested/old.md":  "# Old\n\nold-marker\n",
		".swarm/skills/nested/kept.md": "# Kept\n\nkept-a-marker\n",
	})
	sys, _ := compileGuidance(t, repo, baseA, "developer", guidanceWorkUnit("a"), nil)
	if !activeExists(t, repo, "guidance/skills/x.md") || !activeExists(t, repo, "guidance/skills/nested/old.md") || readActive(t, repo, "ARCHITECTURE.md") != large {
		t.Fatal("base A snapshots missing")
	}
	if strings.Contains(sys, "architecture-a-marker") || !strings.Contains(sys, "- Coordination A") {
		t.Fatalf("large architecture should be referenced by section:\n%s", sys)
	}

	baseB := commitGuidance(t, repo, map[string]string{
		".swarm/architecture.md":       "# Architecture B\n\narchitecture-b-marker\n",
		".swarm/skills/x.md":           "",
		".swarm/skills/nested/old.md":  "",
		".swarm/skills/nested/new.md":  "# New\n\nnew-marker\n",
		".swarm/skills/nested/kept.md": "# Kept\n\nkept-b-marker\n",
	})
	var first string
	for i := 0; i < 2; i++ {
		sys, _ = compileGuidance(t, repo, baseB, "developer", guidanceWorkUnit("b"), nil)
		if activeExists(t, repo, "guidance/skills/x.md") || activeExists(t, repo, "guidance/skills/nested/old.md") || activeExists(t, repo, "ARCHITECTURE.md") {
			t.Fatal("base A guidance survived compilation at base B")
		}
		if strings.Contains(sys, "`.swarm/skills/x.md`") || strings.Contains(sys, "nested/old.md") || !strings.Contains(sys, "`.swarm/skills/nested/new.md`") {
			t.Fatalf("index still reflects base A:\n%s", sys)
		}
		if !strings.Contains(sys, "architecture-b-marker") || strings.Contains(sys, "ARCHITECTURE.md") {
			t.Fatal("small base B architecture should be inline")
		}
		if readActive(t, repo, "guidance/skills/nested/kept.md") != "# Kept\n\nkept-b-marker\n" {
			t.Fatal("kept skill copy not refreshed")
		}
		if i == 0 {
			first = sys
		} else if sys != first {
			t.Fatal("repeated compilation is not deterministic")
		}
	}

	baseC := commitGuidance(t, repo, map[string]string{
		".swarm/skills/nested/new.md":      "",
		".swarm/skills/nested/kept.md":     "",
		".swarm/skills/triage-and-plan.md": "",
		".swarm/rules/default.md":          "",
		".swarm/rules/nested/sec.md":       "",
		".swarm/rules/lessons.md":          "",
	})
	sys, _ = compileGuidance(t, repo, baseC, "developer", guidanceWorkUnit("c"), nil)
	if activeExists(t, repo, "guidance") || strings.Contains(sys, "## Skill index") || strings.Contains(sys, "rules-policy-marker") {
		t.Fatalf("base without skills or rules kept old guidance:\n%s", sys)
	}
}

func TestBundleGuidanceCleanupFailsClosed(t *testing.T) {
	t.Parallel()
	for _, link := range []string{"guidance", "guidance/skills/x.md"} {
		t.Run(link, func(t *testing.T) {
			t.Parallel()
			repo, base := guidanceRepo(t, map[string]string{".swarm/skills/x.md": "# X\n"})
			compileGuidance(t, repo, base, "developer", guidanceWorkUnit("first"), nil)
			before := readActive(t, repo, "SYSTEM_PROMPT.md")
			outside := t.TempDir()
			checkpointWrite(t, outside, "x.md", "outside")
			target := filepath.Join(repo, ".swarm/active", filepath.FromSlash(link))
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
			linkTo := outside
			if link != "guidance" {
				linkTo = filepath.Join(outside, "x.md")
			}
			if err := os.Symlink(linkTo, target); err != nil {
				t.Skip(err)
			}
			if err := CompileHarnessBundleAt(repo, guidanceWorkUnit("second"), &contract.WorkflowSpec{Name: "feature"}, nil, "developer", base); err == nil {
				t.Fatal("symlinked guidance accepted")
			}
			if readActive(t, repo, "SYSTEM_PROMPT.md") != before {
				t.Fatal("prompt published after cleanup failed")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Fatalf("outside directory changed: %v %v", entries, err)
			}
			if data, err := os.ReadFile(filepath.Join(outside, "x.md")); err != nil || string(data) != "outside" {
				t.Fatal("outside file changed")
			}
		})
	}
}

func TestBundleWorkUnitAndOrdering(t *testing.T) {
	t.Parallel()
	repo, base := guidanceRepo(t, map[string]string{".swarm/skills/all.md": "---\nroles: [\"*\"]\n---\n# Everyone\n\nall-body-marker\n"})
	taskMarkers := []string{"order-title", "order-objective", "order-constraint", "order-acceptance"}
	review := []contract.Artifact{{Type: contract.ArtifactReview, Verdict: "CHANGES_REQUESTED", Producer: "mock", Body: "review-artifact-marker"}}
	for _, role := range guidanceRoles {
		for _, pr := range []int{0, 41} {
			t.Run(fmt.Sprintf("%s/pr=%d", role, pr), func(t *testing.T) {
				wu := guidanceWorkUnit("order")
				var arts []contract.Artifact
				if role == "planner" {
					arts = review
				}
				var err error
				if pr > 0 {
					err = CompilePRHarnessBundleAt(repo, wu, &contract.WorkflowSpec{Name: "feature"}, arts, role, base, pr, &RevisionPlanContext{})
				} else {
					err = CompileHarnessBundleAt(repo, wu, &contract.WorkflowSpec{Name: "feature"}, arts, role, base)
				}
				if err != nil {
					t.Fatal(err)
				}
				sys, workunit := readActive(t, repo, "SYSTEM_PROMPT.md"), readActive(t, repo, "WORKUNIT.md")
				for _, marker := range append(taskMarkers, "review-artifact-marker") {
					if strings.Contains(sys, marker) {
						t.Fatalf("system prompt repeats WorkUnit text %q", marker)
					}
					if marker != "review-artifact-marker" && strings.Count(workunit, marker) != 1 {
						t.Fatalf("WORKUNIT.md should contain %q once", marker)
					}
				}
				rolePrompt, err := harness.LoadRolePromptAt(repo, role, base)
				if err != nil {
					t.Fatal(err)
				}
				order := []string{"project-policy-marker", "conventions-policy-marker", "goals-policy-marker", "manual-policy-marker", "rules-policy-marker", "## Approved validation policy", strings.SplitN(rolePrompt, "\n", 2)[0]}
				if role == "planner" {
					order = append(order, "## Claim budget")
				}
				order = append(order, "architecture-small-marker", "## Skill index", "all-body-marker")
				if role == "planner" {
					order = append(order, "Address the previous CHANGES_REQUESTED")
				}
				order = append(order, "\n---\n\n# Task\n", "Your task is Work Unit ISSUE-9 (#9). Read `.swarm/active/WORKUNIT.md`")
				if pr > 0 {
					order = append(order, "\n## PR revision\nRead ORIGINAL_PLAN.md")
				}
				last := -1
				for _, marker := range order {
					at := strings.Index(sys, marker)
					if at <= last {
						t.Fatalf("%q at %d is out of order (previous section at %d):\n%s", marker, at, last, sys)
					}
					last = at
				}
			})
		}
	}
	t.Run("stable prefix", func(t *testing.T) {
		prefix := func(wu *contract.WorkUnit, arts []contract.Artifact) string {
			sys, _ := compileGuidance(t, repo, base, "developer", wu, arts)
			return sys[:strings.LastIndex(sys, "\n---\n")]
		}
		other := &contract.WorkUnit{ID: "ISSUE-10", IssueNumber: 10, Title: "Other", Objective: "Different task", Workflow: "feature"}
		if prefix(guidanceWorkUnit("one"), nil) != prefix(other, []contract.Artifact{{Type: contract.ArtifactDesign, Body: "plan"}}) {
			t.Fatal("task inputs changed the stable policy prefix")
		}
	})
}
