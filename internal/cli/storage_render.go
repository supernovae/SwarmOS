package cli

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/storage"
)

// maxCleanupReasons keeps the summary to a screenful; --details lists the rest.
const maxCleanupReasons = 5

var ledgerEntryPrefix = regexp.MustCompile(`^ledger entry [0-9a-f]+: `)

// reasonTally counts items that were kept for the same reason.
type reasonTally struct {
	counts map[string]int
	order  []string
}

func (t *reasonTally) add(reason string) {
	if reason == "" {
		reason = "no reason recorded"
	}
	if t.counts == nil {
		t.counts = map[string]int{}
	}
	if t.counts[reason] == 0 {
		t.order = append(t.order, reason)
	}
	t.counts[reason]++
}

func (t *reasonTally) render(w io.Writer, indent string) {
	reasons := append([]string(nil), t.order...)
	sort.SliceStable(reasons, func(i, j int) bool { return t.counts[reasons[i]] > t.counts[reasons[j]] })
	for i, reason := range reasons {
		if i == maxCleanupReasons {
			rest := 0
			for _, r := range reasons[i:] {
				rest += t.counts[r]
			}
			fmt.Fprintf(w, "%s%d kept for other reasons\n", indent, rest)
			return
		}
		fmt.Fprintf(w, "%s%d kept: %s\n", indent, t.counts[reason], reason)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// renderStorageCleanup prints a summary by default. Details lists every task
// resource and host cache with its outcome and reason.
func renderStorageCleanup(w io.Writer, report storageCleanupReport, details bool) {
	apply := report.Mode == "apply"
	if len(report.Tasks) == 0 && len(report.Host.Resources) == 0 && len(report.Incomplete) == 0 {
		// Nothing was inspected; the returned error already says why.
		return
	}
	if apply {
		fmt.Fprintln(w, "Storage cleanup applied.")
	} else {
		fmt.Fprintln(w, "Storage cleanup preview. Nothing is removed until you run it with --apply.")
	}
	// A preview stays local, so tasks whose remote branch must be compared are
	// only decided by --apply. Count them apart from tasks that are truly kept.
	ready, deferred := 0, 0
	for _, s := range report.Tasks {
		fmt.Fprintf(w, "\n%s (%s)\n", s.Repository, s.Root)
		if details {
			renderBulkCleanup(w, s)
			ready += s.Totals.Eligible
			continue
		}
		var kept reasonTally
		done, failed, unchecked := 0, 0, 0
		for _, issue := range s.Issues {
			c := issue.Counts
			switch {
			case issue.Error != "" || c.Failed > 0:
				failed++
			case c.Retired > 0 || c.Eligible > 0:
				done++
			default:
				reason := issue.Blocked
				for _, r := range issue.Resources {
					if reason == "" && r.Disposition == "retained" {
						reason = r.Reason
					}
				}
				if strings.Contains(reason, gitcredential.RemoteVerificationDeferred) {
					unchecked++
				} else {
					kept.add(reason)
				}
			}
		}
		if !apply {
			ready += done
		}
		deferred += unchecked
		verb := "ready to clean up"
		if apply {
			verb = "cleaned up"
		}
		fmt.Fprintf(w, "  Tasks: %d of %d %s", done, len(s.Issues), verb)
		if failed > 0 {
			fmt.Fprintf(w, ", %d failed", failed)
		}
		fmt.Fprintln(w)
		if unchecked > 0 {
			fmt.Fprintf(w, "    %d not checked yet: a preview does not contact the remote; --apply checks them\n", unchecked)
		}
		kept.render(w, "    ")
		if s.Error != "" && len(s.Issues) == 0 {
			fmt.Fprintf(w, "    could not inspect tasks: %s\n", s.Error)
		}
		if len(s.Unresolved) > 0 {
			var other reasonTally
			for _, u := range s.Unresolved {
				other.add(u.Reason)
			}
			fmt.Fprintf(w, "  Other worktrees: %d left alone\n", len(s.Unresolved))
			other.render(w, "    ")
		}
	}
	if len(report.Host.Resources) > 0 {
		fmt.Fprintln(w)
		if details {
			fmt.Fprintln(w, "Host caches and scratch")
			for _, r := range report.Host.Resources {
				fmt.Fprintf(w, "  %s %s: %s: %s\n", r.Resource.Class, r.Resource.Location, r.Outcome, r.Reason)
				if !apply && r.Outcome == storage.ReclaimEligible {
					ready++
				}
			}
		} else {
			var kept reasonTally
			done, failed, absent := 0, 0, 0
			var bytes int64
			for _, r := range report.Host.Resources {
				switch r.Outcome {
				case storage.ReclaimEligible:
					done++
					if r.Before.AllocatedBytes != nil {
						bytes += *r.Before.AllocatedBytes
					}
				case storage.ReclaimRemoved:
					done++
					if r.ObservedAllocatedDelta != nil && *r.ObservedAllocatedDelta < 0 {
						bytes -= *r.ObservedAllocatedDelta
					} else if r.Before.AllocatedBytes != nil {
						bytes += *r.Before.AllocatedBytes
					}
				case storage.ReclaimFailed, storage.ReclaimPartial, storage.ReclaimCanceled:
					failed++
				case storage.ReclaimAbsent:
					absent++
				default:
					kept.add(ledgerEntryPrefix.ReplaceAllString(r.Reason, ""))
				}
			}
			if !apply {
				ready += done
			}
			verb := "ready to reclaim"
			if apply {
				verb = "reclaimed"
			}
			fmt.Fprintf(w, "Host caches and scratch: %d of %d %s", done, len(report.Host.Resources)-absent, verb)
			if bytes > 0 {
				fmt.Fprintf(w, " (about %s)", humanBytes(bytes))
			}
			if failed > 0 {
				fmt.Fprintf(w, ", %d failed", failed)
			}
			fmt.Fprintln(w)
			kept.render(w, "    ")
		}
	}
	if len(report.Incomplete) > 0 {
		fmt.Fprintln(w, "\nSome storage could not be checked and was left alone:")
		seen := map[string]bool{}
		for _, r := range report.Incomplete {
			if !seen[r] {
				fmt.Fprintf(w, "  - %s\n", r)
				seen[r] = true
			}
		}
	}
	fmt.Fprintln(w)
	switch {
	case apply:
	case ready > 0:
		fmt.Fprintf(w, "%s ready. Run the same command with --apply to clean up; each one is checked again first.\n", plural(ready, "item is", "items are"))
	case deferred > 0:
		fmt.Fprintf(w, "Nothing is confirmed ready, but %s. Run the same command with --apply to check them and clean up the finished ones.\n", plural(deferred, "task has not been checked", "tasks have not been checked"))
	default:
		fmt.Fprintln(w, "Nothing to clean up right now.")
	}
	if !details {
		fmt.Fprintln(w, "Add --details to list every item and why it was kept.")
	}
}
