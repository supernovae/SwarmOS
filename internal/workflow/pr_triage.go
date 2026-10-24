package workflow

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/tracker"
)

// PRSenseItem is one open pull request in a backlog scan. Scans only read pull
// requests; they never comment on, label, or close them.
type PRSenseItem struct {
	Number    int    `yaml:"number" json:"number"`
	Title     string `yaml:"title" json:"title"`
	Author    string `yaml:"author,omitempty" json:"author,omitempty"`
	Draft     bool   `yaml:"draft" json:"draft"`
	QuietDays int    `yaml:"quiet_days,omitempty" json:"quiet_days,omitempty"`
	// WorkUnit is set only when the issue's coordination state records this PR.
	WorkUnit int `yaml:"work_unit,omitempty" json:"work_unit,omitempty"`
	// Mentions lists issues the PR body says it closes; these are hints only.
	Mentions []int  `yaml:"mentions,omitempty" json:"mentions,omitempty"`
	Lane     string `yaml:"lane" json:"lane"`
}

var prLaneOrder = []struct{ key, title, advice string }{
	{"competing", "Competing PRs", "Several open PRs target the same issue. Pick one to continue and close the rest."},
	{"quiet", "Quiet PRs", "No activity in a long time. Ask the author whether to continue, or close it."},
	{"review", "Ready for review", "Out of draft and waiting on a reviewer."},
	{"in-progress", "In progress", "Drafts still being worked on. Nothing to do yet."},
}

// triageOpenPullRequests reads open PRs and sorts them into lanes. A PR's
// WorkUnit link is confirmed against coordination state, never taken from its body.
func (e *Engine) triageOpenPullRequests(ctx context.Context, now time.Time, staleDays int) ([]PRSenseItem, []BacklogLane, error) {
	prs, err := e.Tracker.ListPullRequests(ctx, tracker.PullRequestFilter{State: "open"})
	if err != nil {
		return nil, nil, err
	}
	items := make([]PRSenseItem, 0, len(prs))
	targets := make(map[int][]int) // issue -> PRs that reference it
	for _, pr := range prs {
		it := PRSenseItem{
			Number: pr.Number, Title: pr.Title, Author: pr.Author, Draft: pr.Draft,
			QuietDays: quietDays(now, pr.UpdatedAt, pr.CreatedAt),
		}
		st, issueNum, err := e.linkedStateForPR(ctx, pr)
		if err != nil {
			return nil, nil, err
		}
		if st != nil {
			it.WorkUnit = issueNum
		}
		seen := map[int]bool{}
		for _, m := range closingIssueRef.FindAllStringSubmatch(pr.Body, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && !seen[n] {
				seen[n] = true
				it.Mentions = append(it.Mentions, n)
			}
		}
		if it.WorkUnit != 0 && !seen[it.WorkUnit] {
			it.Mentions = append(it.Mentions, it.WorkUnit)
		}
		for _, n := range it.Mentions {
			targets[n] = append(targets[n], pr.Number)
		}
		items = append(items, it)
	}
	competing := map[int]bool{}
	for _, nums := range targets {
		if len(nums) > 1 {
			for _, n := range nums {
				competing[n] = true
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Number < items[j].Number })
	lanes := make([]BacklogLane, len(prLaneOrder))
	for i, l := range prLaneOrder {
		lanes[i] = BacklogLane{Key: l.key, Title: l.title, Advice: l.advice}
	}
	for i := range items {
		it := &items[i]
		var idx int
		switch {
		case competing[it.Number]:
			idx = 0
		case staleDays > 0 && it.QuietDays >= staleDays:
			idx = 1
		case !it.Draft:
			idx = 2
		default:
			idx = 3
		}
		it.Lane = lanes[idx].Key
		lanes[idx].Issues = append(lanes[idx].Issues, it.Number)
	}
	out := lanes[:0]
	for _, l := range lanes {
		if len(l.Issues) > 0 {
			out = append(out, l)
		}
	}
	return items, out, nil
}

func renderPRSection(sb *strings.Builder, lanes []BacklogLane, items []PRSenseItem) {
	if len(items) == 0 {
		return
	}
	byNum := make(map[int]*PRSenseItem, len(items))
	for i := range items {
		byNum[items[i].Number] = &items[i]
	}
	fmt.Fprintf(sb, "\n#### 🔀 Open Pull Requests (%d)\n\n", len(items))
	sb.WriteString("| Lane | PRs | What to do |\n| :--- | :--- | :--- |\n")
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
			fmt.Fprintf(sb, "- PR `#%d` %s — %s\n", num, escapeLaneText(it.Title), prDetail(l.Key, it))
		}
		sb.WriteString("\n</details>\n")
	}
}

func prDetail(lane string, it *PRSenseItem) string {
	var parts []string
	switch {
	case it.WorkUnit != 0:
		parts = append(parts, fmt.Sprintf("WorkUnit `#%d`", it.WorkUnit))
	case len(it.Mentions) > 0:
		refs := make([]string, len(it.Mentions))
		for i, n := range it.Mentions {
			refs[i] = fmt.Sprintf("`#%d`", n)
		}
		parts = append(parts, "no WorkUnit, mentions "+strings.Join(refs, ", "))
	default:
		parts = append(parts, "no WorkUnit")
	}
	if it.Author != "" {
		parts = append(parts, "by "+escapeLaneText(it.Author))
	}
	if lane == "quiet" {
		parts = append(parts, fmt.Sprintf("quiet for %d days", it.QuietDays))
	}
	return strings.Join(parts, ", ")
}
