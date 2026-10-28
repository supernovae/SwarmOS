package contract

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
	"gopkg.in/yaml.v3"
)

var (
	htmlCommentRegex     = regexp.MustCompile(`(?s)<!--.*?-->`)
	fencedYAMLBlockRegex = regexp.MustCompile("(?s)```yaml(?:\\s+swarm-workunit)?\\s*\\n(.*?)\\n```")
	externalPathRegex    = regexp.MustCompile(`(?m)^swarm-workunit-path:\s*(\S+)\s*$`)
	issueRefRegex        = regexp.MustCompile(`#?(\d+)`)
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
			data, readErr := safefs.ReadFile(repoRoot, cleanPath, 1<<20)
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
	if wu.IssueNumber != 0 && issueNum > 0 && wu.IssueNumber != issueNum {
		return nil, fmt.Errorf("work unit issue_number %d does not match selected issue #%d", wu.IssueNumber, issueNum)
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
	if err := ValidateGate(wu.Gate); err != nil {
		return nil, err
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
		Objective:            sections["objective"],
		Constraints:          parseListItems(sections["constraints"]),
		Acceptance:           parseListItems(firstNonEmpty(sections["acceptance criteria"], sections["acceptance"])),
		Workflow:             strings.ToLower(strings.TrimSpace(sections["workflow"])),
		RequiredCapabilities: parseListItems(firstNonEmpty(sections["required capabilities"], sections["capabilities"])),
		DependsOn:            parseIssueNumbers(firstNonEmpty(sections["depends on"], sections["dependencies"])),
	}

	if parentRaw := strings.TrimSpace(sections["parent issue"]); parentRaw != "" {
		if nums := parseIssueNumbers(parentRaw); len(nums) > 0 {
			wu.ParentIssue = nums[0]
		}
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

	wu.Size = strings.ToLower(strings.TrimSpace(sections["size"]))
	if wu.Size == "_no response_" {
		wu.Size = ""
	}
	wu.Uncertainty = strings.ToLower(strings.TrimSpace(sections["uncertainty"]))
	if wu.Uncertainty == "_no response_" {
		wu.Uncertainty = ""
	}
	wu.Risk = strings.ToLower(strings.TrimSpace(sections["risk"]))
	if wu.Risk == "_no response_" {
		wu.Risk = ""
	}
	return wu, true
}

func parseIssueNumbers(raw string) []int {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "_no response_") || strings.EqualFold(raw, "none") {
		return nil
	}
	var out []int
	seen := make(map[int]bool)
	for _, m := range issueRefRegex.FindAllStringSubmatch(raw, -1) {
		if len(m) > 1 {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
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

	if wu.ParentIssue > 0 {
		sb.WriteString(fmt.Sprintf("\n### Parent Issue\n\n#%d\n", wu.ParentIssue))
	}
	if len(wu.DependsOn) > 0 {
		sb.WriteString("\n### Depends On\n\n")
		for _, dep := range wu.DependsOn {
			sb.WriteString(fmt.Sprintf("- #%d\n", dep))
		}
	}
	if len(wu.RequiredCapabilities) > 0 {
		sb.WriteString("\n### Required Capabilities\n\n")
		for _, capName := range wu.RequiredCapabilities {
			sb.WriteString("- " + capName + "\n")
		}
	}
	// Omitted when false so bodies, and the project-plan hashes over them,
	// stay unchanged for WorkUnits that never opt in.
	if wu.AllowHarnessChanges {
		sb.WriteString("\n### Allow Harness Changes\n\n" + allowHarnessChangesChecked + "\n")
	}
	if wu.Size != "" {
		sb.WriteString("\n### Size\n\n" + wu.Size + "\n")
	}
	if wu.Uncertainty != "" {
		sb.WriteString("\n### Uncertainty\n\n" + wu.Uncertainty + "\n")
	}
	if wu.Risk != "" {
		sb.WriteString("\n### Risk\n\n" + wu.Risk + "\n")
	}
	return sb.String()
}

const (
	allowHarnessChangesChecked   = "- [x] Allow edits to protected harness paths"
	allowHarnessChangesUnchecked = "- [ ] Allow edits to protected harness paths"
)
