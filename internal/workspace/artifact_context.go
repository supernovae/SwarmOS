package workspace

import (
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"strings"
)

// WithoutUnmergedLessons removes only the lesson section and its subsections.
// A following peer or parent heading remains useful task context.
func WithoutUnmergedLessons(body string) string {
	var out []string
	skip := 0
	fenced := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
		}
		level := 0
		if !fenced {
			level = len(t) - len(strings.TrimLeft(t, "#"))
			if level > 6 || level == len(t) || (level > 0 && t[level] != ' ' && t[level] != '\t') {
				level = 0
			}
		}
		if level > 0 {
			title := strings.Trim(strings.ToLower(strings.TrimSpace(t[level:])), "# :*")
			if title == "what we learned" {
				skip = level
				continue
			}
			if skip > 0 && level <= skip {
				skip = 0
			}
		}
		if skip == 0 {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
func artifactRole(kind contract.ArtifactType) string {
	switch kind {
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
func renderArtifactContext(artifacts []contract.Artifact, role string) string {
	var b strings.Builder
	for _, a := range artifacts {
		if artifactRole(a.Type) == role {
			fmt.Fprintf(&b, "### Superseded previous attempt: %s (by `%s`, verdict: `%s`)\n\nThese are not current findings. Reassess this previous attempt against the current task.\n\n", a.Type, a.Producer, a.Verdict)
		} else {
			fmt.Fprintf(&b, "### %s (by `%s`, verdict: `%s`)\n\n", a.Type, a.Producer, a.Verdict)
		}
		b.WriteString(strings.TrimSpace(WithoutUnmergedLessons(a.Body)) + "\n\n")
	}
	return b.String()
}
