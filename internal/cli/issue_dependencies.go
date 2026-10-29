package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/tracker"
)

func dependencyDriftWarnings(ctx context.Context, tr tracker.Tracker, issue *tracker.Issue, deps []int) []string {
	if tr.BackendName() == "local" {
		return nil
	}
	unavailable := func(err error) []string {
		return []string{fmt.Sprintf("issue #%d native blocked-by check unavailable: %v", issue.Number, err)}
	}
	current, err := tracker.IssueReference(issue.HTMLURL)
	if err != nil {
		return unavailable(err)
	}
	if current.Number != issue.Number {
		return unavailable(fmt.Errorf("issue URL number mismatch"))
	}
	if configured, ok := tr.(interface{ UpstreamRepo() string }); ok && !strings.EqualFold(configured.UpstreamRepo(), current.Owner+"/"+current.Repo) {
		return unavailable(fmt.Errorf("issue URL repository mismatch"))
	}
	native, err := tr.ListBlockedBy(ctx, issue.Number)
	if err != nil {
		return unavailable(err)
	}
	bodySet := map[string]bool{}
	nativeSet := map[string]bool{}
	self := current.String()
	for _, n := range deps {
		if n != issue.Number {
			ref := current
			ref.Number = n
			bodySet[ref.String()] = true
		}
	}
	for _, ref := range native {
		if ref.Owner == "" || ref.Repo == "" || ref.Number <= 0 {
			return unavailable(fmt.Errorf("incomplete native issue identity"))
		}
		key := ref.String()
		if key != self {
			nativeSet[key] = true
		}
	}
	var warnings []string
	for key := range bodySet {
		if !nativeSet[key] {
			warnings = append(warnings, fmt.Sprintf("missing native blocked-by link: %s", key))
		}
	}
	for key := range nativeSet {
		if !bodySet[key] {
			warnings = append(warnings, fmt.Sprintf("native blocked-by link absent from depends_on: %s", key))
		}
	}
	sort.Strings(warnings)
	return warnings
}
