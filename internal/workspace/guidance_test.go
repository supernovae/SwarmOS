package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/safefs"
)

func TestSkillSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, content, role, workflow string
		want                          bool
	}{
		{"unannotated is index-only", "# Skill\nbody", "developer", "feature", false},
		{"empty front matter is index-only", "---\n---\n# Skill\nbody", "developer", "feature", false},
		{"description only is index-only", "---\ndescription: Helps\n---\nbody", "developer", "feature", false},
		{"role match", "---\nroles: [developer]\n---\nbody", "developer", "feature", true},
		{"role mismatch", "---\nroles: [developer]\n---\nbody", "tester", "feature", false},
		{"workflow only matches any role", "---\nworkflows: [feature]\n---\nbody", "reviewer", "feature", true},
		{"workflow mismatch", "---\nworkflows: [bugfix]\n---\nbody", "developer", "feature", false},
		{"role and workflow both match", "---\nroles: [developer, tester]\nworkflows: [feature]\n---\nbody", "tester", "feature", true},
		{"role matches but workflow does not", "---\nroles: [developer]\nworkflows: [bugfix]\n---\nbody", "developer", "feature", false},
		{"workflow matches but role does not", "---\nroles: [planner]\nworkflows: [feature]\n---\nbody", "developer", "feature", false},
		{"wildcard role", "---\nroles: [\"*\"]\n---\nbody", "reviewer", "refactor", true},
		{"wildcard role with workflow", "---\nroles: [\"*\"]\nworkflows: [bugfix]\n---\nbody", "reviewer", "feature", false},
		{"empty workflow name", "---\nroles: [developer]\n---\nbody", "developer", "", true},
		{"CRLF front matter", "---\r\nroles: [developer]\r\n---\r\nbody", "developer", "feature", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			skill, err := parseGuidanceSkill(".swarm/skills/x.md", tc.content)
			if err != nil {
				t.Fatal(err)
			}
			if got := skill.selected(tc.role, tc.workflow); got != tc.want {
				t.Fatalf("selected(%q, %q) = %v, want %v", tc.role, tc.workflow, got, tc.want)
			}
			if strings.Contains(skill.body, "roles:") || strings.HasPrefix(skill.body, "---") {
				t.Fatalf("front matter left in body: %q", skill.body)
			}
			if skill.raw != tc.content {
				t.Fatal("approved copy must keep the original content")
			}
		})
	}
}

func TestSkillSelectionRejectsMalformedMetadata(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, content, field string }{
		{"roles scalar", "---\nroles: developer\n---\n", "roles"},
		{"roles empty", "---\nroles: []\n---\n", "roles"},
		{"roles number", "---\nroles: [1]\n---\n", "roles"},
		{"roles nested list", "---\nroles: [[developer]]\n---\n", "roles"},
		{"roles blank name", "---\nroles: [\"\"]\n---\n", "roles"},
		{"workflows map", "---\nworkflows: {feature: true}\n---\n", "workflows"},
		{"workflows spaced name", "---\nworkflows: [\"new feature\"]\n---\n", "workflows"},
		{"description list", "---\ndescription: [a]\n---\n", "description"},
		{"unclosed", "---\nroles: [developer]\n# Skill\n", "front matter"},
		{"invalid yaml", "---\nroles: [developer\n---\n", "front matter"},
		{"not a mapping", "---\n- developer\n---\n", "front matter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseGuidanceSkill(".swarm/skills/nested/bad.md", tc.content)
			if err == nil {
				t.Fatal("malformed metadata accepted")
			}
			if !strings.Contains(err.Error(), ".swarm/skills/nested/bad.md") || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error must name the file and %q: %v", tc.field, err)
			}
		})
	}
}

func TestSkillTitlesAndIndex(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("word ", 100)
	var skills []guidanceSkill
	for _, tc := range []struct{ path, content string }{
		{".swarm/skills/a.md", "# Alpha skill\n\nalpha-body"},
		{".swarm/skills/empty.md", ""},
		{".swarm/skills/nested/deep/b.md", "---\nroles: [developer]\ndescription: |\n  " + long + "\n---\n# " + long + "\n\nbeta-body"},
		{".swarm/skills/untitled.md", "no heading\nuntitled-body"},
	} {
		skill, err := parseGuidanceSkill(tc.path, tc.content)
		if err != nil {
			t.Fatal(err)
		}
		skills = append(skills, skill)
	}
	if skills[0].title != "Alpha skill" || skills[1].title != "empty.md" || skills[3].title != "untitled.md" {
		t.Fatalf("titles: %q %q %q", skills[0].title, skills[1].title, skills[3].title)
	}
	if got := skills[2].copyPath(); got != "guidance/skills/nested/deep/b.md" {
		t.Fatalf("copy path = %q", got)
	}
	text := renderSkillGuidance(skills, "developer", "feature")
	if text != renderSkillGuidance(skills, "developer", "feature") {
		t.Fatal("index is not deterministic")
	}
	for _, s := range skills {
		if strings.Count(text, "`"+s.path+"`") != 1 {
			t.Fatalf("index must name %s once:\n%s", s.path, text)
		}
	}
	if !strings.Contains(text, "beta-body") || strings.Contains(text, "alpha-body") || strings.Contains(text, "untitled-body") {
		t.Fatalf("only the selected skill body belongs inline:\n%s", text)
	}
	if !strings.Contains(text, "`.swarm/active/guidance/skills/a.md`") {
		t.Fatalf("index must point at the approved copy:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "- `.swarm/skills/nested") && len([]rune(line)) > 2*guidanceSummaryLimit+80 {
			t.Fatalf("index entry is unbounded (%d runes)", len([]rune(line)))
		}
	}
	if renderSkillGuidance(nil, "developer", "feature") != "" {
		t.Fatal("no skills should render nothing")
	}
}

func TestArchitectureGuidanceThreshold(t *testing.T) {
	t.Parallel()
	body := func(n int) string {
		doc := "# Architecture\n\n## Coordination\n"
		return doc + strings.Repeat("x", n-len(doc))
	}
	for _, tc := range []struct {
		name     string
		doc      string
		snapshot bool
	}{
		{"empty", "", false},
		{"below", body(architectureInlineLimit - 1), false},
		{"at", body(architectureInlineLimit), false},
		{"above", body(architectureInlineLimit + 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text, snapshot := renderArchitecture(tc.doc)
			if snapshot != tc.snapshot {
				t.Fatalf("snapshot = %v, want %v", snapshot, tc.snapshot)
			}
			switch {
			case tc.doc == "":
				if text != "" {
					t.Fatalf("empty architecture rendered %q", text)
				}
			case snapshot:
				if strings.Contains(text, "xxxx") || !strings.Contains(text, "`.swarm/active/ARCHITECTURE.md`") || !strings.Contains(text, "- Architecture\n  - Coordination\n") {
					t.Fatalf("large architecture should be indexed:\n%s", text)
				}
			default:
				if !strings.Contains(text, tc.doc) {
					t.Fatal("small architecture should be inline")
				}
			}
		})
	}
	var many strings.Builder
	many.WriteString("```\n# not a heading\n```\n")
	for i := 0; i < architectureMaxSections+5; i++ {
		many.WriteString("## Section " + strings.Repeat("y", 300) + "\n" + strings.Repeat("z", 200) + "\n")
	}
	text, snapshot := renderArchitecture(many.String())
	if !snapshot || strings.Contains(text, "not a heading") || !strings.Contains(text, "and 5 more sections") {
		t.Fatalf("section index should skip fences and stay bounded:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if len([]rune(line)) > guidanceSummaryLimit+8 && strings.HasPrefix(line, "- ") {
			t.Fatalf("section entry is unbounded: %d runes", len([]rune(line)))
		}
	}
}

func TestRemoveManagedTree(t *testing.T) {
	t.Parallel()
	open := func(t *testing.T, dir string) *safefs.Root {
		t.Helper()
		root, err := safefs.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { root.Close() })
		return root
	}
	t.Run("nested", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"guidance/skills/a.md", "guidance/skills/x/y/z.md", "keep.md"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			checkpointWrite(t, dir, name, "content")
		}
		if err := os.MkdirAll(filepath.Join(dir, "guidance/empty"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := removeManagedTree(open(t, dir), "guidance", 0); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "guidance")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("guidance survived: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "keep.md")); err != nil {
			t.Fatal("unmanaged file removed")
		}
	})
	t.Run("missing", func(t *testing.T) {
		if err := removeManagedTree(open(t, t.TempDir()), "guidance", 0); err != nil {
			t.Fatal(err)
		}
	})
	for _, link := range []string{"guidance", "guidance/skills/link"} {
		t.Run("refuses symlink "+link, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			checkpointWrite(t, outside, "secret.md", "outside")
			if err := os.MkdirAll(filepath.Join(dir, "guidance/skills"), 0o755); err != nil {
				t.Fatal(err)
			}
			if link == "guidance" {
				if err := os.RemoveAll(filepath.Join(dir, "guidance")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(dir, link)); err != nil {
				t.Skip(err)
			}
			if err := removeManagedTree(open(t, dir), "guidance", 0); err == nil {
				t.Fatal("symlink accepted")
			}
			if data, err := os.ReadFile(filepath.Join(outside, "secret.md")); err != nil || string(data) != "outside" {
				t.Fatal("cleanup reached outside the managed directory")
			}
		})
	}
}
