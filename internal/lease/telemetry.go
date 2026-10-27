package lease

import (
	"fmt"
	"html"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
)

// MaxRoleRuns bounds the per-claim run history; the oldest runs are dropped.
const MaxRoleRuns = 64

// artifactRole names the only role whose run may produce each artifact type.
func artifactRole(t contract.ArtifactType) string {
	switch t {
	case contract.ArtifactDesign, contract.ArtifactTestPlan:
		return "planner"
	case contract.ArtifactReview:
		return "reviewer"
	case contract.ArtifactImplementation:
		return "developer"
	case contract.ArtifactValidation:
		return "tester"
	}
	return ""
}

// recordRoleRun runs inside updateArtifact's fenced mutation. Telemetry is
// untrusted, including fork-submitted metadata: invalid values are dropped
// without failing the artifact, valid ones are canonicalized and appended to
// the history once per run ID. It never changes the artifact body or a gate.
func recordRoleRun(st *IssueState, art *contract.Artifact) {
	value, ok := art.Metadata[contract.RoleRunTelemetryKey]
	if !ok {
		return
	}
	delete(art.Metadata, contract.RoleRunTelemetryKey)
	t, err := contract.ParseRoleRunTelemetry(value)
	if err != nil || t.Role != artifactRole(art.Type) {
		return
	}
	canonical, err := t.Encode()
	if err != nil {
		return
	}
	art.Metadata[contract.RoleRunTelemetryKey] = canonical
	for _, run := range st.RoleRuns {
		if run.RunID == t.RunID {
			return // the planner's second artifact or a redelivered update
		}
	}
	st.RoleRuns = append(st.RoleRuns, *t)
	if extra := len(st.RoleRuns) - MaxRoleRuns; extra > 0 {
		st.RoleRuns = append([]contract.RoleRunTelemetry(nil), st.RoleRuns[extra:]...)
	}
}

// NextRepairAttempt returns the coordinator's validation-repair attempt for a
// new run: 0 for initial work, one more than the previous developer run for a
// developer repair, and the current developer attempt for a tester.
func NextRepairAttempt(st *IssueState, role string, repair bool) int64 {
	var previous int64
	if st != nil {
		for i := len(st.RoleRuns) - 1; i >= 0; i-- {
			if run := st.RoleRuns[i]; run.Role == "developer" {
				if run.RepairAttempt != nil {
					previous = *run.RepairAttempt
				}
				break
			}
		}
	}
	switch {
	case role == "developer" && repair:
		return previous + 1
	case role == "tester":
		return previous
	}
	return 0
}

// RoleRuns returns the validated run history in recorded order for status
// projections. Entries that no longer validate are omitted.
func RoleRuns(st *IssueState) []contract.RoleRunTelemetry {
	if st == nil {
		return nil
	}
	runs := make([]contract.RoleRunTelemetry, 0, len(st.RoleRuns))
	for _, run := range st.RoleRuns {
		if run.Validate() == nil {
			runs = append(runs, run)
		}
	}
	return runs
}

// RoleRunTableHeader is the compact text table header shared by issue status,
// pr status and PR evidence.
const RoleRunTableHeader = "| Role | Run | Elapsed | Setup | Preflight | Snapshot | Model time | Tool time | Tokens in/out/cached | Tool calls/repeats | Repair/replan | Model/effort |"

// RoleRunTable renders runs as a Markdown table. Unobserved values read
// "unavailable"; measured zero stays 0.
func RoleRunTable(runs []contract.RoleRunTelemetry) string {
	if len(runs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(RoleRunTableHeader + "\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, r := range runs {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s/%s/%s | %s/%s | %s/%s | %s/%s |\n",
			r.Role, r.RunID[:8], ms(r.ElapsedMS), ms(r.SetupMS), ms(r.PreflightMS), ms(r.SnapshotMS), ms(r.ModelMS), ms(r.ToolMS),
			count(r.InputTokens), count(r.OutputTokens), count(r.CachedInputTokens), count(r.ToolCalls), count(r.RepeatedCommands),
			count(r.RepairAttempt), count(r.ReplanCount), label(r.ActualModel), label(r.Effort))
	}
	return b.String()
}

func ms(v *int64) string {
	if v == nil {
		return "unavailable"
	}
	if *v >= 1000 {
		return fmt.Sprintf("%.1fs", float64(*v)/1000)
	}
	return fmt.Sprintf("%dms", *v)
}

func count(v *int64) string {
	if v == nil {
		return "unavailable"
	}
	return fmt.Sprint(*v)
}

// label escapes defensively even though validation already restricts labels.
func label(v *string) string {
	if v == nil {
		return "unavailable"
	}
	return strings.ReplaceAll(html.EscapeString(*v), "|", "\\|")
}
