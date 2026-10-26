package lease

import (
	"fmt"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
)

// PlanSummary projects informational context without local workspace metadata.
func PlanSummary(st *IssueState, bodies bool) string {
	if st == nil || st.RevisionPlan == nil {
		return ""
	}
	var b strings.Builder
	write := func(label, plan, test, pd, td string) {
		fmt.Fprintf(&b, "### %s\n\nPlan SHA-256: %s\nTest plan SHA-256: %s\n\n", label, pd, td)
		if bodies {
			fmt.Fprintf(&b, "%s\n\n#### Test plan\n\n%s\n\n", plan, test)
		}
	}
	p := st.RevisionPlan.Original
	write("Original approved plan", p.Plan, p.TestPlan, p.PlanDigest, p.TestPlanDigest)
	for i, p := range st.RevisionPlan.Accepted {
		write(fmt.Sprintf("Accepted revision %d", i+1), p.Plan, p.TestPlan, p.PlanDigest, p.TestPlanDigest)
	}
	var plan, test string
	for _, a := range st.Artifacts {
		switch a.Type {
		case contract.ArtifactDesign:
			plan = a.Body
		case contract.ArtifactTestPlan:
			test = a.Body
		}
	}
	if plan != "" && test != "" {
		write("Current revision plan", plan, test, hash(plan), hash(test))
	}
	return b.String()
}
