package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

// Explicit outcome lines only: narrative, quotations and resolved blockers do
// not declare a blocked outcome. The bounded read rejects links/special files.
var blockedOutcome = regexp.MustCompile(`(?i)^(?:#{1,6}\s+)?(?:\*\*)?(?:(?:status|verdict|outcome)\s*:\s*)?blocked(?: before implementation)?(?:\*\*)?\s*(?::|-|\.|$)`)

func implementationReport(root string) (string, string) {
	data, err := safefs.ReadFile(root, filepath.Join(".swarm", "active", "IMPLEMENTATION.md"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return "", ""
	}
	if err != nil {
		return "", fmt.Sprintf("implementation report unavailable: %s", strings.ReplaceAll(err.Error(), root, "<workspace>"))
	}
	return string(data), ""
}
func blockingReason(report string) string {
	lines := strings.Split(canonicalReport(report), "\n")
	fenced := false
	for i, line := range lines {
		line = strings.ReplaceAll(strings.TrimSpace(line), "**", "")
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if !blockedOutcome.MatchString(line) {
			continue
		}
		reason := strings.TrimSpace(blockedOutcome.ReplaceAllString(line, ""))
		for _, next := range lines[i+1:] {
			if strings.HasPrefix(strings.TrimSpace(next), "#") {
				break
			}
			reason += "\n" + next
		}
		reason = strings.TrimSpace(reason)
		if reason == "" {
			reason = "Blocked before implementation"
		}
		return reason
	}
	return ""
}
