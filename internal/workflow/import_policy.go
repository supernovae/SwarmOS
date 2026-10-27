package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/validation"
)

// importRejection distinguishes a rejected snapshot from a budget interruption.
type importRejection struct {
	Paths []string
	Role  string
}

func (e *importRejection) Error() string {
	return fmt.Sprintf("rejected agent changes to: %s", strings.Join(e.Paths, ", "))
}

func writableImportPolicy(role string, paths, patterns []string, allowHarness, fork bool) error {
	rejected := map[string]bool{}
	for _, path := range paths {
		active := path == ".swarm/active" || strings.HasPrefix(path, ".swarm/active/")
		output := path == ".swarm/active/LESSONS.md" && (role == "developer" || role == "tester") || role == "developer" && path == ".swarm/active/IMPLEMENTATION.md" || role == "tester" && path == ".swarm/active/VALIDATION.md"
		if active && !output {
			rejected[path] = true
		}
		if fork && !output && (path == ".swarm" || strings.HasPrefix(path, ".swarm/") || path == ".github/workflows" || strings.HasPrefix(path, ".github/workflows/")) {
			rejected[path] = true
		}
		if !allowHarness {
			for _, pattern := range patterns {
				if validation.MatchProtectedPath(pattern, path) {
					rejected[path] = true
				}
			}
		}
	}
	if len(rejected) == 0 {
		return nil
	}
	names := make([]string, 0, len(rejected))
	for name := range rejected {
		names = append(names, name)
	}
	sort.Strings(names)
	return &importRejection{Paths: names, Role: role}
}

// developerOutputOnly reports whether a developer was refused only the
// tester-owned VALIDATION.md, which usually means results went to the wrong file.
func (e *importRejection) developerOutputOnly() bool {
	if e.Role != "developer" || len(e.Paths) == 0 {
		return false
	}
	for _, path := range e.Paths {
		if path != ".swarm/active/VALIDATION.md" {
			return false
		}
	}
	return true
}

// developerOutputInstructions is shell-safe inside single quotes.
const developerOutputInstructions = "Record notes, test results, and blocked checks in .swarm/active/IMPLEMENTATION.md. Do not change other .swarm/active/ files except LESSONS.md."

// importRejectionReason explains a rejected import and how to rerun the implement phase.
func importRejectionReason(rejection *importRejection, issue, pr int) string {
	rerun := fmt.Sprintf("swarm issue work %d --phase implement", issue)
	if pr != 0 {
		rerun = fmt.Sprintf("swarm pr work %d --phase implement", pr)
	}
	if rejection.developerOutputOnly() {
		return fmt.Sprintf("%s; returned changes were not imported; worktree was left unchanged; developers record results in .swarm/active/IMPLEMENTATION.md, not %s; rerun with %s --instructions '%s'", rejection, strings.Join(rejection.Paths, ", "), rerun, developerOutputInstructions)
	}
	return fmt.Sprintf("%s; returned changes were not imported; worktree was left unchanged; rerun with %s", rejection, rerun)
}
