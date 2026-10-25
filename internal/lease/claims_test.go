package lease

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestClaimBaseHint(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	if out, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	base := "0123456789abcdef0123456789abcdef01234567"
	if err := SaveClaimToken(root, 56, "claim", base); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadClaimBaseHint(root, 56); err != nil || got != base {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := LoadClaimToken(root, 56); err != nil || got != "claim" {
		t.Fatalf("token behavior changed: %q %v", got, err)
	}
	path, err := claimControlPath(root, 56)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []persistedClaim{{IssueNumber: 56, ClaimID: "claim"}, {IssueNumber: 56, ClaimID: "claim", BaseSHA: "bad"}, {IssueNumber: 57, ClaimID: "claim", BaseSHA: base}} {
		raw, _ := json.Marshal(tc)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if hint, err := LoadClaimBaseHint(root, 56); err == nil || hint != "" {
			t.Fatalf("invalid hint accepted: %q %v", hint, err)
		}
	}
}
