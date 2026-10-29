package lease

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/supernovae/swarmos/internal/contract"
)

const PRDescriptionLimit = 59999

var descriptionNames = []string{"issue-description", "review-evidence", "rebase"}

func descriptionMarker(name, edge string) string { return "<!-- swarm:" + name + ":" + edge + " -->" }

// DescriptionText prevents artifact prose from impersonating generated boundaries.
func DescriptionText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "<!--", "&lt;!--")
}
func DescriptionSlot(s string) string {
	s = DescriptionText(s)
	return fmt.Sprintf("<!-- swarm:excerpt:v1:%d -->", len(s)) + s + "<!-- swarm:excerpt:end -->\n<!-- swarm:omission:start --><!-- swarm:omission:end -->\n"
}

const atomicSlot = "<!-- swarm:atomic -->"

func DescriptionAtomicSlot(s string) string {
	return fmt.Sprintf("<!-- swarm:excerpt:v1:%d -->", len(DescriptionText(s))) + atomicSlot + DescriptionText(s) + "<!-- swarm:excerpt:end -->\n<!-- swarm:omission:start --><!-- swarm:omission:end -->\n"
}

var slotPattern = regexp.MustCompile(`(?s)<!-- swarm:excerpt:v1:([0-9]+) -->(.*?)<!-- swarm:excerpt:end -->\n<!-- swarm:omission:start -->.*?<!-- swarm:omission:end -->\n`)

func compactDescription(s string, budget int) string {
	slots := slotPattern.FindAllStringSubmatchIndex(s, -1)
	if len(slots) == 0 {
		return s
	}
	var b strings.Builder
	pos := 0
	for i, m := range slots {
		n := budget / len(slots)
		if i < budget%len(slots) {
			n++
		}
		content := s[m[4]:m[5]]
		atomic := strings.HasPrefix(content, atomicSlot)
		if atomic {
			content = strings.TrimPrefix(content, atomicSlot)
			if len(content) > n {
				content = ""
			}
		}
		if len(content) > n {
			content = content[:n]
			for !utf8.ValidString(content) {
				content = content[:len(content)-1]
			}
		}
		b.WriteString(s[pos:m[4]])
		if atomic {
			b.WriteString(atomicSlot)
		}
		b.WriteString(content)
		b.WriteString("<!-- swarm:excerpt:end -->\n<!-- swarm:omission:start -->")
		original, _ := strconv.Atoi(s[m[2]:m[3]])
		notice := ""
		if len(content) < original {
			if atomic {
				notice = "Inline telemetry omitted; complete runs remain in durable state."
			} else {
				notice = "Inline details omitted; complete artifacts remain in durable state."
			}
		}
		// Fixed-width notice space makes mandatory byte accounting independent
		// of whether an excerpt fits. Complete excerpts show no omission notice.
		b.WriteString(notice)
		b.WriteString(strings.Repeat(" ", 80-len(notice)))
		b.WriteString("<!-- swarm:omission:end -->\n")
		pos = m[1]
	}
	b.WriteString(s[pos:])
	return b.String()
}

func validDescriptionSlots(section string) bool {
	matches := slotPattern.FindAllStringSubmatch(section, -1)
	if strings.Count(section, "<!-- swarm:excerpt:") != 2*len(matches) || strings.Count(section, "<!-- swarm:omission:start -->") != len(matches) || strings.Count(section, "<!-- swarm:omission:end -->") != len(matches) {
		return false
	}
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return false
		}
		if len(strings.TrimPrefix(m[2], atomicSlot)) > n {
			return false
		}
	}
	return true
}

var legacyArtifactHeading = regexp.MustCompile(`(?m)^### (design|test-plan|review|implementation|validation|rebase-plan): ([^\n]*)\n\n`)
var legacyPlanDigest = regexp.MustCompile(`(?m)^(?:Plan SHA-256:|Test plan SHA-256:|Operator-steered:)[^\n]*$`)
var legacyMicroPlan = regexp.MustCompile(`(?m)^Rebase micro-plan at [^\n]* \(digest [^\n]*\):\n`)

// migrateDescription recognizes only the two historical generated layouts.
// Ambiguous layouts remain intact and require maintainer repair.
func migrateDescription(name, text, ref string, issue int) (string, error) {
	start, end := descriptionMarker(name, "start"), descriptionMarker(name, "end")
	content := strings.TrimSuffix(strings.TrimPrefix(text, start), end)
	var b strings.Builder
	switch name {
	case "review-evidence":
		if !strings.HasPrefix(content, "\n## SwarmOS review evidence\n\n") {
			return "", fmt.Errorf("unrecognized legacy review layout")
		}
		b.WriteString("## SwarmOS review evidence\n")
		matches := legacyArtifactHeading.FindAllStringSubmatchIndex(content, -1)
		if len(matches) == 0 {
			return "", fmt.Errorf("ambiguous legacy review inventory")
		}
		prefix := content[:matches[0][0]]
		for _, line := range legacyPlanDigest.FindAllString(prefix, -1) {
			b.WriteString(DescriptionText(line) + "\n")
		}
		b.WriteString(DescriptionSlot(prefix))
		for i, m := range matches {
			stop := len(content)
			if i+1 < len(matches) {
				stop = matches[i+1][0]
			}
			typ, verdict := content[m[2]:m[3]], content[m[4]:m[5]]
			part := content[m[1]:stop]
			fmt.Fprintf(&b, "### %s: %s\n", typ, DescriptionText(verdict))
			for _, line := range legacyPlanDigest.FindAllString(part, -1) {
				b.WriteString(DescriptionText(line) + "\n")
			}
			if typ == "validation" {
				b.WriteString(validationInventory(part))
			}
			b.WriteString(DescriptionSlot(part))
		}
	case "rebase":
		if !strings.HasPrefix(content, "\n### Rebase context\n\n```text\n") || !strings.HasSuffix(content, "```\n") {
			return "", fmt.Errorf("unrecognized legacy rebase layout")
		}
		content = strings.TrimSuffix(strings.TrimPrefix(content, "\n### Rebase context\n\n```text\n"), "```\n")
		if !strings.Contains(content, "\nHeads: ") || !strings.Contains(content, "\nPatch identity: ") {
			return "", fmt.Errorf("ambiguous legacy rebase inventory")
		}
		b.WriteString("### Rebase context\n")
		matches := legacyMicroPlan.FindAllStringIndex(content, -1)
		if len(matches) == 0 {
			b.WriteString(DescriptionText(content))
			break
		}
		b.WriteString(DescriptionText(content[:matches[0][0]]))
		for i, m := range matches {
			stop := len(content)
			if i+1 < len(matches) {
				stop = matches[i+1][0]
			}
			part := content[m[1]:stop]
			// Resolutions follow the final micro-plan in the historical renderer.
			if i == len(matches)-1 {
				for _, prefix := range []string{"\nResolved by ", "\nReported resolved by "} {
					if at := strings.Index(part, prefix); at >= 0 {
						b.WriteString(DescriptionText(part[at:]))
						part = part[:at]
					}
				}
			}
			b.WriteString(DescriptionText(content[m[0]:m[1]]))
			b.WriteString(DescriptionSlot(part))
		}
	default:
		return "", fmt.Errorf("unrecognized legacy issue layout")
	}
	b.WriteString(DescriptionRetrieval(issue, ref))
	return start + "\n<!-- swarm:description:v1 -->\n" + b.String() + end, nil
}

// ComposePRDescription preserves every outside byte and budgets all owned sections.
func ComposePRDescription(body, name, section, stateRef string, issue int) (string, error) {
	if !utf8.ValidString(body) || !utf8.ValidString(section) {
		return "", fmt.Errorf("PR description contains invalid UTF-8; a maintainer must repair the body before retrying")
	}
	type span struct {
		start, end, id int
		text           string
	}
	var spans []span
	for id, n := range descriptionNames {
		a, z := descriptionMarker(n, "start"), descriptionMarker(n, "end")
		if strings.Count(body, a) != strings.Count(body, z) || strings.Count(body, a) > 1 {
			return "", fmt.Errorf("malformed Swarm description markers; a maintainer must repair markers on GitHub before retrying")
		}
		start := strings.Index(body, a)
		if start < 0 {
			continue
		}
		end := strings.Index(body, z)
		if end < start {
			return "", fmt.Errorf("malformed Swarm description markers; a maintainer must repair markers on GitHub before retrying")
		}
		text := body[start : end+len(z)]
		if strings.Contains(text, "<!-- swarm:description:v1 -->") && (!strings.HasPrefix(text, a+"\n<!-- swarm:description:v1 -->\n") || strings.Count(text, "<!-- swarm:description:v1 -->") != 1 || !validDescriptionSlots(text)) {
			return "", fmt.Errorf("malformed Swarm excerpt markers; a maintainer must repair markers on GitHub before retrying")
		}
		if !strings.Contains(text, "<!-- swarm:description:v1 -->") {
			migrated, err := migrateDescription(n, text, stateRef, issue)
			if err != nil {
				return "", fmt.Errorf("ambiguous legacy Swarm section (%v); a maintainer must repair markers on GitHub before retrying", err)
			}
			text = migrated
		}
		spans = append(spans, span{start, end + len(z), id, text})
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			if spans[j].start < spans[i].start {
				spans[i], spans[j] = spans[j], spans[i]
			}
		}
	}
	target := -1
	for i, n := range descriptionNames {
		if n == name {
			target = i
		}
	}
	if target < 0 {
		return "", fmt.Errorf("unknown description section")
	}
	if !validDescriptionSlots(section) {
		return "", fmt.Errorf("malformed generated Swarm excerpt markers")
	}
	section = descriptionMarker(name, "start") + "\n<!-- swarm:description:v1 -->\n" + section + descriptionMarker(name, "end")
	found := false
	last := 0
	mandatory := 0
	for i := range spans {
		s := &spans[i]
		if s.start < last {
			return "", fmt.Errorf("nested Swarm description markers; a maintainer must repair markers on GitHub before retrying")
		}
		mandatory += s.start - last
		last = s.end
		if s.id == target {
			s.text = section
			found = true
		}
		mandatory += len(compactDescription(s.text, 0))
	}
	mandatory += len(body) - last
	if !found {
		mandatory += 2 + len(compactDescription(section, 0))
	}
	if mandatory > PRDescriptionLimit {
		outsideBytes := len(body)
		for _, s := range spans {
			outsideBytes -= s.end - s.start
		}
		if !found {
			outsideBytes += 2
		}
		if mandatory-outsideBytes > PRDescriptionLimit {
			return "", fmt.Errorf("required Swarm outcome/provenance/rebase inventory alone exceeds 59999 bytes. A maintainer must review the mandatory inventory and WorkUnit scope before retrying the same submit command; trim outside PR prose on GitHub where possible. Complete stored evidence must remain in %s (swarm issue status %d --json)", stateRef, issue)
		}
		return "", fmt.Errorf("PR description cannot fit preserved text and required Swarm evidence (mandatory inventory) within 59999 bytes. A maintainer must trim the unmarked/outside PR body on GitHub, then retry the same submit command; complete artifacts remain in %s (swarm issue status %d --json)", stateRef, issue)
	}
	remaining := PRDescriptionLimit - mandatory
	share := func(id int) int {
		n := remaining / 3
		if id < remaining%3 {
			n++
		}
		return n
	}
	var out strings.Builder
	last = 0
	for _, s := range spans {
		out.WriteString(body[last:s.start])
		out.WriteString(compactDescription(s.text, share(s.id)))
		last = s.end
	}
	out.WriteString(body[last:])
	if !found {
		out.WriteString("\n\n" + compactDescription(section, share(target)))
	}
	return out.String(), nil
}

// DescriptionStateReference reuses a retrieval hint only within the owned issue
// envelope. It is display context, never authority for artifacts or outcomes.
func DescriptionStateReference(body string, issue int) string {
	start, end := strings.Index(body, descriptionMarker("issue-description", "start")), strings.Index(body, descriptionMarker("issue-description", "end"))
	if start >= 0 && end > start {
		section := body[start:end]
		for _, prefix := range []string{"refs/heads/swarm-state/", "refs/swarm/state/"} {
			ref := fmt.Sprintf("%sissue-%d", prefix, issue)
			if strings.Contains(section, "Complete artifacts: `"+ref+"`") {
				return ref
			}
		}
	}
	return "<selected-state-ref>"
}
func DescriptionRetrieval(issue int, ref string, remote ...string) string {
	if ref == "<selected-state-ref>" {
		return fmt.Sprintf("\nComplete artifacts: `swarm issue status %d --json` (state.artifacts and state.revision_plan). For Git retrieval, replace `<selected-state-ref>` below with `refs/heads/swarm-state/issue-%d` for the branch namespace or `refs/swarm/state/issue-%d` for the hidden namespace configured by your maintainer.\n```sh\ngit fetch <upstream-remote> <selected-state-ref>\ngit show FETCH_HEAD:state.json\n```\n", issue, issue, issue)
	}
	upstream := "<upstream-remote>"
	instruction := "Fetch from your upstream remote (replace `<upstream-remote>`)"
	if len(remote) > 0 && remote[0] != "" {
		upstream = "'" + strings.ReplaceAll(remote[0], "'", "'\\''") + "'"
		instruction = "Fetch from the configured upstream remote"
	}
	return fmt.Sprintf("\nComplete artifacts: `%s`; `swarm issue status %d --json`. %s:\n```sh\ngit fetch %s %s\ngit show FETCH_HEAD:state.json\n```\nFull bodies and digests are in artifacts[]; revision plans are in revision_plan.\n", ref, issue, instruction, upstream, ref)
}

// DescriptionArtifacts extracts outcomes only from the validator's pre-log table.
func DescriptionArtifacts(st *IssueState) string {
	if st == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(DescriptionText(PlanSummary(st, false)))
	b.WriteString(DescriptionSlot(PlanSummary(st, true)))
	for _, a := range st.Artifacts {
		fmt.Fprintf(&b, "\n### %s: %s\nSHA-256: %s\n", DescriptionText(string(a.Type)), DescriptionText(a.Verdict), DescriptionText(a.Metadata["content_sha256"]))
		if a.Metadata["operator_steered"] == "true" {
			fmt.Fprintf(&b, "Operator-steered: %s messages; SHA-256 `%s`.\n", DescriptionText(a.Metadata["guidance_count"]), DescriptionText(a.Metadata["guidance_sha256"]))
		}
		if a.Type == contract.ArtifactValidation {
			b.WriteString(validationInventory(a.Body))
		}
		b.WriteString(DescriptionSlot("Producer: " + a.Producer + "\n" + a.Summary + "\n" + a.Body))
	}
	return b.String()
}
func validationInventory(body string) string {
	prefix, _, _ := strings.Cut(body, "\n### Output:")
	lines := strings.Split(prefix, "\n")
	var rows []string
	for i, line := range lines {
		if line != "| Check | Command | Status | Duration |" {
			continue
		}
		if i+1 >= len(lines) {
			break
		}
		sep := strings.Split(lines[i+1], "|")
		if len(sep) != 6 {
			break
		}
		valid := true
		for _, s := range sep[1:5] {
			if strings.Trim(s, " :-\t") == "" && strings.Contains(s, "-") {
				continue
			}
			valid = false
		}
		if !valid {
			break
		}
		for _, row := range lines[i+2:] {
			if !strings.HasPrefix(row, "|") {
				break
			}
			cells := strings.Split(row, "|")
			if len(cells) != 6 || strings.TrimSpace(cells[1]) == "" || strings.TrimSpace(cells[3]) == "" {
				return "check table unavailable; full validation artifact retained\n"
			}
			rows = append(rows, DescriptionText(strings.TrimSpace(cells[1]))+": "+DescriptionText(strings.TrimSpace(cells[3])))
		}
		break
	}
	if len(rows) == 0 {
		return "check table unavailable; full validation artifact retained\n"
	}
	return strings.Join(rows, "\n") + "\n"
}
