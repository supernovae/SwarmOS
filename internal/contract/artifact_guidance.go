package contract

import "strings"

// CanonicalArtifactBody excludes the coordinator's review-only guidance appendix.
func CanonicalArtifactBody(body string) string {
	if i := strings.Index(body, "\n<!-- swarm:guidance:start -->\n"); i >= 0 {
		return body[:i]
	}
	return body
}
