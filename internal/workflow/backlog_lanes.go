package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

// DefaultStaleDays is how long an issue can go without activity before a
// backlog scan suggests confirming it is still wanted.
const DefaultStaleDays = 90

// maxPublishedReportBytes keeps a published scan report under GitHub's
// 65,536-character comment limit with room for the trim note.
const maxPublishedReportBytes = 60000

// laneListLimit caps how many issues each lane lists in the report; the full
// list is always available through --export.
const laneListLimit = 15

// BacklogLane groups open issues by what a maintainer should do next. Lanes are
// suggestions only: a scan never closes, comments on, or relabels issues
// because of them.
type BacklogLane struct {
	Key    string `yaml:"key" json:"key"`
	Title  string `yaml:"title" json:"title"`
	Advice string `yaml:"advice" json:"advice"`
	Issues []int  `yaml:"issues" json:"issues"`
}

// laneOrder lists lanes in the order a maintainer should look at them. An
// issue belongs to the first lane that matches.
var laneOrder = []struct {
	key, title, advice string
	match              func(it *IssueSenseItem, staleDays int) bool
}{
	{"stabilize", "Stabilize first", "Users may be affected now. Approve a minimal mitigation and file the root cause separately.",
		func(it *IssueSenseItem, _ int) bool { return it.Shape == ShapeUrgent }},
	{"duplicates", "Probably duplicates", "Compare each with the older issue it resembles; close the duplicate or merge the details by hand.",
		func(it *IssueSenseItem, _ int) bool { return it.DuplicateOf != 0 }},
	{"quiet", "Quiet for a while", "No activity in a long time. Ask whether it still matters, or close it.",
		func(it *IssueSenseItem, staleDays int) bool {
			return staleDays > 0 && it.QuietDays >= staleDays && !it.Approved
		}},
	{"ask-author", "Ask the author", "Missing Objective, Constraints, or Acceptance Criteria. `--apply --normalize` drafts them for the author to confirm.",
		func(it *IssueSenseItem, _ int) bool { return it.RecommendedLabel == "swarm:needs-scope" }},
	{"waiting", "Waiting on something", "Blocked by open dependencies or protected paths.",
		func(it *IssueSenseItem, _ int) bool { return it.RecommendedLabel == "swarm:blocked" }},
	{"ready", "Ready for agents", "Approved and claimable with `swarm issue work --next`.",
		func(it *IssueSenseItem, _ int) bool { return it.Approved }},
	{"decide", "Needs a maintainer decision", "Valid drafts. Routine ones can be approved in a batch; read Exploratory ones first.",
		func(it *IssueSenseItem, _ int) bool { return true }},
}

func assignLanes(items []IssueSenseItem, staleDays int) []BacklogLane {
	lanes := make([]BacklogLane, len(laneOrder))
	for i, l := range laneOrder {
		lanes[i] = BacklogLane{Key: l.key, Title: l.title, Advice: l.advice}
	}
	for i := range items {
		for li, l := range laneOrder {
			if l.match(&items[i], staleDays) {
				items[i].Lane = l.key
				lanes[li].Issues = append(lanes[li].Issues, items[i].IssueNumber)
				break
			}
		}
	}
	out := lanes[:0]
	for _, l := range lanes {
		if len(l.Issues) > 0 {
			out = append(out, l)
		}
	}
	return out
}

// quietDays returns whole days since the issue's last recorded activity.
func quietDays(now, updated, created time.Time) int {
	last := updated
	if last.IsZero() {
		last = created
	}
	if last.IsZero() || now.Before(last) {
		return 0
	}
	return int(now.Sub(last).Hours() / 24)
}

var (
	tokenSplit = regexp.MustCompile(`[^a-z0-9]+`)
	// Words that appear in almost every issue title, including issue-form prefixes.
	dupStopWords = map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "when": true, "from": true, "into": true, "that": true,
		"this": true, "not": true, "are": true, "does": true, "should": true, "can": true, "add": true, "fix": true,
		"bug": true, "bugfix": true, "feature": true, "refactor": true, "swarm": true, "issue": true, "support": true,
		"request": true, "using": true, "use": true, "after": true, "before": true, "have": true, "has": true,
	}
)

func dupTokens(text string) map[string]bool {
	out := make(map[string]bool)
	for _, tok := range tokenSplit.Split(strings.ToLower(text), -1) {
		if len(tok) >= 3 && !dupStopWords[tok] {
			out[tok] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

type dupCandidate struct {
	number int
	title  map[string]bool
	text   map[string]bool
}

func newDupCandidate(number int, title, body string) dupCandidate {
	first := ""
	for _, para := range strings.Split(strings.TrimSpace(contract.StripHTMLComments(body)), "\n\n") {
		if p := strings.TrimSpace(para); p != "" && !strings.HasPrefix(p, "#") {
			first = p
			break
		}
	}
	return dupCandidate{number: number, title: dupTokens(title), text: dupTokens(title + " " + first)}
}

// markDuplicates flags each issue that closely resembles an older open issue.
// It compares title words, and title plus first paragraph, so it is cheap and
// deterministic; a maintainer always makes the final call.
func markDuplicates(items []IssueSenseItem, cands []dupCandidate) {
	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return cands[idx[a]].number < cands[idx[b]].number })
	for pos, i := range idx {
		best, bestScore := 0, 0.0
		for _, j := range idx[:pos] {
			score := 0.0
			if len(cands[i].title) >= 3 && len(cands[j].title) >= 3 {
				if s := jaccard(cands[i].title, cands[j].title); s >= 0.7 {
					score = s
				}
			}
			if len(cands[i].text) >= 5 && len(cands[j].text) >= 5 {
				if s := jaccard(cands[i].text, cands[j].text); s >= 0.6 && s > score {
					score = s
				}
			}
			if score > bestScore {
				best, bestScore = cands[j].number, score
			}
		}
		if best != 0 {
			items[i].DuplicateOf = best
			items[i].SafetyNotes = append(items[i].SafetyNotes, fmt.Sprintf("Looks like #%d (%.0f%% of the same words)", best, bestScore*100))
		}
	}
}

func renderLanesSection(sb *strings.Builder, lanes []BacklogLane, items []IssueSenseItem) {
	if len(lanes) == 0 {
		return
	}
	byNum := make(map[int]*IssueSenseItem, len(items))
	for i := range items {
		byNum[items[i].IssueNumber] = &items[i]
	}
	sb.WriteString("\n#### 🗂️ Backlog at a Glance\n\n")
	sb.WriteString("| Lane | Issues | What to do |\n| :--- | :--- | :--- |\n")
	for _, l := range lanes {
		fmt.Fprintf(sb, "| **%s** | `%d` | %s |\n", l.Title, len(l.Issues), l.Advice)
	}
	sb.WriteString("\n")
	for _, l := range lanes {
		fmt.Fprintf(sb, "<details>\n<summary>%s (%d)</summary>\n\n", l.Title, len(l.Issues))
		for n, num := range l.Issues {
			if n == laneListLimit {
				fmt.Fprintf(sb, "- …and %d more (run `swarm repo plan --scan --export backlog.yaml` for the full list)\n", len(l.Issues)-laneListLimit)
				break
			}
			it := byNum[num]
			fmt.Fprintf(sb, "- `#%d` %s%s\n", num, escapeLaneText(it.Title), laneDetail(l.Key, it))
		}
		sb.WriteString("\n</details>\n")
	}
}

func laneDetail(key string, it *IssueSenseItem) string {
	switch {
	case key == "duplicates":
		return fmt.Sprintf(" — looks like `#%d`", it.DuplicateOf)
	case key == "quiet":
		return fmt.Sprintf(" — quiet for %d days", it.QuietDays)
	case len(it.ShapeSignals) > 0:
		return " — " + it.Shape.Title() + ": " + escapeLaneText(strings.Join(it.ShapeSignals, "; "))
	case it.Shape != "":
		return " — " + it.Shape.Title()
	}
	return ""
}

var laneTextEscaper = strings.NewReplacer("\n", " ", "\r", " ", "<", "&lt;", ">", "&gt;")

func escapeLaneText(s string) string { return laneTextEscaper.Replace(s) }

// boundPublishedReport trims a report to fit in one GitHub comment, cutting at
// a line boundary and closing any open <details> block. The lanes come before
// the full table, so the overview survives trimming.
func boundPublishedReport(md string) string {
	if len(md) <= maxPublishedReportBytes {
		return md
	}
	const note = "\n> [!NOTE]\n> This report was trimmed to fit in a GitHub comment. Run `swarm repo plan --scan --export backlog.yaml` for the full list.\n"
	budget := maxPublishedReportBytes - len(note) - len("\n</details>\n")
	var sb strings.Builder
	open := 0
	for _, line := range strings.SplitAfter(md, "\n") {
		if sb.Len()+len(line) > budget {
			break
		}
		sb.WriteString(line)
		trimmed := strings.TrimSpace(line)
		if trimmed == "<details>" {
			open++
		} else if trimmed == "</details>" && open > 0 {
			open--
		}
	}
	if open > 0 {
		sb.WriteString("\n</details>\n")
	}
	sb.WriteString(note)
	return sb.String()
}

// preserveOriginalReport appends the reporter's original text below a
// normalized WorkUnit so their words are not lost. Quoting every line means no
// line of the original can start a WorkUnit section, close a fenced WorkUnit
// YAML block, or name an external WorkUnit path.
func preserveOriginalReport(formatted, original string) string {
	clean := strings.TrimSpace(contract.StripHTMLComments(original))
	if clean == "" {
		return formatted
	}
	clean = strings.ReplaceAll(clean, "\r\n", "\n")
	lines := strings.Split(clean, "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	// The heading ends the last WorkUnit section; the parser ignores unknown sections.
	return strings.TrimRight(formatted, "\n") + "\n\n### Original Report\n\n<details>\n<summary>The reporter's text before normalization</summary>\n\n" + strings.Join(lines, "\n") + "\n\n</details>\n"
}
