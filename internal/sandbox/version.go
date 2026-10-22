package sandbox

import (
	"fmt"
	"strconv"
	"strings"
)

// A version is an interface prerequisite, never evidence of enforcement.
// Callers must also validate the interface and exercise the execution boundary.
func harnessVersion(output, prefix, suffix string, minimum [3]int) (string, error) {
	text := strings.TrimSpace(output)
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, suffix) {
		return "", fmt.Errorf("unrecognized harness version output")
	}
	version := strings.TrimSuffix(strings.TrimPrefix(text, prefix), suffix)
	parts := strings.Split(version, ".")
	var parsed [3]int
	if len(parts) != len(parsed) {
		return "", fmt.Errorf("unsupported version %q; use a stable release", version)
	}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') || strings.Trim(part, "0123456789") != "" {
			return "", fmt.Errorf("unsupported version %q; use a stable release", version)
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return "", fmt.Errorf("invalid version %q", version)
		}
		parsed[i] = n
	}
	for i, n := range parsed {
		if n > minimum[i] {
			return version, nil
		}
		if n < minimum[i] {
			return "", fmt.Errorf("version %s requires at least %d.%d.%d", version, minimum[0], minimum[1], minimum[2])
		}
	}
	return version, nil
}
