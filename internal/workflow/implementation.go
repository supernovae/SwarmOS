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

// testerOutcome is a scheduling hint from the current tester, never validation authority.
type testerOutcome string

const (
	testerUnknown testerOutcome = ""
	testerPass    testerOutcome = "PASS"
	testerFail    testerOutcome = "FAIL"
	testerBlocked testerOutcome = "BLOCKED"
)

var testerDeclaration = regexp.MustCompile(`^VERDICT:[ \t]+(PASS|FAIL|BLOCKED)(?:$|(?:\.|:| -)[ \t]+\S.*$)`)

func parseTesterOutcome(report string) testerOutcome {
	lines := strings.Split(strings.TrimSpace(canonicalReport(report)), "\n")
	line := strings.TrimSpace(lines[0])
	if strings.HasPrefix(line, "# ") && strings.TrimSpace(strings.TrimPrefix(line, "# ")) != "" {
		lines = lines[1:]
		for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
		if len(lines) == 0 {
			return testerUnknown
		}
		line = strings.TrimSpace(lines[0])
	}
	match := testerDeclaration.FindStringSubmatch(line)
	if match == nil {
		return testerUnknown
	}
	return testerOutcome(match[1])
}
