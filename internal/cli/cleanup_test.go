package cli

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCLIReconcileAndCleanupPreviewReadOnly(t *testing.T) {
	f, n, _ := submittedPRFixture(t, "agent")
	number := strconv.Itoa(n)
	before := f.state(t, n)
	raw := f.must(t, "issue", "cleanup", number, "--json")
	var p struct {
		Resources []struct {
			Kind     string `json:"kind"`
			Eligible bool   `json:"eligible"`
		} `json:"resources"`
	}
	if e := json.Unmarshal([]byte(raw), &p); e != nil {
		t.Fatal(e)
	}
	if len(p.Resources) != 7 {
		t.Fatalf("missing resources: %s", raw)
	}
	for _, r := range p.Resources {
		if r.Eligible {
			t.Fatal("open PR resource eligible")
		}
	}
	if after := f.state(t, n); !reflect.DeepEqual(before, after) {
		t.Fatal("preview changed state")
	}
	f.must(t, "issue", "reconcile", number, "--json")
	after := f.state(t, n)
	if after.PROutcome == nil || after.PROutcome.Status != "open" || after.HeadSHA != before.HeadSHA || !reflect.DeepEqual(after.Artifacts, before.Artifacts) || !reflect.DeepEqual(after.Attestations, before.Attestations) {
		t.Fatal("reconciliation lost submission evidence")
	}
	f.must(t, "issue", "reconcile", number)
	if again := f.state(t, n); !reflect.DeepEqual(after, again) {
		t.Fatal("repeat reconciliation changed state")
	}
}

func TestCLICleanupApplyRefusalHasJSONFailure(t *testing.T) {
	f, n, _ := submittedPRFixture(t, "agent")
	raw, err := f.run("issue", "cleanup", strconv.Itoa(n), "--apply", "--json")
	if err == nil {
		t.Fatal("open PR cleanup succeeded")
	}
	var document struct {
		Kind      string `json:"kind"`
		Status    string `json:"status"`
		Error     string `json:"error"`
		Resources []any  `json:"resources"`
	}
	// Cobra may append human-readable diagnostics; the command's first output
	// document must still contain the full preview and refusal.
	if e := json.NewDecoder(strings.NewReader(raw)).Decode(&document); e != nil {
		t.Fatalf("missing JSON failure: %s %v", raw, e)
	}
	if document.Kind != "cleanup" || document.Status != "failed" || document.Error == "" || len(document.Resources) != 7 {
		t.Fatalf("refusal not actionable: %s", raw)
	}
}
