package workflow

import (
	"context"
	"errors"
	"fmt"
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

// failedRun preserves the safe run error and any fencing error independently.
func (e *Engine) failedRun(ctx context.Context, st *lease.IssueState, opts WorkOptions, run *agent.RunResult, runErr error, role string) (*lease.IssueState, error) {
	var classified interface{ FailureClass() string }
	if !errors.As(runErr, &classified) || classified.FailureClass() == "" {
		return st, runErr
	}
	class := classified.FailureClass()
	if run != nil && run.Telemetry != nil {
		updated, err := e.LeaseManager.RecordFailedRoleRun(ctx, opts.IssueNumber, opts.Owner, run.Telemetry)
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("record failed role run: %w", err))
		} else {
			st = updated
		}
	}
	if cause := agent.FailureCause(class); cause != "" {
		runErr = fmt.Errorf("%w; %s; resume: %s", runErr, cause, failureResume(opts, role))
	}
	return st, runErr
}

func shellArgument(s string) string {
	return "\x27" + strings.ReplaceAll(s, "\x27", "\x27\\\x27\x27") + "\x27"
}

func failureResume(opts WorkOptions, role string) string {
	phase := "implement"
	if role == "planner" {
		phase = "plan"
	} else if role == "reviewer" {
		phase = "review"
	}
	command := fmt.Sprintf("swarm issue work %d", opts.IssueNumber)
	if opts.PRNumber != 0 {
		command = fmt.Sprintf("swarm pr work %d", opts.PRNumber)
	}
	if opts.ForkMode {
		command = strings.Replace(command, "swarm ", "swarm --fork ", 1)
	}
	command += " --phase " + phase
	for _, option := range []struct{ flag, value string }{
		{"agent", opts.AgentName}, {"planner-agent", opts.PlannerAgent}, {"reviewer-agent", opts.ReviewerAgent},
		{"developer-agent", opts.DeveloperAgent}, {"tester-agent", opts.TesterAgent}, {"fork-remote", opts.ForkRemote},
	} {
		if option.value != "" {
			command += " --" + option.flag + " " + shellArgument(option.value)
		}
	}
	return command
}

// instructionsFor keeps role guidance out of other roles' prompts and metadata.
func (o WorkOptions) instructionsFor(role string) string {
	scoped := map[string]string{"developer": o.DeveloperInstructions, "tester": o.TesterInstructions, "planner": o.PlannerInstructions, "reviewer": o.ReviewerInstructions}[role]
	parts := []string{}
	for _, text := range []string{o.Instructions, scoped} {
		if strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}
