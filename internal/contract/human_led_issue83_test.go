package contract

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"reflect"
	"strings"
	"testing"
)

func TestIssue83OptionalWireCompatibility(t *testing.T) {
	legacy := `{"role":"planner","agent":"codex","harness":"codex","harness_version":"1","model":"default","sandbox":"host"}`
	var att RoleAttestation
	if err := json.Unmarshal([]byte(legacy), &att); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(att)
	if err != nil || string(raw) != legacy {
		t.Fatalf("legacy wire changed: %s %v", raw, err)
	}
	oldYAML, err := yaml.Marshal(att)
	if err != nil {
		t.Fatal(err)
	}
	expectedYAML := "role: planner\nagent: codex\nharness: codex\nharness_version: \"1\"\nmodel: default\nsandbox: host\n"
	if string(oldYAML) != expectedYAML {
		t.Fatalf("legacy YAML changed: %s", oldYAML)
	}
	for _, key := range []string{"production_mode", "staged_by", "harness_source", "model_source"} {
		if strings.Contains(string(oldYAML), key) {
			t.Fatal(string(oldYAML))
		}
	}
	att.ProductionMode, att.StagedBy, att.HarnessSource, att.ModelSource = "interactive", "authenticated-login", "self-reported", "self-reported"
	for _, codec := range []struct {
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{{json.Marshal, json.Unmarshal}, {yaml.Marshal, yaml.Unmarshal}} {
		raw, err := codec.marshal(att)
		if err != nil {
			t.Fatal(err)
		}
		var decoded RoleAttestation
		if err := codec.unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(att, decoded) {
			t.Fatalf("roundtrip: %+v %v", decoded, err)
		}
	}
	base := map[string]string{"task_digest": "task", "policy_digest": "policy", "plan_digest": "plan"}
	got := att.Metadata(base)
	if got["staged_by"] != att.StagedBy || got["harness_source"] != "self-reported" || len(base) != 3 {
		t.Fatal(got, base)
	}
	for k, v := range base {
		if got[k] != v {
			t.Fatal("binding changed", k)
		}
	}
}
