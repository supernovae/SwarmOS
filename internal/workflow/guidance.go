package workflow

import (
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
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
	return agent.GuidanceMetadata(run.Guidance, run.Attestation.Metadata(metadata))
}
