package contract

import (
	"fmt"
	"strings"
	"unicode"
)

// ValidateBaseBranch accepts an optional short heads-ref name, never a revision.
func ValidateBaseBranch(name string) error {
	if name == "" {
		return nil
	}
	invalid := name == "HEAD" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "refs/") || strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.ContainsAny(name, "~^:?*[\\") || strings.HasSuffix(name, ".") || name == "@"
	for _, c := range name {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			invalid = true
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			invalid = true
		}
	}
	if invalid {
		return fmt.Errorf("invalid base branch %q: use a branch name", name)
	}
	return nil
}
