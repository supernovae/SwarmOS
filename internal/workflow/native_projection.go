package workflow

import (
	"context"
	"fmt"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/tracker"
)

// ProjectDependencies mirrors body dependencies without changing their authority.
func ProjectDependencies(ctx context.Context, tr tracker.Tracker, number int, deps []int, warn func(string)) {
	seen := map[int]bool{}
	for _, dep := range deps {
		if dep == number || seen[dep] {
			continue
		}
		seen[dep] = true
		if err := tr.LinkBlockedBy(ctx, number, dep); err != nil && warn != nil {
			warn(fmt.Sprintf("issue #%d blocked-by #%d projection: %v", number, dep, err))
		}
	}
}

func projectIssueDependencies(ctx context.Context, tr tracker.Tracker, issue *tracker.Issue, warn func(string)) {
	wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
	if err != nil {
		if warn != nil {
			warn(fmt.Sprintf("issue #%d dependency projection unavailable: %v", issue.Number, err))
		}
		return
	}
	ProjectDependencies(ctx, tr, issue.Number, wu.DependsOn, warn)
}

func projectChildOrder(ctx context.Context, tr tracker.Tracker, parent int, children []int, warn func(string)) {
	if err := tr.OrderSubIssues(ctx, parent, children); err != nil && warn != nil {
		warn(fmt.Sprintf("issue #%d sub-issue priority projection: %v", parent, err))
	}
}
