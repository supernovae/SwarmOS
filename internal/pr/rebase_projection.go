package pr

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

const rebaseStart = "<!-- swarm:rebase:start -->"
const rebaseEnd = "<!-- swarm:rebase:end -->"

// RebaseSummary formats authoritative review context for CLI and PR projection.
func RebaseSummary(st *lease.IssueState, number int) string {
	if st == nil {
		return ""
	}
	var b strings.Builder
	if st.CarryReason != "" {
		fmt.Fprintf(&b, "Plan: %s\n", st.CarryReason)
		if len(st.ChangedPolicyPaths) > 0 {
			fmt.Fprintf(&b, "Policy paths: %s\n", strings.Join(st.ChangedPolicyPaths, ", "))
		}
		if st.CarryReason != "approved plan carried forward" && st.CarryReason != "approved plan re-reviewed" {
			fmt.Fprintf(&b, "Next: swarm pr work %d\n", number)
		}
	}
	if r := st.LastRebase; r != nil {
		fmt.Fprintf(&b, "Rebase: %s\nHeads: %s -> %s\nBases: %s -> %s\nPatch identity: %s (verified: %t)\n", r.Status, r.OldHead, r.NewHead, r.OldBase, r.NewBase, r.PatchIdentity, r.Verified)
		fmt.Fprintf(&b, "Conflicts resolved: %d\nRepair attempts: %d\n", len(r.Resolutions), r.RepairAttempts)
		if r.Status == "completed" {
			if r.DroppedPathsVerified {
				b.WriteString(DroppedPathsWarning(r.DroppedPaths, "", r.OldHead, r.NewHead, number, st.ForkRepo != ""))
			} else {
				b.WriteString("Changed-path comparison: unavailable\n")
			}
		}
		if len(r.ConflictedPaths) > 0 {
			label := "Conflicted paths"
			if r.ConflictPathsReported {
				label = "Reported conflicted paths"
			}
			fmt.Fprintf(&b, "%s: %s\n", label, strings.Join(r.ConflictedPaths, ", "))
		}
		for _, plan := range r.Plans {
			fmt.Fprintf(&b, "Rebase micro-plan at %s (digest %s):\n%s\n", plan.Commit, plan.Digest, plan.Body)
		}
		for _, res := range r.Resolutions {
			label := "Resolved by"
			if res.Reported {
				label = "Reported resolved by"
			}
			tool := strings.TrimSpace(strings.Join([]string{res.Harness, res.HarnessVersion}, " "))
			if res.Sandbox != "" {
				tool = strings.TrimSpace(tool + ", " + res.Sandbox)
			}
			if tool != "" {
				tool = " (" + strings.TrimPrefix(tool, ", ") + ")"
			}
			fmt.Fprintf(&b, "%s %s%s at %s: %s\n", label, res.Agent, tool, res.Commit, strings.Join(res.Paths, ", "))
		}
	}
	return b.String()
}

// DroppedPathsWarning names each path the PR changed before a rebase but no
// longer changes, with the next steps. It never gates the rebase.
func DroppedPathsWarning(paths []string, source, oldHead, newHead string, number int, fork bool) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	if source != "" {
		fmt.Fprintf(&b, "Changed-path comparison source: %s\n", source)
	}
	for _, p := range paths {
		fmt.Fprintf(&b, "Warning: no longer changed: %s\n", displayPath(p))
	}
	flag := forkFlag(fork)
	fmt.Fprintf(&b, "Inspect each path with git diff %s %s -- <path>. To restore intended changes, run swarm %spr work %d. swarm %spr rebase %d --abort abandons a paused rebase only; it cannot undo a completed one.\n", oldHead, newHead, flag, number, flag, number)
	return b.String()
}

// displayPath quotes paths containing control characters for terminals.
func displayPath(p string) string {
	if strings.IndexFunc(p, func(c rune) bool { return c < 0x20 || c == 0x7f }) >= 0 {
		return strconv.Quote(p)
	}
	return p
}

// ProjectRebase replaces only its own managed section, preserving human text.
// Callers must authorize publication before invoking this projection.
func ProjectRebase(ctx context.Context, tr tracker.Tracker, st *lease.IssueState, number int) error {
	if st == nil || st.LastRebase == nil {
		return nil
	}
	pr, err := tr.GetPullRequest(ctx, number)
	if err != nil {
		return err
	}
	body := pr.Body
	start := strings.Index(body, rebaseStart)
	if start >= 0 {
		end := strings.Index(body[start:], rebaseEnd)
		if end < 0 {
			return fmt.Errorf("managed rebase section is incomplete")
		}
		body = body[:start] + body[start+end+len(rebaseEnd):]
	}
	section := rebaseStart + "\n### Rebase context\n\n```text\n" + strings.ReplaceAll(RebaseSummary(st, number), "```", "` ` `") + "```\n" + rebaseEnd
	if start >= 0 {
		body = body[:start] + section + body[start:]
	} else {
		body = strings.TrimRight(body, "\n") + "\n\n" + section + "\n"
	}
	if body == pr.Body {
		return nil
	}
	_, err = tr.UpdateDraftPR(ctx, number, pr.Title, body)
	return err
}
