package workflow

import (
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/safefs"
)

const guidanceStart = "\n<!-- swarm:guidance:start -->\n"

// canonicalReport excludes coordinator appendices from plan comparisons.
func canonicalReport(body string) string { return contract.CanonicalArtifactBody(body) }
func guidedReport(body string, run *agent.RunResult) string {
	if run == nil || len(run.Guidance) == 0 {
		return body
	}
	body = canonicalReport(body) + guidanceStart + "## Operator guidance\n\n"
	for _, message := range run.Guidance {
		body += "> " + strings.ReplaceAll(message, "\n", "\n> ") + "\n\n"
	}
	return body + "<!-- swarm:guidance:end -->\n"
}
func recordGuidance(wtPath, name, body string, run *agent.RunResult) (string, error) {
	body = guidedReport(body, run)
	safe, err := safefs.Open(wtPath)
	if err != nil {
		return "", err
	}
	defer safe.Close()
	if err := safe.WriteFile(filepath.Join(".swarm", "active", name), []byte(body), 0644); err != nil {
		return "", err
	}
	return body, nil
}
func runMetadata(run *agent.RunResult, metadata map[string]string) map[string]string {
	if run == nil {
		return metadata
	}
	return telemetryMetadata(run, agent.GuidanceMetadata(run.Guidance, run.Attestation.Metadata(metadata)))
}

// observeRun records the coordinator-selected attempt counters on a run's
// telemetry: re-plans the claim had recorded when the run started, and the
// validation-repair attempt it belongs to (initial work is 0).
func observeRun(run *agent.RunResult, st *lease.IssueState, repair bool) {
	if run == nil || run.Telemetry == nil {
		return
	}
	var replan int64
	if st != nil {
		replan = int64(st.PlanReplanCount)
	}
	run.Telemetry.ReplanCount = contract.Count(replan)
	run.Telemetry.RepairAttempt = contract.Count(lease.NextRepairAttempt(st, run.Telemetry.Role, repair))
}

// telemetryMetadata adds the run's encoded telemetry. Invalid telemetry is
// omitted rather than failing the artifact.
func telemetryMetadata(run *agent.RunResult, metadata map[string]string) map[string]string {
	if run == nil || run.Telemetry == nil {
		return metadata
	}
	if value, err := run.Telemetry.Encode(); err == nil {
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadata[contract.RoleRunTelemetryKey] = value
	}
	return metadata
}
