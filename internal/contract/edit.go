package contract

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkUnitEdit distinguishes omitted fields from explicitly empty replacements.
type WorkUnitEdit struct {
	Title, Objective, Workflow, Gate *string
	Constraints, Acceptance          *[]string
	DependsOn                        *[]int
	AllowHarnessChanges              *bool
}

type editSpan struct {
	start, end int
	text       string
}

// EditWorkUnitIssue replaces selected source spans and validates the resulting contract.
func EditWorkUnitIssue(number int, title, body string, edit WorkUnitEdit) (string, string, error) {
	for _, comment := range htmlCommentRegex.FindAllString(body, -1) {
		if strings.Contains(comment, "```yaml") {
			return title, body, fmt.Errorf("ambiguous YAML fence inside HTML comment")
		}
	}
	newTitle := title
	if edit.Title != nil {
		newTitle = *edit.Title
	}
	fields := map[string]any{}
	if edit.Objective != nil {
		fields["objective"] = *edit.Objective
	}
	if edit.Workflow != nil {
		fields["workflow"] = *edit.Workflow
	}
	if edit.Gate != nil {
		fields["gate"] = *edit.Gate
	}
	if edit.Constraints != nil {
		fields["constraints"] = *edit.Constraints
	}
	if edit.Acceptance != nil {
		fields["acceptance"] = *edit.Acceptance
	}
	if edit.DependsOn != nil {
		fields["depends_on"] = *edit.DependsOn
	}
	if edit.AllowHarnessChanges != nil {
		fields["allow_harness_changes"] = *edit.AllowHarnessChanges
	}
	old, oldErr := editableWorkUnit(number, title, body)
	candidate := body
	var err error
	if loc := fencedYAMLBlockRegex.FindStringSubmatchIndex(body); loc != nil {
		candidate, err = editYAML(body, loc, fields, edit.Title)
	} else if externalPathRegex.MatchString(StripHTMLComments(body)) {
		return newTitle, body, fmt.Errorf("external-file WorkUnit: edit the referenced file instead")
	} else {
		candidate, err = editMarkdown(body, fields)
	}
	if err != nil {
		return newTitle, body, err
	}
	parsed, err := ParseWorkUnitFromIssue(number, newTitle, candidate)
	if err != nil {
		return newTitle, candidate, err
	}
	// Comparing the parsed result with the intended contract guards
	// against injected headings, comments, or YAML changing unselected semantics.
	if oldErr == nil {
		expected := *old
		if edit.Title != nil {
			expected.Title = strings.TrimSpace(*edit.Title)
		}
		if edit.Objective != nil {
			expected.Objective = *edit.Objective
		}
		if edit.Workflow != nil {
			expected.Workflow = *edit.Workflow
		}
		if edit.Gate != nil {
			expected.Gate = GateMode(*edit.Gate)
		}
		if edit.Constraints != nil {
			expected.Constraints = *edit.Constraints
			if len(expected.Constraints) == 0 && !fencedYAMLBlockRegex.MatchString(body) {
				expected.Constraints = nil
			}
		}
		if edit.Acceptance != nil {
			expected.Acceptance = *edit.Acceptance
		}
		if edit.DependsOn != nil {
			expected.DependsOn = *edit.DependsOn
		}
		if edit.AllowHarnessChanges != nil {
			expected.AllowHarnessChanges = *edit.AllowHarnessChanges
		}
		a := DigestWorkUnit(&expected)
		b := DigestWorkUnit(parsed)
		if a != b {
			return newTitle, candidate, fmt.Errorf("replacement changes fields beyond the requested values; use plain section content")
		}
	} else {
		return newTitle, candidate, oldErr
	}

	return newTitle, candidate, nil
}

func applyEditSpans(body string, spans []editSpan) string {
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	for _, s := range spans {
		body = body[:s.start] + s.text + body[s.end:]
	}
	return body
}

func sectionKey(header string) string {
	switch strings.ToLower(strings.TrimSpace(header)) {
	case "objective":
		return "objective"
	case "constraints":
		return "constraints"
	case "acceptance criteria", "acceptance":
		return "acceptance"
	case "workflow":
		return "workflow"
	case "plan gate", "plan review gate", "gate":
		return "gate"
	case "depends on", "dependencies":
		return "depends_on"
	case "allow harness changes":
		return "allow_harness_changes"
	}
	return ""
}

func editMarkdown(body string, fields map[string]any) (string, error) {
	if len(fields) == 0 {
		return body, nil
	}
	type section struct {
		key        string
		start, end int
	}
	var sections []section
	offset := 0
	// Comments containing headings make source mapping ambiguous: refuse rather than edit them.
	if StripHTMLComments(body) != body {
		for _, c := range htmlCommentRegex.FindAllString(body, -1) {
			if strings.Contains(c, "### ") {
				return body, fmt.Errorf("ambiguous heading inside HTML comment")
			}
		}
	}
	for _, line := range strings.SplitAfter(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "### ") {
			if len(sections) > 0 {
				sections[len(sections)-1].end = offset
			}
			sections = append(sections, section{sectionKey(strings.TrimPrefix(trimmed, "### ")), offset + len(line), len(body)})
		}
		offset += len(line)
	}
	newline := "\n"
	if strings.Contains(body, "\r\n") {
		newline = "\r\n"
	}
	names := map[string]string{"objective": "Objective", "constraints": "Constraints", "acceptance": "Acceptance Criteria", "workflow": "Workflow", "gate": "Plan Gate", "depends_on": "Depends On", "allow_harness_changes": "Allow Harness Changes"}
	var spans []editSpan
	var additions strings.Builder
	for _, key := range []string{"objective", "constraints", "acceptance", "workflow", "gate", "depends_on", "allow_harness_changes"} {
		value, ok := fields[key]
		if !ok {
			continue
		}
		fragment := &WorkUnit{}
		switch key {
		case "objective":
			fragment.Objective = value.(string)
		case "constraints":
			fragment.Constraints = value.([]string)
		case "acceptance":
			fragment.Acceptance = value.([]string)
		case "workflow":
			fragment.Workflow = value.(string)
		case "gate":
			fragment.Gate = GateMode(value.(string))
		case "depends_on":
			fragment.DependsOn = value.([]int)
		case "allow_harness_changes":
			fragment.AllowHarnessChanges = value.(bool)
		}
		rendered := FormatIssueMarkdown(fragment)
		content := ""
		header := "### " + names[key] + "\n"
		if at := strings.Index(rendered, header); at >= 0 {
			content = rendered[at+len(header):]
			if next := strings.Index(content, "### "); next >= 0 {
				content = content[:next]
			}
			content = strings.TrimSpace(content)
		}
		// The formatter defaults an empty gate to none; an explicit empty flag
		// means inherit policy, just as an omitted gate in a new WorkUnit does.
		if key == "gate" && value.(string) == "" {
			content = ""
		}
		// The formatter omits a false permission; an existing section needs an
		// unchecked box, while a missing one already parses as false.
		if key == "allow_harness_changes" && !value.(bool) {
			content = allowHarnessChangesUnchecked
		}
		content = strings.ReplaceAll(content, "\n", newline) + newline

		found := false
		for _, s := range sections {
			if s.key != key {
				continue
			}
			if found {
				return body, fmt.Errorf("ambiguous duplicate %s sections", key)
			}
			found = true
			// A final section has no following heading to separate from, so
			// rewriting it with its current value leaves the body unchanged.
			separator := newline
			if s.end == len(body) {
				separator = ""
			}
			spans = append(spans, editSpan{s.start, s.end, newline + content + separator})
		}
		if !found && (key != "allow_harness_changes" || value.(bool)) {
			additions.WriteString(newline + "### " + names[key] + newline + newline + content)
		}
	}
	return applyEditSpans(body, spans) + additions.String(), nil
}

func editYAML(body string, loc []int, fields map[string]any, title *string) (string, error) {
	source := body[loc[2]:loc[3]]
	// Parser strips comments first; refuse constructs whose source offsets differ.
	if StripHTMLComments(source) != source {
		return body, fmt.Errorf("HTML comments inside YAML are unsupported for editing")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(source), &doc); err != nil {
		return body, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode || doc.Content[0].Style == yaml.FlowStyle {
		return body, fmt.Errorf("WorkUnit YAML must be a block mapping")
	}
	mapping := doc.Content[0]
	if err := checkEditableYAML(mapping); err != nil {
		return body, err
	}
	if strings.Contains(source, "\n---") || strings.Contains(source, "\n...") {
		return body, fmt.Errorf("multiple YAML documents are unsupported for editing")
	}
	offsets := []int{0}
	for i, c := range source {
		if c == '\n' {
			offsets = append(offsets, i+1)
		}
	}
	position := func(line int) int {
		if line > len(offsets) {
			return len(source)
		}
		return offsets[line-1]
	}
	if title != nil {
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == "title" {
				fields["title"] = *title
			}
		}
	}
	seen := map[string]bool{}
	var spans []editSpan
	for i := 0; i < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		if key.Column != 1 {
			return body, fmt.Errorf("indented top-level YAML fields are unsupported for editing")
		}
		if seen[key.Value] {
			return body, fmt.Errorf("duplicate YAML field %s", key.Value)
		}
		seen[key.Value] = true

		value, ok := fields[key.Value]
		if !ok {
			continue
		}
		end := len(source)
		if i+2 < len(mapping.Content) {
			end = position(mapping.Content[i+2].Line)
		}
		// Keep standalone comments and whitespace preceding the next field.
		for end > position(key.Line) {
			trimEnd := end
			if trimEnd > 0 && source[trimEnd-1] == '\n' {
				trimEnd--
			}
			begin := strings.LastIndex(source[:trimEnd], "\n") + 1
			line := source[begin:trimEnd]
			if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
				break
			}
			end = begin
		}
		encoded, err := yaml.Marshal(map[string]any{key.Value: value})
		if err != nil {
			return body, err
		}
		spans = append(spans, editSpan{loc[2] + position(key.Line), loc[2] + end, string(encoded)})
	}
	var missing []string
	for key := range fields {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	suffix := ""
	for _, key := range missing {
		encoded, err := yaml.Marshal(map[string]any{key: fields[key]})
		if err != nil {
			return body, err
		}
		suffix += string(encoded)
	}
	if suffix != "" {
		spans = append(spans, editSpan{loc[3], loc[3], "\n" + strings.TrimSuffix(suffix, "\n")})
	}
	return applyEditSpans(body, spans), nil
}

// editableWorkUnit retains invalid field values so a targeted edit can repair
// them while the equality guard still protects every unselected field.
func editableWorkUnit(number int, title, body string) (*WorkUnit, error) {
	clean := StripHTMLComments(body)
	var wu *WorkUnit
	if loc := fencedYAMLBlockRegex.FindStringSubmatch(clean); loc != nil {
		var raw WorkUnit
		if err := yaml.Unmarshal([]byte(loc[1]), &raw); err != nil {
			return nil, err
		}
		wu = &raw
	} else {
		wu, _ = parseMarkdownSections(clean)
	}
	if wu == nil {
		return nil, fmt.Errorf("issue does not contain editable WorkUnit sections")
	}
	if wu.ID == "" {
		wu.ID = fmt.Sprintf("ISSUE-%d", number)
	}
	if wu.IssueNumber == 0 {
		wu.IssueNumber = number
	}
	if wu.Title == "" {
		wu.Title = strings.TrimSpace(title)
	}
	return wu, nil
}

func checkEditableYAML(node *yaml.Node) error {
	if node.Anchor != "" || node.Kind == yaml.AliasNode || node.Tag == "!!merge" {
		return fmt.Errorf("YAML aliases and anchors are unsupported for editing")
	}
	for _, child := range node.Content {
		if err := checkEditableYAML(child); err != nil {
			return err
		}
	}
	return nil
}
