package pr

import (
	"context"
	"fmt"
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
		if st.CarryReason != "approved plan carried forward" {
			fmt.Fprintf(&b, "Next: swarm pr work %d\n", number)
		}
	}
	if r := st.LastRebase; r != nil {
		fmt.Fprintf(&b, "Rebase: %s\nHeads: %s -> %s\nBases: %s -> %s\nPatch identity: %s (verified: %t)\n", r.Status, r.OldHead, r.NewHead, r.OldBase, r.NewBase, r.PatchIdentity, r.Verified)
		if len(r.ConflictedPaths) > 0 {
			label := "Conflicted paths"
			if r.ConflictPathsReported {
				label = "Reported conflicted paths"
			}
			fmt.Fprintf(&b, "%s: %s\n", label, strings.Join(r.ConflictedPaths, ", "))
		}
	}
	return b.String()
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
