package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
	"gopkg.in/yaml.v3"
)

const (
	// architectureInlineLimit is the largest approved architecture document
	// inlined in a role prompt; larger documents are referenced by section.
	architectureInlineLimit = 4096
	architectureMaxSections = 40
	guidanceSummaryLimit    = 160
	guidanceDir             = "guidance"
	architectureSnapshot    = "ARCHITECTURE.md"
	guidanceMaxDepth        = 32
)

// bundleGuidance is the approved-base guidance a role prompt is composed from.
// Policy documents are always inlined; skills are indexed and inlined only
// when their front matter selects the active role and workflow.
type bundleGuidance struct {
	policy []string
	skills []guidanceSkill
}

type guidanceSkill struct {
	path        string // repository path in the approved base
	raw         string // approved content, copied verbatim for on-demand reading
	body        string // content without front matter
	title       string
	description string
	roles       []string
	workflows   []string
}

// copyPath is where the approved copy of a skill is written under .swarm/active.
func (s guidanceSkill) copyPath() string {
	return path.Join(guidanceDir, "skills", strings.TrimPrefix(s.path, ".swarm/skills/"))
}

// selected reports whether the skill is inlined for role and workflow. Each
// selector that is present must match; a skill with no selector is index-only.
func (s guidanceSkill) selected(role, workflow string) bool {
	if s.roles == nil && s.workflows == nil {
		return false
	}
	return selectorMatches(s.roles, role) && selectorMatches(s.workflows, workflow)
}

func selectorMatches(values []string, want string) bool {
	if values == nil {
		return true
	}
	for _, v := range values {
		if v == "*" || v == want {
			return true
		}
	}
	return false
}

// parseGuidanceSkill reads optional YAML front matter (roles, workflows,
// description) and rejects malformed selectors with the file and field named.
func parseGuidanceSkill(repoPath, content string) (guidanceSkill, error) {
	skill := guidanceSkill{path: repoPath, raw: content, body: content}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if strings.TrimRight(lines[0], " \t") == "---" {
		end := 0
		for i := 1; i < len(lines); i++ {
			if strings.TrimRight(lines[i], " \t") == "---" {
				end = i
				break
			}
		}
		if end == 0 {
			return skill, fmt.Errorf("skill %s: front matter is not closed with ---", repoPath)
		}
		header := strings.Join(lines[1:end], "\n")
		skill.body = strings.Join(lines[end+1:], "\n")
		var fields map[string]yaml.Node
		if err := yaml.Unmarshal([]byte(header), &fields); err != nil {
			return skill, fmt.Errorf("skill %s: invalid front matter: %w", repoPath, err)
		}
		var err error
		if node, ok := fields["roles"]; ok {
			if skill.roles, err = selectorValues(repoPath, "roles", node); err != nil {
				return skill, err
			}
		}
		if node, ok := fields["workflows"]; ok {
			if skill.workflows, err = selectorValues(repoPath, "workflows", node); err != nil {
				return skill, err
			}
		}
		if node, ok := fields["description"]; ok {
			if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
				return skill, fmt.Errorf("skill %s: field description must be a string", repoPath)
			}
			skill.description = node.Value
		}
	}
	skill.body = strings.TrimSpace(skill.body)
	skill.title = path.Base(repoPath)
	for _, line := range strings.Split(skill.body, "\n") {
		if heading, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok && strings.TrimSpace(heading) != "" {
			skill.title = heading
			break
		}
	}
	return skill, nil
}

func selectorValues(repoPath, field string, node yaml.Node) ([]string, error) {
	if node.Kind != yaml.SequenceNode || len(node.Content) == 0 {
		return nil, fmt.Errorf("skill %s: field %s must be a non-empty list of names", repoPath, field)
	}
	values := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		value := strings.TrimSpace(item.Value)
		if item.Kind != yaml.ScalarNode || item.Tag != "!!str" || value == "" || strings.ContainsAny(value, " \t\n") {
			return nil, fmt.Errorf("skill %s: field %s must list names as strings", repoPath, field)
		}
		values = append(values, value)
	}
	return values, nil
}

// boundedLine collapses whitespace and truncates s for a one-line index entry.
func boundedLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return s
}

// renderSkillGuidance lists every skill and inlines the selected bodies.
func renderSkillGuidance(skills []guidanceSkill, role, workflow string) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Skill index\n\n")
	sb.WriteString("These skills come from the approved base. Skills selected for this role and workflow are inlined below; read any other skill from its approved copy under `.swarm/active/` when your task needs it.\n\n")
	var selected []guidanceSkill
	for _, s := range skills {
		entry := "- `" + s.path + "`: " + boundedLine(s.title, guidanceSummaryLimit)
		if s.description != "" {
			entry += " — " + boundedLine(s.description, guidanceSummaryLimit)
		}
		if s.selected(role, workflow) {
			entry += " (inlined below)"
			if s.body != "" {
				selected = append(selected, s)
			}
		} else {
			entry += " (approved copy: `.swarm/active/" + s.copyPath() + "`)"
		}
		sb.WriteString(entry + "\n")
	}
	sb.WriteString("\n")
	for _, s := range selected {
		sb.WriteString(s.body + "\n\n")
	}
	return sb.String()
}

// renderArchitecture inlines a small document, or indexes the sections of a
// large one that is written to .swarm/active/ARCHITECTURE.md.
func renderArchitecture(doc string) (text string, snapshot bool) {
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return "", false
	}
	if len(doc) <= architectureInlineLimit {
		return doc + "\n\n", false
	}
	var headings []string
	fenced := false
	for _, line := range strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || !strings.HasPrefix(trimmed, "#") {
			continue
		}
		level := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
		title := strings.TrimSpace(trimmed[level:])
		if level > 6 || title == "" || trimmed[level] != ' ' {
			continue
		}
		headings = append(headings, strings.Repeat("  ", min(level, 3)-1)+"- "+boundedLine(title, guidanceSummaryLimit))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Architecture\n\nThe approved architecture document is %d bytes, so it is not inlined. Read the sections relevant to your task from `.swarm/active/%s`, a copy of the approved base.\n\n", len(doc), architectureSnapshot)
	for i, h := range headings {
		if i == architectureMaxSections {
			fmt.Fprintf(&sb, "- … and %d more sections\n", len(headings)-i)
			break
		}
		sb.WriteString(h + "\n")
	}
	sb.WriteString("\n")
	return sb.String(), true
}

// writeApprovedGuidance rebuilds the managed approved copies under active:
// guidance/ is always removed and recreated, and ARCHITECTURE.md exists only
// when the architecture document is referenced rather than inlined.
func writeApprovedGuidance(active *safefs.Root, skills []guidanceSkill, archDoc string, archSnapshot bool) error {
	if err := removeManagedTree(active, guidanceDir, 0); err != nil {
		return fmt.Errorf("clean approved guidance copies: %w", err)
	}
	if archSnapshot {
		if err := active.WriteFile(architectureSnapshot, []byte(archDoc), 0o644); err != nil {
			return err
		}
	} else if err := active.Remove(architectureSnapshot); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale architecture snapshot: %w", err)
	}
	for _, s := range skills {
		if err := active.WriteFile(filepath.FromSlash(s.copyPath()), []byte(s.raw), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// removeManagedTree deletes name and its contents inside root. Every step is
// resolved through root, so a link can never redirect deletion; links and
// special files are refused rather than removed.
func removeManagedTree(root *safefs.Root, name string, depth int) error {
	if depth > guidanceMaxDepth {
		return fmt.Errorf("managed directory %q is nested too deeply", name)
	}
	dir, err := root.OpenDirectory(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var entries []os.DirEntry
	for {
		batch, readErr := dir.ReadDir(128)
		entries = append(entries, batch...)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			dir.Close()
			return readErr
		}
	}
	if err := dir.Close(); err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		child := filepath.Join(name, entry.Name())
		switch {
		case entry.IsDir():
			if err := removeManagedTree(root, child, depth+1); err != nil {
				return err
			}
		case entry.Type().IsRegular():
			if err := root.Remove(child); err != nil {
				return err
			}
		default:
			return fmt.Errorf("refuse to remove non-regular managed entry %q", filepath.ToSlash(child))
		}
	}
	return root.Remove(name)
}
