package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	htmlCommentRegex     = regexp.MustCompile(`(?s)<!--.*?-->`)
	fencedYAMLBlockRegex = regexp.MustCompile("(?s)```yaml(?:\\s+swarm-workunit)?\\s*\\n(.*?)\\n```")
	externalPathRegex    = regexp.MustCompile(`(?m)^swarm-workunit-path:\s*(\S+)\s*$`)
)

// StripHTMLComments removes all <!-- ... --> blocks from untrusted markdown input.
func StripHTMLComments(s string) string {
	return htmlCommentRegex.ReplaceAllString(s, "")
}

// ParseWorkUnitFromIssue parses a WorkUnit from an issue body using 3 deterministic formats in priority order:
// 1. Fenced YAML block tagged ```yaml swarm-workunit or ```yaml
// 2. GitHub Issue Form markdown sections (### Objective, ### Constraints, ### Acceptance Criteria, ### Workflow, ### Plan Gate)
// 3. External file reference swarm-workunit-path: .swarm/workunits/<id>.yaml
func ParseWorkUnitFromIssue(issueNum int, title, body string) (*WorkUnit, error) {
	return ParseWorkUnitFromIssueWithRoot(issueNum, title, body, ".")
}

func ParseWorkUnitFromIssueWithRoot(issueNum int, title, body, repoRoot string) (*WorkUnit, error) {
	cleanBody := StripHTMLComments(body)

	var wu *WorkUnit
	var err error

	// 1. Check fenced YAML block
	if matches := fencedYAMLBlockRegex.FindStringSubmatch(cleanBody); len(matches) > 1 {
		yamlContent := matches[1]
		var candidate WorkUnit
		if err := yaml.Unmarshal([]byte(yamlContent), &candidate); err == nil && (candidate.Objective != "" || candidate.Workflow != "" || len(candidate.Acceptance) > 0) {
			wu = &candidate
		} else if err != nil {
			return nil, fmt.Errorf("invalid fenced YAML workunit in issue #%d: %w", issueNum, err)
		}
	}

	// 2. Check GitHub Issue Form markdown sections
	if wu == nil && strings.Contains(cleanBody, "### ") {
		parsed, ok := parseMarkdownSections(cleanBody)
		if ok {
			wu = parsed
		}
	}

	// 3. Check external file reference
	if wu == nil {
		if matches := externalPathRegex.FindStringSubmatch(cleanBody); len(matches) > 1 {
			relPath := strings.TrimSpace(matches[1])
			cleanPath := filepath.Clean(relPath)
			if strings.HasPrefix(cleanPath, "..") || filepath.IsAbs(cleanPath) {
				return nil, fmt.Errorf("invalid external workunit path %q: must be relative to repo root", relPath)
			}
			fullPath := filepath.Join(repoRoot, cleanPath)
			data, readErr := os.ReadFile(fullPath)
			if readErr != nil {
				return nil, fmt.Errorf("failed to read external workunit file %q: %w", relPath, readErr)
			}
			var candidate WorkUnit
			if yamlErr := yaml.Unmarshal(data, &candidate); yamlErr != nil {
				return nil, fmt.Errorf("failed to parse external workunit YAML %q: %w", relPath, yamlErr)
			}
			wu = &candidate
		}
	}

	if wu == nil {
		return nil, fmt.Errorf("issue #%d does not contain a valid SwarmOS WorkUnit (expected fenced YAML, ### Objective markdown sections, or swarm-workunit-path)", issueNum)
	}

	if wu.IssueNumber == 0 && issueNum > 0 {
		wu.IssueNumber = issueNum
	}
	if wu.ID == "" {
		if issueNum > 0 {
			wu.ID = fmt.Sprintf("ISSUE-%d", issueNum)
		} else {
			wu.ID = "WORKUNIT-LOCAL"
		}
	}
	if wu.Title == "" && title != "" {
		wu.Title = strings.TrimSpace(title)
	}
	if wu.Gate == "" {
		wu.Gate = GateNone
	}

	if err = ValidateWorkUnit(wu); err != nil {
		return nil, err
	}
	return wu, nil
}

func parseMarkdownSections(cleanBody string) (*WorkUnit, bool) {
	sections := make(map[string]string)
	lines := strings.Split(strings.ReplaceAll(cleanBody, "\r\n", "\n"), "\n")

	var currentHeader string
	var buf []string

	flush := func() {
		if currentHeader != "" {
			sections[currentHeader] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
		buf = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "### ") {
			flush()
			currentHeader = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "### ")))
			continue
		}
		if currentHeader != "" {
			buf = append(buf, line)
		}
	}
	flush()

	if len(sections) == 0 {
		return nil, false
	}

	wu := &WorkUnit{
		Objective:   sections["objective"],
		Constraints: parseListItems(sections["constraints"]),
		Acceptance:  parseListItems(firstNonEmpty(sections["acceptance criteria"], sections["acceptance"])),
		Workflow:    strings.ToLower(strings.TrimSpace(sections["workflow"])),
	}

	gateRaw := strings.ToLower(strings.TrimSpace(firstNonEmpty(sections["plan gate"], sections["plan review gate"], sections["gate"])))
	if gateRaw != "" && gateRaw != "_no response_" {
		wu.Gate = GateMode(gateRaw)
	}

	if allowRaw, ok := sections["allow harness changes"]; ok {
		lower := strings.ToLower(allowRaw)
		if strings.Contains(lower, "[x]") || lower == "true" || lower == "yes" {
			wu.AllowHarnessChanges = true
		}
	}

	return wu, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func parseListItems(section string) []string {
	section = strings.TrimSpace(section)
	if section == "" || section == "_No response_" {
		return nil
	}
	var items []string
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, prefix := range []string{"- [ ] ", "- [x] ", "- [X] ", "* [ ] ", "* [x] ", "- ", "* "} {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				break
			}
		}
		if line != "" {
			items = append(items, line)
		}
	}
	return items
}

func FormatIssueMarkdown(wu *WorkUnit) string {
	var sb strings.Builder
	sb.WriteString("### Objective\n\n")
	sb.WriteString(strings.TrimSpace(wu.Objective) + "\n\n")

	sb.WriteString("### Constraints\n\n")
	for _, c := range wu.Constraints {
		sb.WriteString("- " + c + "\n")
	}
	sb.WriteString("\n")

	sb.WriteString("### Acceptance Criteria\n\n")
	for _, a := range wu.Acceptance {
		sb.WriteString("- [ ] " + a + "\n")
	}
	sb.WriteString("\n")

	sb.WriteString("### Workflow\n\n")
	sb.WriteString(wu.Workflow + "\n\n")

	gate := wu.Gate
	if gate == "" {
		gate = GateNone
	}
	sb.WriteString("### Plan Gate\n\n")
	sb.WriteString(string(gate) + "\n")
	return sb.String()
}
