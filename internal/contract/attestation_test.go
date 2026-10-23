package contract

import (
	"reflect"
	"testing"
)

func TestRoleAttestationMetadataKeepsBaseAndSkipsEmptyFields(t *testing.T) {
	base := map[string]string{"kind": "agent", "model": "configured"}
	got := RoleAttestation{Role: "tester", Harness: "codex", HarnessVersion: "1.2.3", Sandbox: "bwrap"}.Metadata(base)
	want := map[string]string{"kind": "agent", "model": "configured", "role": "tester", "harness": "codex", "harness_version": "1.2.3", "sandbox": "bwrap"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata = %v, want %v", got, want)
	}
	if len(base) != 2 {
		t.Fatalf("base map was mutated: %v", base)
	}
}
