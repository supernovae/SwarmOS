package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/tracker"
)

// Agents may note surprises about the repository in .swarm/active/LESSONS.md.
// SwarmOS copies them into the implementation artifact, where reviewers see
// them, and `swarm repo lessons` proposes lessons from merged work for a
// maintainer to commit to .swarm/rules/lessons.md. Lessons never become policy
// on their own: rules load from the approved base commit like every other rule.
const (
	activeLessonsFile = "LESSONS.md"
	LessonsRulePath   = ".swarm/rules/lessons.md"
	lessonsHeading    = "## What we learned"
	maxLessons        = 10
	maxLessonBytes    = 280
	maxLessonsFile    = 16 << 10
)

// ReadActiveLessons returns the cleaned lessons an agent left in a worktree's
// active bundle, or nil when there are none.
func ReadActiveLessons(wtPath string) []string {
	data, err := safefs.ReadFile(wtPath, filepath.Join(".swarm", "active", activeLessonsFile), maxLessonsFile)
	if err != nil {
		return nil
	}
	return SanitizeLessons(string(data))
}

var lessonSpace = regexp.MustCompile(`\s+`)

// SanitizeLessons keeps only Markdown bullet lines, collapses whitespace,
// trims each lesson to a bounded length, and drops duplicates.
func SanitizeLessons(raw string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(contract.StripHTMLComments(raw), "\n") {
		t := strings.TrimSpace(line)
		var ok bool
		for _, p := range []string{"- [ ] ", "- [x] ", "- ", "* "} {
			if strings.HasPrefix(t, p) {
				t, ok = strings.TrimSpace(strings.TrimPrefix(t, p)), true
				break
			}
		}
		if !ok {
			continue
		}
		t = lessonSpace.ReplaceAllString(t, " ")
		if len(t) > maxLessonBytes {
			cut := maxLessonBytes
			for cut > 0 && !utf8.RuneStart(t[cut]) {
				cut--
			}
			t = strings.TrimSpace(t[:cut]) + "…"
		}
		key := lessonKey(t)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
		if len(out) == maxLessons {
			break
		}
	}
	return out
}

func formatLessonsSection(lessons []string) string {
	if len(lessons) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(lessonsHeading + "\n\n")
	for _, l := range lessons {
		sb.WriteString("- " + l + "\n")
	}
	return sb.String()
}

// ExtractLessons reads the What we learned section from an implementation
// artifact body.
func ExtractLessons(body string) []string {
	_, rest, ok := strings.Cut(body, "\n"+lessonsHeading+"\n")
	if !ok {
		return nil
	}
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	return SanitizeLessons(rest)
}

func lessonKey(s string) string {
	return strings.Join(tokenSplit.Split(strings.ToLower(s), -1), " ")
}

// HarvestedLesson is a lesson from merged work, with where it came from.
type HarvestedLesson struct {
	Text        string `json:"text"`
	IssueNumber int    `json:"issue_number"`
	PRNumber    int    `json:"pr_number"`
}

// LessonsReport lists proposed additions to the lessons rule file.
type LessonsReport struct {
	Path        string            `json:"path"`
	ScannedPRs  int               `json:"scanned_prs"`
	MergedPRs   int               `json:"merged_prs"`
	Proposed    []HarvestedLesson `json:"proposed"`
	AlreadyKnow int               `json:"already_known"`
	Applied     bool              `json:"applied"`
}

var closingIssueRef = regexp.MustCompile(`(?im)^\s*(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

// HarvestLessons collects lessons from recently merged pull requests. A PR
// counts only when its linked issue's coordination state records that exact PR,
// so a PR body cannot attribute lessons to work it did not do; merging is the
// maintainer's acceptance of that work. Lessons already in the rule file are
// skipped. With apply, new lessons are appended to the local rule file for the
// maintainer to review and commit.
func (e *Engine) HarvestLessons(ctx context.Context, repoRoot string, limit int, apply bool) (*LessonsReport, error) {
	prs, err := e.Tracker.ListPullRequests(ctx, tracker.PullRequestFilter{State: "closed", Limit: limit})
	if err != nil {
		return nil, err
	}
	safe, err := safefs.Open(repoRoot)
	if err != nil {
		return nil, err
	}
	defer safe.Close()
	existing, err := safe.ReadFile(LessonsRulePath, 1<<20)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	known := make(map[string]bool)
	for _, line := range strings.Split(string(existing), "\n") {
		if t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")); t != "" {
			known[lessonKey(stripLessonSource(t))] = true
		}
	}

	report := &LessonsReport{Path: LessonsRulePath, ScannedPRs: len(prs)}
	for _, pr := range prs {
		if !pr.Merged {
			continue
		}
		report.MergedPRs++
		st, issueNum, err := e.linkedStateForPR(ctx, pr)
		if err != nil {
			return nil, err
		}
		if st == nil {
			continue
		}
		for _, a := range st.Artifacts {
			if a.Type != contract.ArtifactImplementation {
				continue
			}
			for _, text := range ExtractLessons(a.Body) {
				key := lessonKey(text)
				if known[key] {
					report.AlreadyKnow++
					continue
				}
				known[key] = true
				report.Proposed = append(report.Proposed, HarvestedLesson{Text: text, IssueNumber: issueNum, PRNumber: pr.Number})
			}
		}
	}
	if apply && len(report.Proposed) > 0 {
		if err := appendLessons(safe, existing, report.Proposed); err != nil {
			return nil, err
		}
		report.Applied = true
	}
	return report, nil
}

// linkedStateForPR returns the coordination state of the issue that records
// this PR, or nil when no hinted issue does.
func (e *Engine) linkedStateForPR(ctx context.Context, pr *tracker.PullRequest) (*lease.IssueState, int, error) {
	var hints []int
	if pr.IssueNumber > 0 {
		hints = append(hints, pr.IssueNumber)
	}
	for _, m := range closingIssueRef.FindAllStringSubmatch(pr.Body, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			hints = append(hints, n)
		}
	}
	for _, n := range hints {
		st, err := e.LeaseManager.GetCurrentState(ctx, n)
		if errors.Is(err, coordination.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if st != nil && st.PRNumber == pr.Number {
			return st, n, nil
		}
	}
	return nil, 0, nil
}

var lessonSource = regexp.MustCompile(`\s*\(learned in #\d+(?:, PR #\d+)?\)$`)

func stripLessonSource(s string) string { return lessonSource.ReplaceAllString(s, "") }

func appendLessons(safe *safefs.Root, existing []byte, lessons []HarvestedLesson) error {
	content := string(existing)
	if strings.TrimSpace(content) == "" {
		content = "# Lessons From This Repository\n\nThings that have surprised contributors here before. Agents read this file with every task.\n"
	}
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(content, "\n") + "\n\n")
	for _, l := range lessons {
		sb.WriteString("- " + l.Text + " (learned in #" + strconv.Itoa(l.IssueNumber) + ", PR #" + strconv.Itoa(l.PRNumber) + ")\n")
	}
	return safe.WriteFile(LessonsRulePath, []byte(sb.String()), 0o644)
}
