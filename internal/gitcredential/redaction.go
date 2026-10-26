package gitcredential

import (
	"encoding/base64"
	"strings"
)

// Redaction removes the coordinator credential from text that leaves a
// subprocess, such as Git diagnostics or saved agent failure transcripts.
type Redaction struct {
	secrets []string
}

// NewRedaction resolves the coordinator token once and returns its redaction.
func NewRedaction() (*Redaction, error) {
	token, _, err := GitHubToken()
	if err != nil {
		return nil, err
	}
	return redactionFor(token), nil
}

func redactionFor(token string) *Redaction {
	if token == "" {
		return &Redaction{}
	}
	return &Redaction{secrets: []string{token, basicAuth(token)}}
}

func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// Redact replaces every credential representation with a fixed marker.
func (r *Redaction) Redact(output string) string {
	for _, secret := range r.secrets {
		output = strings.ReplaceAll(output, secret, "[REDACTED]")
	}
	return output
}

// MaxLen is the longest redacted representation. Callers that cut text before
// redacting discard this many leading bytes so no partial credential survives.
func (r *Redaction) MaxLen() int {
	longest := 0
	for _, secret := range r.secrets {
		longest = max(longest, len(secret))
	}
	return longest
}
