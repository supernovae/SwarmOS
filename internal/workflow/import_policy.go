package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/validation"
)

// importRejection distinguishes a rejected snapshot from a budget interruption.
type importRejection struct{ Paths []string }

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
	return &importRejection{Paths: names}
}
