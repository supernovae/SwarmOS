package gitcredential

import (
	"strings"
	"testing"
)

func TestRedactionMatchesGitDiagnostics(t *testing.T) {
	const token = "ghp_test_diagnostics_secret"
	t.Setenv("SWARM_GITHUB_TOKEN", token)
	t.Setenv(TokenCommandEnv, "")
	resetTokenCache()
	t.Cleanup(resetTokenCache)
	redaction, err := NewRedaction()
	if err != nil {
		t.Fatal(err)
	}
	basic := basicAuth(token)
	got := redaction.Redact("a " + token + " b " + basic)
	if got != "a [REDACTED] b [REDACTED]" {
		t.Fatalf("redacted = %q", got)
	}
	if redaction.MaxLen() != len(basic) {
		t.Fatalf("MaxLen = %d, want %d", redaction.MaxLen(), len(basic))
	}
	if empty := redactionFor(""); empty.Redact("plain") != "plain" || empty.MaxLen() != 0 {
		t.Fatal("empty token changed output")
	}
}

func TestRedactionFailsClosedOnTokenError(t *testing.T) {
	t.Setenv("SWARM_GITHUB_TOKEN", "ghp_conflicting_secret")
	t.Setenv(TokenCommandEnv, "printf two")
	resetTokenCache()
	t.Cleanup(resetTokenCache)
	if redaction, err := NewRedaction(); err == nil || redaction != nil || strings.Contains(err.Error(), "ghp_conflicting_secret") {
		t.Fatalf("NewRedaction = %v, %v", redaction, err)
	}
}
